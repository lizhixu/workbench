package apps

// 应用挂载卷备份与恢复（2026-10-22）。
//
// 每个应用声明的卷（Application.Volumes，docker -v 规格）是业务数据的落点，
// 平台此前只有删除应用时才接触它们（purge 时区分 bind mount 与具名卷），既不能
// 备份也不能找回。本文件补齐卷数据生命周期：把卷的数据目录打成 tar.gz，直传
// S3 默认存储目标（与快照引擎同一 direct-to-storage 原则——GB 级归档过控制端会
// 打满上行带宽），元数据落控制端 dataDir 下的 app_volume_archives.json，供详情页
// 列举、下载、恢复与删除。被管主机只承担 tar / curl 的临时暂存，归档本身不落在
// 主机磁盘上（恢复时的安全副本除外）。
//
// 卷规格两种形态都支持：
//   - bind mount：/opt/apps/xxx/data:/data —— 直接 tar 宿主机路径；
//   - Docker 具名卷：volname:/data —— 先 docker volume inspect 解析挂载点再 tar
//     （与 purge.go「src 含 / 即宿主机目录，绝不误删」的区分规则同源）。
//
// 恢复遵循安全停机语义：先把归档从 S3 下载到主机临时路径并校验 SHA256，再
// docker stop 应用容器防止在线写盘与文件锁，安全副本后解压覆盖，出错路径由
// defer 统一把容器拉回运行态（控制端进程在停机与重启之间崩溃时容器会留在停止态，
// 需人工启动，见 AGENTS.md §3.7）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"watchman/server/internal/rpc"
	"watchman/server/internal/snapshots"
)

// errVolumeOpInProgress rejects a deployment while a volume backup/restore is
// running; the reverse direction uses "应用正在部署" errors from the engine.
var errVolumeOpInProgress = errors.New("该应用正在执行数据卷备份或恢复，请稍后再试")

// errVolumeS3TargetMissing means the recorded S3 destination no longer exists
// (deleted in the snapshot module). The object is unreachable, so retention /
// delete drop the metadata instead of retrying forever.
var errVolumeS3TargetMissing = errors.New("归档的 S3 存储目标已不存在")

// NOTE: an earlier revision had PurgeVolumeArchives acquire the volume slot
// itself and return a busy sentinel, but its only caller (the purge flow)
// already holds that slot for the whole purge — the self-acquire could
// never succeed, so archive cleanup was silently skipped on every purge.
// The slot discipline now lives with the caller: deleteApp rejects when
// VolumeBusy, and purgeAppResources acquires the slot at entry and holds it
// for the whole purge, so no backup/restore can interleave with the archive
// cleanup below.

const (
	// volumeStagingRoot is scratch space on the managed host: tar output before
	// the S3 upload, archives downloaded for a restore, and the pre-restore
	// safety copy. Archives themselves live in S3, not here.
	volumeStagingRoot = "/var/backups/watchman/apps"

	// volumeRetention keeps the newest N successful archives per volume spec
	// (S3 objects plus their metadata). Mirrors the snapshot engine's default
	// so the two backup surfaces behave alike.
	volumeRetention = 5

	// noPruneVolumes disables the post-backup prune. A manual backup is a
	// deliberate safety copy and must never evict older archives on its own —
	// retention belongs to the scheduled plans (snapshot jobs), so only they
	// pass a positive keep.
	noPruneVolumes = -1

	// volumeArchiveMetaCap bounds stored metadata per application; retention
	// pruning keeps this far below the cap in normal operation.
	volumeArchiveMetaCap = 100

	// appVolumeArchivesFile persists archive metadata under the data dir. A
	// full-file atomic rewrite (not append-only JSONL) because a record
	// migrates running -> success/failed after its agent run returns.
	appVolumeArchivesFile = "app_volume_archives.json"

	// Archive lifecycle states. Running is visible to the UI (polled while the
	// agent tar + upload runs); failed keeps its record so the operator can see
	// why.
	VolumeBackupRunning = "running"
	VolumeBackupSuccess = "success"
	VolumeBackupFailed  = "failed"
	// VolumeBackupUploaded marks a record created from a file the operator
	// uploaded for a restore (not from a panel backup). It is never a
	// downloadable/restorable S3 archive: it has no StoragePath, its restore
	// runs from the panel's temporary upload, and its Status stays "uploaded"
	// for life — the restore outcome lives only in LastRestore* (the normal
	// restore/download handlers gate on Status == success, so they reject it
	// without any special case).
	VolumeBackupUploaded = "uploaded"

	// VolumeArchiveSourceUpload marks a record created by an upload-restore
	// (Source field). Empty means a panel backup, the historical default.
	VolumeArchiveSourceUpload = "upload"

	// Restore outcome recorded on the archive itself: a restore is an async
	// agent run like a backup, so without this the operator only saw a 202 and
	// had no way to learn the result from the panel.
	VolumeRestoreSuccess = "success"
	VolumeRestoreFailed  = "failed"

	// volumeS3OpTimeout bounds the tar/upload/download agent scripts. Large
	// volumes can take a while; the agent exec timeout adds its own margin.
	volumeS3OpTimeout = int32(1800)

	// Consistency mode options.
	ConsistencyModeOnline = "online" // default online hot backup
	ConsistencyModePause  = "pause"  // pause container during tar
	ConsistencyModeStop   = "stop"   // stop container during tar

	// StorageTargetLocal marks host-local storage under /var/backups/watchman/apps/<appID>.
	StorageTargetLocal = "local"
)

// VolumeItemArchive records one volume entry within a multi-volume app snapshot.
type VolumeItemArchive struct {
	VolumeSpec    string `json:"volume_spec"`       // e.g. /host:/container[:ro] or named:/container
	VolumeType    string `json:"volume_type"`       // "bind" | "volume"
	HostPath      string `json:"host_path"`         // resolved host dir or volume mountpoint
	ContainerPath string `json:"container_path"`    // path inside the container
	Service       string `json:"service,omitempty"` // compose service name when applicable
	Size          int64  `json:"size,omitempty"`    // extracted volume size in bytes
	SHA256        string `json:"sha256,omitempty"`
}

// VolumeArchive is one tar.gz backup of an application volume or entire app snapshot.
// The object is stored in S3 or host-local storage; ID carries the "vbk_" prefix.
type VolumeArchive struct {
	ID            string `json:"id"`
	AppID         string `json:"app_id"`
	HostID        string `json:"host_id"`
	VolumeSpec    string `json:"volume_spec"`    // original "host:container[:ro]" spec, or "ALL" for multi-volume snapshot
	HostPath      string `json:"host_path"`      // resolved data dir (bind path or volume mountpoint)
	ContainerPath string `json:"container_path"` // path inside the container
	// Volumes contains the detailed volume items in this archive (supports multi-volume snapshots).
	Volumes []VolumeItemArchive `json:"volumes,omitempty"`
	// ConsistencyMode records the mode used ("online" | "pause" | "stop").
	ConsistencyMode string `json:"consistency_mode,omitempty"`
	// CleanRestore records whether this archive defaults or was restored with clean wipe.
	CleanRestore bool `json:"clean_restore,omitempty"`
	// StorageTarget/StorageTargetName are frozen at backup time: switching the
	// default target later must not orphan older archives.
	// Can be "local" (host-local storage) or an S3 target ID.
	StorageTarget     string `json:"storage_target"`
	StorageTargetName string `json:"storage_target_name,omitempty"`
	StoragePath       string `json:"storage_path"` // full S3 object key or host-local path
	Size              int64  `json:"size"`         // bytes
	SHA256            string `json:"sha256,omitempty"`
	Status            string `json:"status"` // running | success | failed | uploaded
	// Source distinguishes panel backups ("", historical) from records
	// created by an upload-restore ("upload"): the latter have no S3 object
	// and their bytes live only as a temporary panel upload until restored.
	Source     string     `json:"source,omitempty"`
	Error      string     `json:"error,omitempty"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	// Restore outcome lives on the archive so the panel can show whether the
	// last restore from this archive succeeded without a separate history.
	LastRestoreAt     *time.Time `json:"last_restore_at,omitempty"`
	LastRestoreStatus string     `json:"last_restore_status,omitempty"` // success | failed
	LastRestoreError  string     `json:"last_restore_error,omitempty"`
}

// S3TargetStore resolves configured S3 destinations (implemented by
// snapshots.Store). A narrow interface keeps the engine testable and makes the
// dependency explicit.
type S3TargetStore interface {
	GetDefaultS3Target() (*snapshots.S3Target, bool)
	GetS3Target(id string) (*snapshots.S3Target, bool)
}

// SnapshotJobStore drops the scheduled app_volume backup jobs that point at an
// application being deleted (implemented by snapshots.Store). Without it the
// cron sweep keeps firing jobs whose application is gone, writing a failing
// archive record every run.
type SnapshotJobStore interface {
	DeleteJobsByApp(appID string) (int, error)
}

// S3DestinationStatus is the read-only S3 readiness block the UI uses to show
// where archives go and to gate the backup button.
type S3DestinationStatus struct {
	Ready      bool   `json:"ready"`
	TargetID   string `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
}

// appVolumeArchives is the on-disk shape of app_volume_archives.json.
type appVolumeArchives struct {
	Archives []*VolumeArchive `json:"archives"`
}

// VolumeStore persists volume archive metadata, following the same
// in-memory + atomic-file pattern as the other watchman stores.
type VolumeStore struct {
	mu   sync.RWMutex
	dir  string
	log  *slog.Logger
	byID map[string]*VolumeArchive // appID|archiveID -> record
}

// NewVolumeStore loads or initializes the archive store under dataDir.
func NewVolumeStore(dataDir string, log *slog.Logger) (*VolumeStore, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &VolumeStore{dir: dataDir, log: log, byID: make(map[string]*VolumeArchive)}
	b, err := os.ReadFile(filepath.Join(dataDir, appVolumeArchivesFile))
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	var doc appVolumeArchives
	if err := json.Unmarshal(b, &doc); err != nil {
		// Corrupt metadata must not block booting: start empty, the S3 objects
		// remain discoverable through the bucket's own listing.
		s.log.Warn("app_volume_archives.json corrupted, starting empty", "err", err)
		return s, nil
	}
	// A "running" record cannot survive a control-plane restart: the agent run
	// belonged to the old process and will never report back. Leaving it
	// running would make the UI poll forever and keep the archive unrestorable.
	stale := false
	for _, a := range doc.Archives {
		if a == nil || a.ID == "" {
			continue
		}
		if a.Status == VolumeBackupRunning {
			a.Status = VolumeBackupFailed
			a.Error = "控制端重启，任务已中断"
			stale = true
		}
		if a.Source == VolumeArchiveSourceUpload && a.Status == VolumeBackupUploaded && a.LastRestoreStatus == "" {
			now := time.Now()
			a.LastRestoreAt = &now
			a.LastRestoreStatus = VolumeBackupFailed
			a.LastRestoreError = "控制端重启，上传恢复已中断"
			stale = true
		}
		// Backward compatibility: materialize legacy single-volume records into the multi-volume slice
		if len(a.Volumes) == 0 && (a.VolumeSpec != "" || a.HostPath != "") {
			vType := "bind"
			if !strings.Contains(a.VolumeSpec, "/") && strings.Contains(a.VolumeSpec, ":") {
				vType = "volume"
			}
			a.Volumes = []VolumeItemArchive{
				{
					VolumeSpec:    a.VolumeSpec,
					VolumeType:    vType,
					HostPath:      a.HostPath,
					ContainerPath: a.ContainerPath,
					Size:          a.Size,
					SHA256:        a.SHA256,
				},
			}
		}
		s.byID[volumeArchiveKey(a.AppID, a.ID)] = a
	}
	if stale {
		if err := s.saveLocked(); err != nil {
			s.log.Warn("reconcile interrupted volume archives failed", "err", err)
		}
	}
	return s, nil
}

func volumeArchiveKey(appID, id string) string { return appID + "|" + id }

// cloneArchive detaches a record from the store's map. Records are handed to
// the HTTP layer (JSON marshaling on another goroutine) and to the engine
// (which mutates status/size in place); shared pointers would let those race.
func cloneArchive(a *VolumeArchive) *VolumeArchive {
	if a == nil {
		return nil
	}
	c := *a
	if a.LastRestoreAt != nil {
		t := *a.LastRestoreAt
		c.LastRestoreAt = &t
	}
	if len(a.Volumes) > 0 {
		c.Volumes = append([]VolumeItemArchive(nil), a.Volumes...)
	}
	return &c
}

func cloneArchivesMap(m map[string]*VolumeArchive) map[string]*VolumeArchive {
	out := make(map[string]*VolumeArchive, len(m))
	for k, v := range m {
		out[k] = cloneArchive(v)
	}
	return out
}

// AddArchive records a freshly created archive (status running).
func (s *VolumeStore) AddArchive(a *VolumeArchive) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneArchivesMap(s.byID)
	candidate[volumeArchiveKey(a.AppID, a.ID)] = cloneArchive(a)
	if err := s.saveCandidateLocked(candidate); err != nil {
		return err
	}
	s.byID = candidate
	return nil
}

// PutArchive updates a stored record (status migration, size/sha, error).
func (s *VolumeStore) PutArchive(a *VolumeArchive) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneArchivesMap(s.byID)
	candidate[volumeArchiveKey(a.AppID, a.ID)] = cloneArchive(a)
	if err := s.saveCandidateLocked(candidate); err != nil {
		return err
	}
	s.byID = candidate
	return nil
}

// GetArchive returns one archive of an application.
func (s *VolumeStore) GetArchive(appID, id string) (*VolumeArchive, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byID[volumeArchiveKey(appID, id)]
	if !ok {
		return nil, false
	}
	return cloneArchive(a), true
}

// ListArchives returns an application's archives, newest first.
func (s *VolumeStore) ListArchives(appID string) []*VolumeArchive {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*VolumeArchive, 0, 8)
	for _, a := range s.byID {
		if a.AppID == appID {
			out = append(out, cloneArchive(a))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// DeleteArchive drops one archive record.
func (s *VolumeStore) DeleteArchive(appID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneArchivesMap(s.byID)
	delete(candidate, volumeArchiveKey(appID, id))
	if err := s.saveCandidateLocked(candidate); err != nil {
		return err
	}
	s.byID = candidate
	return nil
}

// DeleteAppArchives drops every archive record of an application (purge).
func (s *VolumeStore) DeleteAppArchives(appID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneArchivesMap(s.byID)
	for k, a := range candidate {
		if a.AppID == appID {
			delete(candidate, k)
		}
	}
	if err := s.saveCandidateLocked(candidate); err != nil {
		return err
	}
	s.byID = candidate
	return nil
}

func (s *VolumeStore) saveCandidateLocked(candidate map[string]*VolumeArchive) error {
	doc := appVolumeArchives{Archives: make([]*VolumeArchive, 0, len(candidate))}
	for _, a := range candidate {
		doc.Archives = append(doc.Archives, a)
	}
	sort.Slice(doc.Archives, func(i, j int) bool { return doc.Archives[i].CreatedAt.After(doc.Archives[j].CreatedAt) })
	// Metadata cap per app: drop the oldest overflow so a long-lived app cannot
	// grow the file without bound (S3 objects are pruned separately).
	perApp := make(map[string]int)
	kept := make([]*VolumeArchive, 0, len(doc.Archives))
	for _, a := range doc.Archives {
		perApp[a.AppID]++
		if perApp[a.AppID] > volumeArchiveMetaCap {
			// Drop it from candidate map too, so memory matches the saved file.
			delete(candidate, volumeArchiveKey(a.AppID, a.ID))
			continue
		}
		kept = append(kept, a)
	}
	doc.Archives = kept
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, appVolumeArchivesFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, appVolumeArchivesFile))
}

func (s *VolumeStore) saveLocked() error {
	return s.saveCandidateLocked(s.byID)
}

// parseVolumeSpec splits a docker -v spec into source / container path / mode.
// Both forms are accepted:
//
//	/opt/apps/xxx/data:/data        bind mount
//	volname:/data:ro                named volume, read-only
//	C:\data:/data                   Windows host path (kept intact)
//
// Everything before the last container segment rejoins with ':' so a drive
// letter does not get mistaken for a separator. ok is false for a spec without
// an absolute container path, a traversing source, or a bogus mode segment.
func parseVolumeSpec(spec string) (src, dst, mode string, ok bool) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", "", "", false
	}
	parts := strings.Split(s, ":")
	if n := len(parts); n >= 3 && (parts[n-1] == "ro" || parts[n-1] == "rw") {
		mode = parts[n-1]
		parts = parts[:n-1]
	}
	if len(parts) < 2 {
		return "", "", "", false
	}
	dst = strings.TrimSpace(parts[len(parts)-1])
	src = strings.TrimSpace(strings.Join(parts[:len(parts)-1], ":"))
	if src == "" || dst == "" {
		return "", "", "", false
	}
	// Docker requires an absolute container path; anything else (including a
	// third segment that is not ro/rw) is a malformed spec, not a mount.
	if !strings.HasPrefix(dst, "/") {
		return "", "", "", false
	}
	// Same traversal guard as the snapshot engine's dir jobs.
	if strings.Contains(src, "..") || strings.Contains(dst, "..") {
		return "", "", "", false
	}
	return src, dst, mode, true
}

// volumeIsBindMount reports whether a volume source is a host directory path
// (as opposed to a Docker named volume).
func volumeIsBindMount(src string) bool {
	if strings.HasPrefix(src, "/") {
		return true
	}
	// Windows drive path ("C:\data" / "C:/data"): a third character of two with
	// a colon in front is not a valid Docker volume name anyway.
	if len(src) >= 3 && src[1] == ':' && (src[2] == '/' || src[2] == '\\') {
		return true
	}
	return false
}

// ---- host paths ----

func volumeStagingDir(appID string) string {
	return path.Join(volumeStagingRoot, appID)
}

// volumeStagingPath is where the tar output waits for its S3 upload.
func volumeStagingPath(appID, archiveID string) string {
	return path.Join(volumeStagingDir(appID), archiveID+".tar.gz")
}

// volumeRestorePath is where an archive is downloaded before unpacking.
func volumeRestorePath(appID, archiveID string) string {
	return path.Join(volumeStagingDir(appID), archiveID+".restore.tar.gz")
}

// volumeSafetyPath keeps the pre-restore state of the volume. It is a local
// recovery aid, not an archive: it is removed together with its archive.
func volumeSafetyPath(appID, archiveID string) string {
	return path.Join(volumeStagingDir(appID), archiveID+".pre-restore.tar.gz")
}

// volumeObjectKey computes the S3 key for an archive, target prefix included.
func volumeObjectKey(target *snapshots.S3Target, appID, archiveID string) string {
	return snapshots.NewS3Client(target).ObjectKey(path.Join("apps", appID, archiveID+".tar.gz"))
}

// ---- agent-side scripts ----

// volumeResolveMountpointScript prints a Docker volume's host mountpoint, then
// fails (set -e) when the volume does not exist.
func volumeResolveMountpointScript(name string) string {
	return fmt.Sprintf(`set -e
docker volume inspect --format '{{.Mountpoint}}' %s`, shellQuote(name))
}

// SnapshotManifest represents the manifest embedded in multi-volume application snapshots.
type SnapshotManifest struct {
	Version   string              `json:"version"`
	AppID     string              `json:"app_id"`
	ArchiveID string              `json:"archive_id"`
	CreatedAt time.Time           `json:"created_at"`
	Volumes   []VolumeItemArchive `json:"volumes"`
}

// volumeLocalArchivePath is the persistent location on the managed host for local storage.
func volumeLocalArchivePath(appID, archiveID string) string {
	return path.Join(volumeStagingDir(appID), archiveID+".tar.gz")
}

// volumeMultiBackupScript packages one or more volumes with manifest.json and symlinks.
func volumeMultiBackupScript(pkgDir, archivePath, manifestJSON string, items []VolumeItemArchive) string {
	var sb strings.Builder
	sb.WriteString("set -e\n")
	sb.WriteString(fmt.Sprintf("mkdir -p %s/volumes\n", shellQuote(pkgDir)))
	sb.WriteString(fmt.Sprintf("mkdir -p %s\n", shellQuote(path.Dir(archivePath))))
	b64 := base64.StdEncoding.EncodeToString([]byte(manifestJSON))
	sb.WriteString(fmt.Sprintf("echo %s | base64 -d > %s/manifest.json\n", shellQuote(b64), shellQuote(pkgDir)))
	for i, it := range items {
		volSubdir := fmt.Sprintf("%s/volumes/vol_%d", pkgDir, i)
		sb.WriteString(fmt.Sprintf("ln -s %s %s\n", shellQuote(it.HostPath), shellQuote(volSubdir)))
	}
	sb.WriteString(fmt.Sprintf("tar -C %s -czhf %s manifest.json volumes\n", shellQuote(pkgDir), shellQuote(archivePath)))
	sb.WriteString(fmt.Sprintf("rm -rf %s\n", shellQuote(pkgDir)))
	sb.WriteString(fmt.Sprintf(`SIZE=$(stat -c %%s %s 2>/dev/null || stat -f %%z %s)
SHA=$(sha256sum %s | awk '{print $1}')
echo "WATCHMAN_SIZE=$SIZE"
echo "WATCHMAN_SHA256=$SHA"`, shellQuote(archivePath), shellQuote(archivePath), shellQuote(archivePath)))
	return sb.String()
}

// RestoreTarget designates a volume from an archive to be unpacked into a specific host directory.
type RestoreTarget struct {
	Index   int    `json:"index"`
	DestDir string `json:"dest_dir"`
	Spec    string `json:"spec"`
}

// volumeMultiRestoreScript generates the agent shell script to unpack one or more volumes
// from an archive, supporting clean directory wipe and safety copy preservation.
// Every tar invocation relies on GNU tar's compression auto-detection (-tf/-xf
// instead of -tzf/-xzf): system snapshots are gzip, but an operator-uploaded
// archive may be a plain uncompressed .tar (e.g. piped straight out of
// `docker cp`); both must restore without ceremony.
func volumeMultiRestoreScript(archivePath, safetyDir string, cleanRestore bool, targets []RestoreTarget) string {
	var sb strings.Builder
	sb.WriteString("set -e\n")
	sb.WriteString(fmt.Sprintf("[ -f %s ] || { echo %s >&2; exit 1; }\n",
		shellQuote(archivePath), shellQuote("归档文件不存在: "+archivePath)))
	sb.WriteString(fmt.Sprintf("mkdir -p %s\n", shellQuote(safetyDir)))
	sb.WriteString(fmt.Sprintf(`IS_SNAPSHOT=0
if tar -tf %s manifest.json >/dev/null 2>&1; then
  IS_SNAPSHOT=1
fi
`, shellQuote(archivePath)))

	for _, t := range targets {
		destDirQ := shellQuote(t.DestDir)
		safetyFileQ := shellQuote(path.Join(safetyDir, fmt.Sprintf("pre-restore-%d.tar.gz", t.Index)))
		sb.WriteString(fmt.Sprintf("\n# Restore target: %s (index %d)\n", t.Spec, t.Index))
		sb.WriteString(fmt.Sprintf("mkdir -p %s\n", destDirQ))
		// Safety copy of current contents before overwrite
		sb.WriteString(fmt.Sprintf("tar -C %s -czf %s . 2>/dev/null || true\n", destDirQ, safetyFileQ))
		if cleanRestore {
			// Wipe child entries while keeping root directory and its inode intact
			sb.WriteString(fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || rm -rf %s/* %s/.[!.]* 2>/dev/null || true\n",
				destDirQ, destDirQ, destDirQ))
		}
		volPattern := fmt.Sprintf("volumes/vol_%d", t.Index)
		sb.WriteString(fmt.Sprintf(`if [ "$IS_SNAPSHOT" -eq 1 ]; then
  tar -C %s -xf %s --strip-components=2 %s
else
  tar -C %s -xf %s
fi
`, destDirQ, shellQuote(archivePath), shellQuote(volPattern), destDirQ, shellQuote(archivePath)))
	}
	return sb.String()
}

// volumeBackupScript packs the volume's data dir into one tar.gz in the host's
// staging area, then prints the size/sha markers the engine parses (same
// contract as the snapshot engine's buildScript).
func volumeBackupScript(hostDir, archivePath string) string {
	return fmt.Sprintf(`set -e
mkdir -p %s
tar -C %s -czf %s .
SIZE=$(stat -c %%s %s 2>/dev/null || stat -f %%z %s)
SHA=$(sha256sum %s | awk '{print $1}')
echo "WATCHMAN_SIZE=$SIZE"
echo "WATCHMAN_SHA256=$SHA"`,
		shellQuote(path.Dir(archivePath)), shellQuote(hostDir), shellQuote(archivePath),
		shellQuote(archivePath), shellQuote(archivePath), shellQuote(archivePath))
}

// volumeUploadScript streams the staged archive to a pre-signed S3 PUT URL.
// Direct-to-storage: the bytes never traverse the control plane.
func volumeUploadScript(localPath, putURL string) string {
	return fmt.Sprintf("curl -fsSL --connect-timeout 15 -X PUT -T %s %s",
		shellQuote(localPath), shellQuote(putURL))
}

// volumeDownloadScript fetches an archive from a pre-signed S3 GET URL into
// the host's staging area and rejects an empty body.
func volumeDownloadScript(getURL, localPath string) string {
	return fmt.Sprintf(`set -e
mkdir -p %s
curl -fsSL --connect-timeout 15 -o %s %s
[ -s %s ] || { echo "归档下载为空" >&2; exit 1; }`,
		shellQuote(path.Dir(localPath)), shellQuote(localPath), shellQuote(getURL), shellQuote(localPath))
}

// volumeSHA256Script prints the downloaded archive's digest for verification.
func volumeSHA256Script(localPath string) string {
	return fmt.Sprintf("sha256sum %s | awk '{print $1}'", shellQuote(localPath))
}

// volumeTarProbeScript lists a tar archive without extracting it: an upload with
// no operator-supplied SHA is still rejected before any downtime when it is
// not a tar at all. GNU tar auto-detects the compression, so plain .tar and
// .tar.gz/.tgz both pass — an archive copied straight out of `docker cp`
// needs no re-packing.
func volumeTarProbeScript(localPath string) string {
	return fmt.Sprintf("tar -tf %s >/dev/null", shellQuote(localPath))
}

// volumeContainerStateScript reports a container state and fails closed when
// docker inspect cannot determine it. Treating inspect failures as STOPPED is
// unsafe: restore would overwrite a live container's data without arranging a
// restart.
func volumeContainerStateScript(container string) string {
	return fmt.Sprintf(`set -e
STATE=$(docker inspect --format '{{.State.Running}}' %s)
case "$STATE" in
  true) echo RUNNING ;;
  false) echo STOPPED ;;
  *) echo "无法确定容器运行状态" >&2; exit 1 ;;
esac`, shellQuote(container))
}

func parseVolumeContainerState(stdout []byte) (running bool, err error) {
	switch strings.TrimSpace(strings.ToUpper(string(stdout))) {
	case "RUNNING":
		return true, nil
	case "STOPPED":
		return false, nil
	default:
		return false, fmt.Errorf("Agent 返回了无法识别的容器状态")
	}
}

// volumeRemoveScript deletes staging files from the host (best effort).
func volumeRemoveScript(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		quoted = append(quoted, shellQuote(p))
	}
	if len(quoted) == 0 {
		return "true"
	}
	return "rm -f " + strings.Join(quoted, " ")
}

// parseVolumeArchiveMeta extracts the WATCHMAN_SIZE / WATCHMAN_SHA256 markers.
func parseVolumeArchiveMeta(stdout []byte) (size int64, sha string) {
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "WATCHMAN_SIZE="):
			if v, err := strconv.ParseInt(strings.TrimPrefix(line, "WATCHMAN_SIZE="), 10, 64); err == nil {
				size = v
			}
		case strings.HasPrefix(line, "WATCHMAN_SHA256="):
			sha = strings.TrimSpace(strings.TrimPrefix(line, "WATCHMAN_SHA256="))
		}
	}
	return
}

// validateVolumeArchiveMeta rejects a backup whose agent markers are missing
// or implausible. The markers are the archive's only integrity proof: without
// a SHA the restore path cannot verify the downloaded bytes, so such an
// archive must fail here rather than be recorded as a usable success.
func validateVolumeArchiveMeta(size int64, sha string) error {
	if size <= 0 || sha == "" {
		return fmt.Errorf("备份产物标记缺失（未能读取归档大小与 SHA256），归档不可信")
	}
	return nil
}

// ---- S3 ----

// SetS3TargetStore injects the S3 destination store (snapshots.Store).
func (e *Engine) SetS3TargetStore(s S3TargetStore) {
	e.s3 = s
}

// SetSnapshotJobStore injects the snapshot job cleanup hook (snapshots.Store).
func (e *Engine) SetSnapshotJobStore(s SnapshotJobStore) {
	e.snapJobs = s
}

// defaultS3Target resolves the destination every new archive is written to.
// Volume backups are S3-only: falling back to host-local files would contradict
// the archive location shown in the UI and leave data behind on app hosts.
func (e *Engine) defaultS3Target() (*snapshots.S3Target, error) {
	if e.s3 != nil {
		if t, ok := e.s3.GetDefaultS3Target(); ok && t != nil {
			return t, nil
		}
	}
	return nil, fmt.Errorf("未配置默认 S3 存储目标，请先在「快照备份 → S3 存储」中添加目标并设为默认")
}

// S3Status reports whether volume backups have a destination, for the UI.
func (e *Engine) S3Status() S3DestinationStatus {
	if e.s3 == nil {
		return S3DestinationStatus{}
	}
	t, ok := e.s3.GetDefaultS3Target()
	if !ok || t == nil {
		return S3DestinationStatus{}
	}
	return S3DestinationStatus{Ready: true, TargetID: t.ID, TargetName: t.Name, Bucket: t.Bucket}
}

func (e *Engine) s3TargetByID(id string) (*snapshots.S3Target, error) {
	if e.s3 == nil || id == "" {
		return nil, errVolumeS3TargetMissing
	}
	t, ok := e.s3.GetS3Target(id)
	if !ok || t == nil {
		return nil, errVolumeS3TargetMissing
	}
	return t, nil
}

// errVolumeTargetUnknown is returned when the caller names an S3 target that
// no longer exists; errVolumeTargetNone when no target was named and no
// default is configured. The HTTP front door maps them to distinct 400
// messages so the panel can tell "pick another one" from "configure one".
var (
	errVolumeTargetUnknown = errors.New("所选备份位置不存在")
	errVolumeTargetNone    = errors.New("未选择备份位置且未配置默认目标")
)

// resolveVolumeBackupTarget picks the destination for a manual backup.
// Returns (target, false, nil) for S3, (target, true, nil) for local storage,
// or an error if the named target is invalid or no target can be resolved.
func (e *Engine) resolveVolumeBackupTarget(targetID string) (*snapshots.S3Target, bool, error) {
	id := strings.TrimSpace(targetID)
	if id == StorageTargetLocal || id == "local" {
		return &snapshots.S3Target{ID: StorageTargetLocal, Name: "本地存储 (/var/backups)"}, true, nil
	}
	if id != "" {
		t, err := e.s3TargetByID(id)
		if err != nil {
			return nil, false, errVolumeTargetUnknown
		}
		return t, false, nil
	}
	t, err := e.defaultS3Target()
	if err != nil {
		return nil, false, errVolumeTargetNone
	}
	return t, false, nil
}

// deleteVolumeObject removes one archive's S3 object. Host-local archives or
// empty paths have nothing to delete from S3.
func (e *Engine) deleteVolumeObject(arch *VolumeArchive) error {
	if arch == nil || arch.StoragePath == "" || arch.StorageTarget == StorageTargetLocal {
		return nil
	}
	target, err := e.s3TargetByID(arch.StorageTarget)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := snapshots.NewS3Client(target).DeleteObject(ctx, arch.StoragePath); err != nil {
		return fmt.Errorf("删除 S3 归档对象失败: %w", err)
	}
	return nil
}

// VolumeArchiveDownloadURL presigns a GET so the browser can fetch the archive
// straight from S3 (the control plane only issues the redirect). Host-local archives
// must be downloaded via the streaming host endpoint.
func (e *Engine) VolumeArchiveDownloadURL(arch *VolumeArchive, expires time.Duration) (string, error) {
	if arch == nil || arch.StoragePath == "" {
		return "", fmt.Errorf("归档缺少存储信息")
	}
	if arch.StorageTarget == StorageTargetLocal {
		return "", fmt.Errorf("本地存储归档请使用受管主机下载接口")
	}
	target, err := e.s3TargetByID(arch.StorageTarget)
	if err != nil {
		return "", err
	}
	u, err := snapshots.NewS3Client(target).PresignGet(arch.StoragePath, expires)
	if err != nil {
		return "", fmt.Errorf("生成 S3 下载地址失败: %v", err)
	}
	return u, nil
}

// cleanupHostPaths removes staging files best-effort; a failure only wastes a
// little disk on the host and must never fail the operation itself.
func (e *Engine) cleanupHostPaths(hub *rpc.Hub, paths ...string) {
	if hub == nil {
		return
	}
	if _, err := execOnAgent(hub, "volclean-"+randomToken(4), volumeRemoveScript(paths), 60); err != nil {
		e.log.Warn("volume staging cleanup failed", "paths", paths, "err", err)
	}
}

// ---- engine ----

// backupAppVolume archives one volume spec of an application synchronously and
// returns the stored record. target is resolved by the caller: the manual
// backup path follows the default S3 target, while a snapshot job may pin a
// specific one.
func (e *Engine) backupAppVolume(hub *rpc.Hub, app *Application, spec, note string, target *snapshots.S3Target) (*VolumeArchive, error) {
	src, dst, _, ok := parseVolumeSpec(spec)
	if !ok {
		return nil, fmt.Errorf("挂载卷格式非法: %s（应为 宿主路径:容器路径）", spec)
	}
	if target == nil {
		return nil, fmt.Errorf("未配置 S3 存储目标")
	}
	hostDir := src
	if !volumeIsBindMount(src) {
		// Named volume: resolve its host-side mountpoint first so the record
		// shows a real path and a missing volume fails loudly here rather than
		// deep inside a long tar.
		res, err := execOnAgent(hub, "volpath-"+randomToken(4), volumeResolveMountpointScript(src), 30)
		if err != nil {
			return nil, fmt.Errorf("查询 Docker 卷 %s 挂载点失败: %v", src, err)
		}
		if res.GetExitCode() != 0 {
			return nil, fmt.Errorf("Docker 卷 %s 不存在", src)
		}
		hostDir = strings.TrimSpace(string(res.GetStdout()))
		if hostDir == "" {
			return nil, fmt.Errorf("无法解析 Docker 卷 %s 的挂载点", src)
		}
	}

	archiveID := "vbk_" + randomToken(8)
	arch := &VolumeArchive{
		ID:                archiveID,
		AppID:             app.ID,
		HostID:            app.HostID,
		VolumeSpec:        spec,
		HostPath:          hostDir,
		ContainerPath:     dst,
		StorageTarget:     target.ID,
		StorageTargetName: target.Name,
		StoragePath:       volumeObjectKey(target, app.ID, archiveID),
		Note:              note,
		Status:            VolumeBackupRunning,
		CreatedAt:         time.Now(),
	}
	staging := volumeStagingPath(app.ID, archiveID)
	if e.vols != nil {
		if err := e.vols.AddArchive(arch); err != nil {
			return nil, fmt.Errorf("记录备份元数据失败: %v", err)
		}
	}

	res, err := execOnAgent(hub, "volbk-"+arch.ID, volumeBackupScript(hostDir, staging), volumeS3OpTimeout)
	if err != nil {
		e.failVolumeArchive(hub, arch, staging, err.Error())
		return arch, fmt.Errorf("备份执行失败: %v", err)
	}
	if res.GetExitCode() != 0 {
		msg := strings.TrimSpace(string(res.GetStderr()))
		if msg == "" {
			msg = fmt.Sprintf("备份失败 (exit %d)", res.GetExitCode())
		}
		e.failVolumeArchive(hub, arch, staging, msg)
		return arch, fmt.Errorf("%s", msg)
	}
	size, sha := parseVolumeArchiveMeta(res.GetStdout())
	// A tar that "succeeded" but reported no size/sha markers must not become
	// a success record: the markers are the only proof the archive is intact,
	// and a sha-less success record would skip restore verification entirely
	// (see runRestoreAppVolume). An empty volume still tars to a non-empty
	// gzip stream, so size <= 0 always means the markers were lost or cut.
	if err := validateVolumeArchiveMeta(size, sha); err != nil {
		e.failVolumeArchive(hub, arch, staging, err.Error())
		return arch, err
	}
	arch.Size = size
	arch.SHA256 = sha

	putURL, err := snapshots.NewS3Client(target).PresignPut(arch.StoragePath, 2*time.Hour)
	if err != nil {
		e.failVolumeArchive(hub, arch, staging, "生成 S3 上传地址失败: "+err.Error())
		return arch, fmt.Errorf("生成 S3 上传地址失败: %v", err)
	}
	upRes, upErr := execOnAgent(hub, "volup-"+arch.ID, volumeUploadScript(staging, putURL), volumeS3OpTimeout)
	// The staged tar has served its purpose whether or not the upload worked.
	e.cleanupHostPaths(hub, staging)
	if upErr != nil {
		e.failVolumeArchive(hub, arch, "", "上传归档到 S3 失败: "+upErr.Error())
		return arch, fmt.Errorf("上传归档到 S3 失败: %v", upErr)
	}
	if upRes.GetExitCode() != 0 {
		msg := strings.TrimSpace(string(upRes.GetStderr()))
		if msg == "" {
			msg = fmt.Sprintf("上传归档到 S3 失败 (exit %d)", upRes.GetExitCode())
		}
		e.failVolumeArchive(hub, arch, "", msg)
		return arch, fmt.Errorf("%s", msg)
	}
	arch.Status = VolumeBackupSuccess
	if e.vols != nil {
		if err := e.vols.PutArchive(arch); err != nil {
			// The object is already in S3 but the authoritative record cannot be
			// written; drop the object rather than leave it unreferenced.
			e.failVolumeArchive(hub, arch, "", "记录备份元数据失败: "+err.Error())
			return arch, fmt.Errorf("记录备份元数据失败: %v", err)
		}
	}
	e.log.Info("volume backup completed", "app", app.ID, "spec", spec, "archive", arch.ID,
		"bytes", size, "s3_target", target.Name, "key", arch.StoragePath)
	return arch, nil
}

// failVolumeArchive marks an in-flight record failed (kept for forensics),
// drops a possibly partially uploaded object, and removes staging files.
func (e *Engine) failVolumeArchive(hub *rpc.Hub, arch *VolumeArchive, staging, msg string) {
	arch.Status = VolumeBackupFailed
	arch.Error = msg
	if e.vols != nil {
		_ = e.vols.PutArchive(arch)
	}
	if err := e.deleteVolumeObject(arch); err != nil && !errors.Is(err, errVolumeS3TargetMissing) {
		e.log.Warn("failed volume archive S3 cleanup failed", "app", arch.AppID, "archive", arch.ID, "err", err)
	}
	if staging != "" {
		e.cleanupHostPaths(hub, staging)
	}
}

// BackupSnapshotOptions configures an application snapshot backup.
type BackupSnapshotOptions struct {
	Specs           []string `json:"specs"`
	Note            string   `json:"note"`
	StorageTarget   string   `json:"storage_target"`   // "local", "s3", targetID, or "" (falls back to default S3)
	ConsistencyMode string   `json:"consistency_mode"` // "online" (default), "pause", "stop"
	Keep            int      `json:"keep"`
}

// RestoreVolumeOptions configures an application snapshot restore.
type RestoreVolumeOptions struct {
	CleanRestore bool     `json:"clean_restore"` // whether to wipe child files before unpacking (default: true)
	Specs        []string `json:"specs"`         // subset of volume specs to restore (empty = all in archive)
}

// BackupAppSnapshot archives one or all volumes of an application as a unified snapshot.
func (e *Engine) BackupAppSnapshot(app *Application, opts BackupSnapshotOptions) (*VolumeArchive, error) {
	if !e.acquireVolumeSlot(app.ID) {
		return nil, fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)
	return e.runBackupAppSnapshot(app, opts)
}

// runBackupAppSnapshot assumes the caller already holds the app's volume slot.
func (e *Engine) runBackupAppSnapshot(app *Application, opts BackupSnapshotOptions) (*VolumeArchive, error) {
	if e.IsRunning(app.ID) {
		return nil, fmt.Errorf("应用正在部署，暂不能备份数据卷")
	}

	disc, err := e.DiscoverAppVolumes(app)
	if err != nil {
		return nil, err
	}
	if len(disc) == 0 {
		return nil, fmt.Errorf("应用未配置任何挂载卷")
	}

	var filtered []DiscoveredVolume
	if len(opts.Specs) > 0 {
		for _, s := range opts.Specs {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			found := false
			for _, d := range disc {
				if d.VolumeSpec == s || d.ContainerPath == s {
					filtered = append(filtered, d)
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("挂载卷 %s 不在当前应用挂载清单中", s)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("应用未配置任何挂载卷")
		}
	} else {
		filtered = disc
	}

	target, isLocal, err := e.resolveVolumeBackupTarget(opts.StorageTarget)
	if err != nil {
		return nil, err
	}

	hub := e.volumeHub(app)
	if hub == nil {
		return nil, fmt.Errorf("目标主机 Agent 不在线")
	}

	consistency := strings.ToLower(strings.TrimSpace(opts.ConsistencyMode))
	if consistency == "" {
		consistency = ConsistencyModeOnline
	}
	if consistency != ConsistencyModeOnline && consistency != ConsistencyModePause && consistency != ConsistencyModeStop {
		return nil, fmt.Errorf("不支持的一致性模式: %s (有效值为 online, pause, stop)", opts.ConsistencyMode)
	}

	if consistency == ConsistencyModePause {
		if err := e.PauseApp(hub, app); err != nil {
			return nil, fmt.Errorf("暂停容器失败: %w", err)
		}
		defer func() {
			_ = e.UnpauseApp(hub, app)
		}()
	} else if consistency == ConsistencyModeStop {
		wasRunning, _ := e.IsAppRunning(hub, app)
		if wasRunning {
			if err := e.StopApp(hub, app); err != nil {
				return nil, fmt.Errorf("停止应用失败: %w", err)
			}
			defer func() {
				_ = e.StartApp(hub, app)
			}()
		}
	}

	archiveID := "vbk_" + randomToken(8)
	items := make([]VolumeItemArchive, len(filtered))
	for i, d := range filtered {
		items[i] = VolumeItemArchive{
			VolumeSpec:    d.VolumeSpec,
			VolumeType:    d.VolumeType,
			HostPath:      d.HostPath,
			ContainerPath: d.ContainerPath,
			Service:       d.Service,
		}
	}

	specStr := "ALL"
	if len(items) == 1 {
		specStr = items[0].VolumeSpec
	}

	arch := &VolumeArchive{
		ID:              archiveID,
		AppID:           app.ID,
		HostID:          app.HostID,
		VolumeSpec:      specStr,
		Volumes:         items,
		HostPath:        items[0].HostPath,
		ContainerPath:   items[0].ContainerPath,
		ConsistencyMode: consistency,
		Note:            opts.Note,
		Status:          VolumeBackupRunning,
		CreatedAt:       time.Now(),
	}

	if isLocal {
		arch.StorageTarget = StorageTargetLocal
		arch.StorageTargetName = "受管主机本地存储"
		arch.StoragePath = volumeLocalArchivePath(app.ID, archiveID)
	} else {
		arch.StorageTarget = target.ID
		arch.StorageTargetName = target.Name
		arch.StoragePath = volumeObjectKey(target, app.ID, archiveID)
	}

	staging := volumeStagingPath(app.ID, archiveID)
	if e.vols != nil {
		if err := e.vols.AddArchive(arch); err != nil {
			return nil, fmt.Errorf("记录备份元数据失败: %v", err)
		}
	}

	manifest := SnapshotManifest{
		Version:   "1.0",
		AppID:     app.ID,
		ArchiveID: archiveID,
		CreatedAt: time.Now(),
		Volumes:   items,
	}
	manifestJSON, _ := json.Marshal(manifest)
	pkgDir := path.Join(volumeStagingDir(app.ID), "pkg_"+archiveID)

	tarPath := staging
	if isLocal {
		tarPath = arch.StoragePath
	}

	res, err := execOnAgent(hub, "volbk-"+arch.ID, volumeMultiBackupScript(pkgDir, tarPath, string(manifestJSON), items), volumeS3OpTimeout)
	if err != nil {
		e.failVolumeArchive(hub, arch, staging, err.Error())
		if isLocal {
			e.cleanupHostPaths(hub, arch.StoragePath)
		}
		return arch, fmt.Errorf("备份执行失败: %v", err)
	}
	if res.GetExitCode() != 0 {
		msg := strings.TrimSpace(string(res.GetStderr()))
		if msg == "" {
			msg = fmt.Sprintf("备份失败 (exit %d)", res.GetExitCode())
		}
		e.failVolumeArchive(hub, arch, staging, msg)
		if isLocal {
			e.cleanupHostPaths(hub, arch.StoragePath)
		}
		return arch, fmt.Errorf("%s", msg)
	}

	size, sha := parseVolumeArchiveMeta(res.GetStdout())
	if err := validateVolumeArchiveMeta(size, sha); err != nil {
		e.failVolumeArchive(hub, arch, staging, err.Error())
		if isLocal {
			e.cleanupHostPaths(hub, arch.StoragePath)
		}
		return arch, err
	}
	arch.Size = size
	arch.SHA256 = sha

	if !isLocal {
		putURL, err := snapshots.NewS3Client(target).PresignPut(arch.StoragePath, 2*time.Hour)
		if err != nil {
			e.failVolumeArchive(hub, arch, staging, "生成 S3 上传地址失败: "+err.Error())
			return arch, fmt.Errorf("生成 S3 上传地址失败: %v", err)
		}
		upRes, upErr := execOnAgent(hub, "volup-"+arch.ID, volumeUploadScript(staging, putURL), volumeS3OpTimeout)
		e.cleanupHostPaths(hub, staging)
		if upErr != nil {
			e.failVolumeArchive(hub, arch, "", "上传归档到 S3 失败: "+upErr.Error())
			return arch, fmt.Errorf("上传归档到 S3 失败: %v", upErr)
		}
		if upRes.GetExitCode() != 0 {
			msg := strings.TrimSpace(string(upRes.GetStderr()))
			if msg == "" {
				msg = fmt.Sprintf("上传归档到 S3 失败 (exit %d)", upRes.GetExitCode())
			}
			e.failVolumeArchive(hub, arch, "", msg)
			return arch, fmt.Errorf("%s", msg)
		}
	}

	arch.Status = VolumeBackupSuccess
	now := time.Now()
	arch.FinishedAt = &now
	if e.vols != nil {
		if err := e.vols.PutArchive(arch); err != nil {
			e.failVolumeArchive(hub, arch, "", "记录备份元数据失败: "+err.Error())
			return arch, fmt.Errorf("记录备份元数据失败: %v", err)
		}
	}

	// Keep > 0 prunes to that count after a successful run; the manual paths
	// pass noPruneVolumes so a one-off safety copy never deletes anything.
	if keep, ok := effectiveVolumeKeep(opts.Keep); ok {
		e.pruneVolumeArchives(hub, app.ID, arch.VolumeSpec, keep)
	}

	e.log.Info("volume backup completed", "app", app.ID, "spec", arch.VolumeSpec, "archive", arch.ID,
		"bytes", size, "target", arch.StorageTargetName, "path", arch.StoragePath)
	return arch, nil
}

// BackupAppVolumes archives one or more volumes of an application. specs empty
// means every declared volume.
func (e *Engine) BackupAppVolumes(app *Application, specs []string, note string) ([]*VolumeArchive, error) {
	if !e.acquireVolumeSlot(app.ID) {
		return nil, fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)
	return e.runBackupAppVolumes(app, specs, note)
}

// runBackupAppVolumes assumes the caller already holds the app's volume slot.
// Manual backup: never prunes — the operator asked for one copy, not for
// deleting older ones.
func (e *Engine) runBackupAppVolumes(app *Application, specs []string, note string) ([]*VolumeArchive, error) {
	target, err := e.defaultS3Target()
	if err != nil {
		return nil, err
	}
	return e.runBackupAppVolumesWith(app, specs, note, target, noPruneVolumes)
}

// runBackupAppVolumesWith backs the same flow with an explicit S3 target and
// retention count, which is what a snapshot job provides. The caller must hold
// the app's volume slot.
func (e *Engine) runBackupAppVolumesWith(app *Application, specs []string, note string, target *snapshots.S3Target, keep int) ([]*VolumeArchive, error) {
	targetID := ""
	if target != nil {
		targetID = target.ID
	}
	arch, err := e.runBackupAppSnapshot(app, BackupSnapshotOptions{
		Specs:         specs,
		Note:          note,
		StorageTarget: targetID,
		Keep:          keep,
	})
	if arch != nil {
		return []*VolumeArchive{arch}, err
	}
	return nil, err
}

// effectiveVolumeKeep resolves the backup option's keep knob: noPruneVolumes
// means the caller (a manual backup) explicitly opted out of pruning, any
// other non-positive value falls back to the package default, and a positive
// value is the rolling window a scheduled job asked for. The bool reports
// whether a prune should run at all.
func effectiveVolumeKeep(keep int) (int, bool) {
	if keep == noPruneVolumes {
		return 0, false
	}
	if keep <= 0 {
		return volumeRetention, true
	}
	return keep, true
}

// staleVolumeArchives returns the successful archives of one spec beyond the
// keep count. Failed records stay put: they are small metadata the operator
// may still inspect, and they must not evict a restorable archive.
func staleVolumeArchives(all []*VolumeArchive, spec string, keep int) []*VolumeArchive {
	if keep <= 0 {
		keep = volumeRetention
	}
	kept := 0
	var stale []*VolumeArchive
	for _, a := range all { // ListArchives is newest first
		if a.Status != VolumeBackupSuccess {
			continue
		}
		if spec != "" && spec != "ALL" {
			matched := a.VolumeSpec == spec
			if !matched {
				for _, v := range a.Volumes {
					if v.VolumeSpec == spec {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}
		kept++
		if kept > keep {
			stale = append(stale, a)
		}
	}
	return stale
}

// pruneVolumeArchives keeps the newest keep successful archives of one spec and
// deletes the rest.
func (e *Engine) pruneVolumeArchives(hub *rpc.Hub, appID, spec string, keep int) {
	if e.vols == nil {
		return
	}
	stale := staleVolumeArchives(e.vols.ListArchives(appID), spec, keep)
	for _, a := range stale {
		if a.StorageTarget == StorageTargetLocal {
			if hub != nil && a.StoragePath != "" {
				_, _ = execOnAgent(hub, "volrm-"+a.ID, "rm -f "+shellQuote(a.StoragePath), 30)
			}
		} else {
			err := e.deleteVolumeObject(a)
			if err != nil && !errors.Is(err, errVolumeS3TargetMissing) {
				e.log.Warn("volume archive prune skipped: S3 delete failed", "app", appID, "spec", spec, "archive", a.ID, "err", err)
				continue
			}
			if err != nil {
				e.log.Warn("volume archive prune drops record of deleted S3 target", "app", appID, "archive", a.ID)
			}
		}
		e.cleanupHostPaths(hub, volumeStagingPath(appID, a.ID), volumeRestorePath(appID, a.ID), volumeSafetyPath(appID, a.ID))
		if err := e.vols.DeleteArchive(appID, a.ID); err != nil {
			e.log.Warn("volume archive metadata delete failed", "app", appID, "archive", a.ID, "err", err)
		}
	}
}

// RestoreAppVolume unpacks an archive back onto its volume with default options.
func (e *Engine) RestoreAppVolume(app *Application, arch *VolumeArchive) (restarted bool, err error) {
	return e.RestoreAppVolumeWithOptions(app, arch, RestoreVolumeOptions{CleanRestore: true})
}

// RestoreAppVolumeWithOptions unpacks an archive back onto its volume using the specified options.
func (e *Engine) RestoreAppVolumeWithOptions(app *Application, arch *VolumeArchive, opts RestoreVolumeOptions) (restarted bool, err error) {
	if !e.acquireVolumeSlot(app.ID) {
		return false, snapshots.ErrVolumeRestoreBusy
	}
	defer e.releaseVolumeSlot(app.ID)
	return e.runRestoreAppVolume(app, arch, opts)
}

// resolveRestoreVolumeDir validates that the archive's volume spec is still
// declared on the app (trim-exact match, the same rule the backup front door
// enforces) and returns the host directory the archive must be unpacked into.
func (e *Engine) resolveRestoreVolumeDir(hub *rpc.Hub, app *Application, arch *VolumeArchive) (string, error) {
	spec := strings.TrimSpace(arch.VolumeSpec)
	if spec == "" {
		return "", fmt.Errorf("归档卷规格缺失，无法恢复（请重新备份）")
	}
	declared := false
	for _, v := range app.Volumes {
		if strings.TrimSpace(v) == spec {
			declared = true
			break
		}
	}
	if !declared {
		return "", fmt.Errorf("卷配置已变更，请重新备份")
	}
	src, dst, _, ok := parseVolumeSpec(spec)
	if !ok {
		return "", fmt.Errorf("归档卷规格无效，请重新备份")
	}
	if arch.ContainerPath != "" && arch.ContainerPath != dst {
		return "", fmt.Errorf("卷配置已变更，请重新备份")
	}
	if volumeIsBindMount(src) {
		if arch.HostPath != src {
			return "", fmt.Errorf("卷配置已变更，请重新备份")
		}
		return src, nil
	}
	// Named volume: re-resolve the mountpoint instead of trusting the
	// recorded path — the volume may have been deleted and recreated.
	res, err := execOnAgent(hub, "volpath-"+randomToken(4), volumeResolveMountpointScript(src), 30)
	if err != nil {
		return "", fmt.Errorf("查询 Docker 卷 %s 挂载点失败: %v", src, err)
	}
	if res.GetExitCode() != 0 {
		return "", fmt.Errorf("Docker 卷 %s 不存在，卷配置已变更，请重新备份", src)
	}
	hostDir := strings.TrimSpace(string(res.GetStdout()))
	if hostDir == "" {
		return "", fmt.Errorf("无法解析 Docker 卷 %s 的挂载点", src)
	}
	if hostDir != arch.HostPath {
		return "", fmt.Errorf("卷配置已变更，请重新备份")
	}
	return hostDir, nil
}

// resolveRestoreDestinations maps items in an archive to their respective target host directories.
func (e *Engine) resolveRestoreDestinations(hub *rpc.Hub, app *Application, arch *VolumeArchive, requestedSpecs []string) ([]RestoreTarget, error) {
	if arch == nil {
		return nil, fmt.Errorf("归档记录不存在")
	}
	items := arch.Volumes
	if len(items) == 0 && (arch.VolumeSpec != "" || arch.HostPath != "") {
		vType := "bind"
		if !volumeIsBindMount(arch.HostPath) && strings.Contains(arch.VolumeSpec, ":") && !strings.Contains(arch.VolumeSpec, "/") {
			vType = "volume"
		}
		items = []VolumeItemArchive{
			{
				VolumeSpec:    arch.VolumeSpec,
				VolumeType:    vType,
				HostPath:      arch.HostPath,
				ContainerPath: arch.ContainerPath,
			},
		}
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("归档卷规格缺失，无法恢复（请重新备份）")
	}

	var targets []RestoreTarget
	for idx, item := range items {
		if len(requestedSpecs) > 0 {
			matched := false
			for _, reqSpec := range requestedSpecs {
				reqSpec = strings.TrimSpace(reqSpec)
				if reqSpec == item.VolumeSpec || reqSpec == item.ContainerPath {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		destDir := ""
		if isComposeApp(app) {
			disc, err := e.DiscoverAppVolumes(app)
			if err == nil {
				for _, d := range disc {
					if d.VolumeSpec == item.VolumeSpec || (d.ContainerPath == item.ContainerPath && (d.Service == item.Service || item.Service == "")) {
						destDir = d.HostPath
						break
					}
				}
			}
			if destDir == "" {
				if item.VolumeType == "volume" {
					src, _, _, _ := parseVolumeSpec(item.VolumeSpec)
					if src != "" && hub != nil {
						cmd := fmt.Sprintf(`docker volume create %s >/dev/null 2>&1 || true; docker volume inspect --format '{{.Mountpoint}}' %s`, shellQuote(src), shellQuote(src))
						res, err := execOnAgent(hub, "volres-"+randomToken(4), cmd, 30)
						if err == nil && res.GetExitCode() == 0 {
							destDir = strings.TrimSpace(string(res.GetStdout()))
						}
					}
				}
				if destDir == "" {
					destDir = item.HostPath
				}
			}
		} else {
			if arch.HostID == app.HostID {
				singleArch := &VolumeArchive{
					VolumeSpec:    item.VolumeSpec,
					HostPath:      item.HostPath,
					ContainerPath: item.ContainerPath,
				}
				dir, err := e.resolveRestoreVolumeDir(hub, app, singleArch)
				if err != nil {
					return nil, err
				}
				destDir = dir
			} else {
				found := false
				for _, v := range app.Volumes {
					src, dst, _, ok := parseVolumeSpec(v)
					if !ok {
						continue
					}
					if strings.TrimSpace(v) == strings.TrimSpace(item.VolumeSpec) || dst == item.ContainerPath {
						found = true
						if volumeIsBindMount(src) {
							destDir = src
						} else {
							if hub != nil {
								cmd := fmt.Sprintf(`docker volume create %s >/dev/null 2>&1 || true; docker volume inspect --format '{{.Mountpoint}}' %s`, shellQuote(src), shellQuote(src))
								res, err := execOnAgent(hub, "volres-"+randomToken(4), cmd, 30)
								if err == nil && res.GetExitCode() == 0 {
									destDir = strings.TrimSpace(string(res.GetStdout()))
								}
							}
							if destDir == "" {
								destDir = src
							}
						}
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("目标主机应用未配置挂载卷 %s，无法恢复", item.VolumeSpec)
				}
			}
		}

		if destDir == "" {
			return nil, fmt.Errorf("无法解析挂载卷 %s 的目标恢复目录", item.VolumeSpec)
		}

		targets = append(targets, RestoreTarget{
			Index:   idx,
			DestDir: destDir,
			Spec:    item.VolumeSpec,
		})
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("未匹配到需要恢复的挂载卷")
	}
	return targets, nil
}

// recordVolumeRestoreOutcome writes the restore result back to the archive.
func (e *Engine) recordVolumeRestoreOutcome(app *Application, arch *VolumeArchive, err error) {
	if arch == nil || e.vols == nil {
		return
	}
	now := time.Now()
	arch.LastRestoreAt = &now
	if err != nil {
		arch.LastRestoreStatus = VolumeRestoreFailed
		arch.LastRestoreError = err.Error()
	} else {
		arch.LastRestoreStatus = VolumeRestoreSuccess
		arch.LastRestoreError = ""
	}
	if perr := e.vols.PutArchive(arch); perr != nil {
		e.log.Warn("record volume restore result failed", "app", app.ID, "archive", arch.ID, "err", perr)
	}
}

// runRestoreAppVolume restores an archive: S3-specific or local preflight, then the shared core.
func (e *Engine) runRestoreAppVolume(app *Application, arch *VolumeArchive, opts RestoreVolumeOptions) (restarted bool, err error) {
	defer func() { e.recordVolumeRestoreOutcome(app, arch, err) }()
	if e.volumeHub(app) == nil {
		return false, snapshots.ErrVolumeRestoreOffline
	}
	if arch == nil || arch.StoragePath == "" {
		return false, fmt.Errorf("归档记录不完整（缺少存储信息）")
	}
	if arch.Status != VolumeBackupSuccess {
		return false, fmt.Errorf("归档未成功完成，不能用于恢复")
	}

	if arch.StorageTarget == StorageTargetLocal {
		if arch.HostID != app.HostID {
			return false, fmt.Errorf("本地存储归档仅保存在原主机 (%s)，跨主机恢复请使用 S3 存储归档或上传归档", arch.HostID)
		}
		return e.runRestoreCore(app, arch, "", arch.SHA256, "", opts)
	}

	if arch.SHA256 == "" {
		return false, fmt.Errorf("归档缺少 SHA256 标记，无法校验完整性，请重新备份")
	}
	target, err := e.s3TargetByID(arch.StorageTarget)
	if err != nil {
		return false, fmt.Errorf("归档的 S3 存储目标已不存在，无法下载归档")
	}
	getURL, err := snapshots.NewS3Client(target).PresignGet(arch.StoragePath, 2*time.Hour)
	if err != nil {
		return false, fmt.Errorf("生成 S3 下载地址失败: %v", err)
	}
	return e.runRestoreCore(app, arch, getURL, arch.SHA256, "从 S3 下载归档失败", opts)
}

// uploadedRestoreItem couples one uploaded file's archive record with the
// one-time URL the agent pulls its bytes from and its expected digest.
// Specs optionally restricts a multi-volume snapshot tar to a subset of its
// manifest volumes: it is passed as requestedSpecs so resolveRestoreDestinations
// filters items but keeps their original indices — the tar's volumes/vol_<i>
// layout only lines up with the untouched index.
type uploadedRestoreItem struct {
	Arch        *VolumeArchive
	DownloadURL string
	ExpectedSHA string
	Specs       []string
}

// uploadStagingPaths lists where each batch item's bytes are pulled to on the
// host. A path only exists for certain after a successful pull, but rm -f on
// a missing file is a no-op while a partial download (curl -o interrupted
// mid-body) must be cleaned either way, so the batch cleanup is
// unconditional and covers every item.
func uploadStagingPaths(app *Application, items []*uploadedRestoreItem) []string {
	paths := make([]string, 0, len(items))
	for _, it := range items {
		paths = append(paths, volumeRestorePath(app.ID, it.Arch.ID))
	}
	return paths
}

// runRestoreUploadedBatch restores a batch of uploaded files into their paired
// volumes as ONE import: every file is pulled onto the host and verified
// BEFORE any downtime (one bad file cancels the whole batch without stopping
// the container), then the app is stopped once, each file is unpacked into its
// own volume, and the app is restarted once. Outcomes are written to the
// individual archive records; the panel-staged files and one-time tokens are
// dropped however the run ends.
func (e *Engine) runRestoreUploadedBatch(app *Application, items []*uploadedRestoreItem, opts RestoreVolumeOptions) (restarted bool, err error) {
	pairErrs := make([]error, len(items))
	defer func() {
		for i, it := range items {
			e.recordVolumeRestoreOutcome(app, it.Arch, pairErrs[i])
		}
		if e.volUploads != nil {
			for _, it := range items {
				e.volUploads.RemoveFile(it.Arch.ID)
				e.volUploads.DropTokenByArchive(it.Arch.ID)
			}
		}
	}()

	hub := e.volumeHub(app)
	if hub == nil {
		for i := range pairErrs {
			pairErrs[i] = snapshots.ErrVolumeRestoreOffline
		}
		return false, snapshots.ErrVolumeRestoreOffline
	}
	if e.IsRunning(app.ID) {
		for i := range pairErrs {
			pairErrs[i] = snapshots.ErrVolumeRestoreDeploying
		}
		return false, snapshots.ErrVolumeRestoreDeploying
	}
	defer e.cleanupHostPaths(hub, uploadStagingPaths(app, items)...)

	type stagedItem struct {
		idx     int
		item    *uploadedRestoreItem
		staging string
		targets []RestoreTarget
	}
	ready := make([]*stagedItem, 0, len(items))

	// Phase 1 — full preflight before any downtime: resolve every destination
	// and pull+verify every file. One failure cancels the whole batch, so a
	// partially imported app (some volumes new data, others old) never exists
	// and the container is never stopped for nothing.
	for i, it := range items {
		targets, terr := e.resolveRestoreDestinations(hub, app, it.Arch, it.Specs)
		var staging string
		if terr == nil {
			staging, _, terr = e.pullRestoreStaging(hub, app, it.Arch, it.DownloadURL, it.ExpectedSHA, "从面板下载归档失败")
		}
		if terr != nil {
			// The failing pair carries its real error; the others state why
			// they were skipped so the archive list stays honest per record.
			for j := range pairErrs {
				if j == i {
					pairErrs[j] = terr
				} else {
					pairErrs[j] = fmt.Errorf("批次导入已取消：目标卷 %s 的文件拉取或校验失败（%v），未对任何卷做改动", it.Arch.VolumeSpec, terr)
				}
			}
			return false, terr
		}
		ready = append(ready, &stagedItem{idx: i, item: it, staging: staging, targets: targets})
	}

	// Phase 2 — one stop for the whole batch, one restart after all unpacks.
	wasRunning, _ := e.IsAppRunning(hub, app)
	if wasRunning {
		if serr := e.StopApp(hub, app); serr != nil {
			serr = fmt.Errorf("停止应用失败: %v", serr)
			for j := range pairErrs {
				pairErrs[j] = serr
			}
			return false, serr
		}
		defer func() {
			if startErr := e.StartApp(hub, app); startErr != nil {
				restartErr := fmt.Errorf("恢复后重启应用失败: %v", startErr)
				e.log.Error("volume restore: restart failed", "app", app.ID, "err", restartErr)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				for j := range pairErrs {
					if pairErrs[j] == nil {
						pairErrs[j] = restartErr
					}
				}
				return
			}
			running, _ := e.IsAppRunning(hub, app)
			if !running {
				restartErr := fmt.Errorf("恢复后应用未保持运行")
				e.log.Error("volume restore: app not running after restart", "app", app.ID)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				for j := range pairErrs {
					if pairErrs[j] == nil {
						pairErrs[j] = restartErr
					}
				}
				return
			}
			restarted = true
		}()
	}

	// Phase 3 — per-pair unpack. The container is already stopped and every
	// other pair's data is staged and verified, so one failure marks only its
	// own record; the remaining pairs still restore.
	var unpackErrs []error
	for _, st := range ready {
		if uerr := e.execRestoreTargets(hub, app, st.item.Arch, st.staging, opts.CleanRestore, st.targets); uerr != nil {
			pairErrs[st.idx] = fmt.Errorf("目标卷 %s: %v", st.item.Arch.VolumeSpec, uerr)
			unpackErrs = append(unpackErrs, pairErrs[st.idx])
		}
	}
	if len(unpackErrs) > 0 {
		err = errors.Join(unpackErrs...)
		return restarted, err
	}
	e.log.Info("volume batch restored", "app", app.ID, "targets", len(ready), "restarted", restarted)
	return restarted, nil
}

// pullRestoreStaging brings the archive bytes onto the host (or points at the
// local-storage copy) and verifies them: a supplied SHA256 is checked as a
// hard match, and an upload without one is still probed as a tar so a bad
// file fails before any downtime. downloaded=true means the caller must clean
// the staging path up once the restore is done; the helper itself cleans a
// partial download when verification or the transfer fails.
func (e *Engine) pullRestoreStaging(hub *rpc.Hub, app *Application, arch *VolumeArchive, downloadURL, expectedSHA, dlFail string) (staging string, downloaded bool, err error) {
	if arch.StorageTarget == StorageTargetLocal {
		if expectedSHA != "" {
			shaRes, shaErr := execOnAgent(hub, "volsha-"+arch.ID, volumeSHA256Script(arch.StoragePath), 120)
			if shaErr != nil || shaRes.GetExitCode() != 0 {
				return "", false, fmt.Errorf("本地归档文件校验失败: %v", shaErr)
			}
			if got := strings.TrimSpace(string(shaRes.GetStdout())); got != expectedSHA {
				return "", false, fmt.Errorf("归档校验失败：SHA256 不匹配（期望 %s，实际 %s）", expectedSHA, got)
			}
		}
		return arch.StoragePath, false, nil
	}

	staging = volumeRestorePath(app.ID, arch.ID)
	fail := func(ferr error) (string, bool, error) {
		e.cleanupHostPaths(hub, staging)
		return "", false, ferr
	}
	dlRes, dlErr := execOnAgent(hub, "voldl-"+arch.ID, volumeDownloadScript(downloadURL, staging), volumeS3OpTimeout)
	if dlErr != nil {
		return fail(fmt.Errorf("%s: %v", dlFail, dlErr))
	}
	if dlRes.GetExitCode() != 0 {
		msg := strings.TrimSpace(string(dlRes.GetStderr()))
		if msg == "" {
			msg = fmt.Sprintf("%s (exit %d)", dlFail, dlRes.GetExitCode())
		}
		return fail(fmt.Errorf("%s", msg))
	}
	if expectedSHA != "" {
		shaRes, shaErr := execOnAgent(hub, "volsha-"+arch.ID, volumeSHA256Script(staging), 120)
		if shaErr != nil {
			return fail(fmt.Errorf("校验下载归档失败: %v", shaErr))
		}
		if got := strings.TrimSpace(string(shaRes.GetStdout())); got != expectedSHA {
			return fail(fmt.Errorf("归档校验失败：SHA256 不匹配（期望 %s，实际 %s）", expectedSHA, got))
		}
	} else {
		probeRes, probeErr := execOnAgent(hub, "volprobe-"+arch.ID, volumeTarProbeScript(staging), 120)
		if probeErr != nil {
			return fail(fmt.Errorf("校验上传文件失败: %v", probeErr))
		}
		if probeRes.GetExitCode() != 0 {
			msg := strings.TrimSpace(string(probeRes.GetStderr()))
			if msg == "" {
				msg = "不是有效的 tar 归档"
			}
			return fail(fmt.Errorf("上传文件校验失败：%s", msg))
		}
	}
	return staging, true, nil
}

// execRestoreTargets runs the agent-side unpack for one archive: safety copy
// of each target's current contents, optional clean wipe, then extract.
func (e *Engine) execRestoreTargets(hub *rpc.Hub, app *Application, arch *VolumeArchive, staging string, cleanRestore bool, targets []RestoreTarget) error {
	safetyDir := volumeSafetyPath(app.ID, arch.ID)
	script := volumeMultiRestoreScript(staging, safetyDir, cleanRestore, targets)
	res, rerr := execOnAgent(hub, "volre-"+arch.ID, script, volumeS3OpTimeout)
	if rerr != nil {
		return fmt.Errorf("恢复执行失败: %v", rerr)
	}
	if res.GetExitCode() != 0 {
		return fmt.Errorf("恢复失败(exit %d): %s", res.GetExitCode(), strings.TrimSpace(string(res.GetStderr())))
	}
	return nil
}

// runRestoreCore is the shared core of restore paths: volume-dir resolution,
// deployment check, download/verify, stop -> safety copy -> clean -> unpack -> restart.
func (e *Engine) runRestoreCore(app *Application, arch *VolumeArchive, downloadURL, expectedSHA, dlFail string, opts RestoreVolumeOptions) (restarted bool, err error) {
	hub := e.volumeHub(app)
	if hub == nil {
		return false, snapshots.ErrVolumeRestoreOffline
	}
	if e.IsRunning(app.ID) {
		return false, snapshots.ErrVolumeRestoreDeploying
	}

	targets, err := e.resolveRestoreDestinations(hub, app, arch, opts.Specs)
	if err != nil {
		return false, err
	}

	staging, downloaded, err := e.pullRestoreStaging(hub, app, arch, downloadURL, expectedSHA, dlFail)
	if err != nil {
		return false, err
	}
	if downloaded {
		defer e.cleanupHostPaths(hub, staging)
	}

	wasRunning, _ := e.IsAppRunning(hub, app)
	if wasRunning {
		if err := e.StopApp(hub, app); err != nil {
			return false, fmt.Errorf("停止应用失败: %v", err)
		}
		defer func() {
			if startErr := e.StartApp(hub, app); startErr != nil {
				restartErr := fmt.Errorf("恢复后重启应用失败: %v", startErr)
				e.log.Error("volume restore: restart failed", "app", app.ID, "err", restartErr)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				return
			}
			running, _ := e.IsAppRunning(hub, app)
			if !running {
				restartErr := fmt.Errorf("恢复后应用未保持运行")
				e.log.Error("volume restore: app not running after restart", "app", app.ID)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				return
			}
			restarted = true
		}()
	}

	if uerr := e.execRestoreTargets(hub, app, arch, staging, opts.CleanRestore, targets); uerr != nil {
		return restarted, uerr
	}
	e.log.Info("volume restored", "app", app.ID, "archive", arch.ID, "targets", len(targets), "restarted", restarted)
	return restarted, nil
}

// DeleteAppVolumeArchive removes an archive's storage object and metadata.
func (e *Engine) DeleteAppVolumeArchive(app *Application, arch *VolumeArchive) error {
	if !e.acquireVolumeSlot(app.ID) {
		return fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)
	return e.deleteAppVolumeArchive(app, arch)
}

// deleteAppVolumeArchive assumes the caller already holds the app's volume slot.
func (e *Engine) deleteAppVolumeArchive(app *Application, arch *VolumeArchive) error {
	if arch == nil {
		return fmt.Errorf("归档记录不存在")
	}
	if arch.StorageTarget == StorageTargetLocal {
		if hub := e.volumeHub(app); hub != nil && arch.StoragePath != "" {
			_, _ = execOnAgent(hub, "volrm-"+arch.ID, "rm -f "+shellQuote(arch.StoragePath), 30)
		}
	} else {
		err := e.deleteVolumeObject(arch)
		if err != nil && !errors.Is(err, errVolumeS3TargetMissing) {
			return err
		}
		if err != nil {
			e.log.Warn("volume archive S3 target gone, dropping record", "app", app.ID, "archive", arch.ID)
		}
	}
	if arch.Source == VolumeArchiveSourceUpload && e.volUploads != nil {
		e.volUploads.RemoveFile(arch.ID)
		e.volUploads.DropTokenByArchive(arch.ID)
	}
	if hub := e.volumeHub(app); hub != nil {
		e.cleanupHostPaths(hub,
			volumeStagingPath(app.ID, arch.ID),
			volumeRestorePath(app.ID, arch.ID),
			volumeSafetyPath(app.ID, arch.ID),
		)
	}
	if e.vols == nil {
		return nil
	}
	return e.vols.DeleteArchive(app.ID, arch.ID)
}

// PurgeVolumeArchives removes an application's S3 objects, host staging
// directory and all of its metadata. Called from the purge flow (host already
// proven online). Every object is attempted; metadata goes either way because
// the owning application is being deleted.
//
// Caller contract: the caller must already hold the app's volume slot (the
// purge flow acquires it at entry and holds it for the whole purge), so this
// function must NOT try to acquire it again — the slot is non-reentrant and
// a self-acquire would always fail and skip the cleanup entirely.
func (e *Engine) PurgeVolumeArchives(app *Application) error {
	var firstErr error
	// Scheduled app_volume jobs live in the snapshot module; with the app gone
	// each cron sweep would fail on 「应用不存在」and append a failing archive
	// record forever, so drop them together with the archives. A failure here
	// must not skip the archive cleanup below: the application is deleted
	// either way, and bailing out would strand paid S3 objects behind
	// metadata nobody can reach anymore.
	if e.snapJobs != nil {
		n, err := e.snapJobs.DeleteJobsByApp(app.ID)
		if err != nil {
			firstErr = fmt.Errorf("删除应用数据卷备份任务失败: %w", err)
		} else if n > 0 {
			e.log.Info("dropped app_volume snapshot jobs with the application", "app", app.ID, "jobs", n)
		}
	}
	if e.vols == nil {
		return firstErr
	}
	for _, a := range e.vols.ListArchives(app.ID) {
		// Uploaded records carry no S3 object; their panel temporary files
		// and download tokens go with the application.
		if a.Source == VolumeArchiveSourceUpload && e.volUploads != nil {
			e.volUploads.RemoveFile(a.ID)
			e.volUploads.DropTokenByArchive(a.ID)
		}
		err := e.deleteVolumeObject(a)
		if err == nil {
			continue
		}
		if errors.Is(err, errVolumeS3TargetMissing) {
			e.log.Warn("volume archive S3 target gone during purge", "app", app.ID, "archive", a.ID)
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("删除 S3 归档对象 %s 失败: %v", a.ID, err)
		}
	}
	if hub := e.volumeHub(app); hub != nil {
		dir := volumeStagingDir(app.ID)
		res, err := execOnAgent(hub, "volpurge-"+randomToken(4), "rm -rf "+shellQuote(dir), 60)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("清理数据卷暂存目录失败: %v", err)
			}
		} else if res.GetExitCode() != 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("清理数据卷暂存目录失败(exit %d): %s", res.GetExitCode(), strings.TrimSpace(string(res.GetStderr())))
			}
		}
	}
	if err := e.vols.DeleteAppArchives(app.ID); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// volumeHub resolves the app host's agent channel. e.reg may be nil in unit
// tests (handlers wired without a registry), so the nil check stays.
func (e *Engine) volumeHub(app *Application) *rpc.Hub {
	if e.reg == nil {
		return nil
	}
	return e.reg.Hub(app.HostID)
}

// acquireVolumeSlot serializes backup/restore per application: the second
// caller gets a clear conflict instead of interleaving two agent scripts that
// touch the same data dir.
func (e *Engine) acquireVolumeSlot(appID string) bool {
	e.volMu.Lock()
	defer e.volMu.Unlock()
	if e.volBusy == nil {
		e.volBusy = make(map[string]bool)
	}
	if e.volBusy[appID] {
		return false
	}
	e.volBusy[appID] = true
	return true
}

func (e *Engine) releaseVolumeSlot(appID string) {
	e.volMu.Lock()
	delete(e.volBusy, appID)
	e.volMu.Unlock()
}

// VolumeBusy reports whether a backup/restore is in flight for an app.
func (e *Engine) VolumeBusy(appID string) bool {
	e.volMu.Lock()
	defer e.volMu.Unlock()
	return e.volBusy[appID]
}

// SetVolumeStore injects the app volume archive metadata store.
func (e *Engine) SetVolumeStore(vs *VolumeStore) {
	e.vols = vs
}

// SetVolumeUploadStore injects the panel-side upload staging store for
// upload-restores (volupload.go).
func (e *Engine) SetVolumeUploadStore(us *VolumeUploadStore) {
	e.volUploads = us
}

// VolumeArchives lists an application's volume archives (nil store → empty).
func (e *Engine) VolumeArchives(appID string) []*VolumeArchive {
	if e.vols == nil {
		return nil
	}
	return e.vols.ListArchives(appID)
}

// GetVolumeArchive returns one archive of an application.
func (e *Engine) GetVolumeArchive(appID, archiveID string) (*VolumeArchive, bool) {
	if e.vols == nil {
		return nil, false
	}
	return e.vols.GetArchive(appID, archiveID)
}

// ---- snapshot job bridge (snapshots.AppVolumeHost) ----
//
// 快照备份的 app_volume 任务把「定义 / 定时 / 保留份数 / 统一归档历史」交给快照
// 模块，执行语义仍在本引擎：声明卷白名单、S3 唯一存储、SHA 校验与安全停机恢复。
// 同一条归档记录（VolumeStore）同时出现在应用详情页与快照任务历史里。

// ResolveAppVolume returns the application's backupable volumes for snapshot
// job create/update validation.
func (e *Engine) ResolveAppVolume(appID string) (*snapshots.AppVolumeTarget, error) {
	app, ok := e.store.GetApp(appID)
	if !ok {
		return nil, fmt.Errorf("应用不存在")
	}
	if !volumeBackupSupported(app) {
		return nil, fmt.Errorf("Compose 应用的挂载卷在编排文件中声明，暂不支持应用级备份")
	}
	if e.hostIsWindows(app.HostID) {
		return nil, fmt.Errorf("数据卷备份暂仅支持 Linux 主机")
	}
	vols := make([]string, 0, len(app.Volumes))
	for _, v := range app.Volumes {
		if v = strings.TrimSpace(v); v != "" {
			vols = append(vols, v)
		}
	}
	if len(vols) == 0 {
		return nil, fmt.Errorf("应用未配置任何挂载卷")
	}
	return &snapshots.AppVolumeTarget{AppID: app.ID, Name: app.Name, HostID: app.HostID, Volumes: vols}, nil
}

// RunAppVolumeBackup executes one snapshot job run. Preflight order mirrors the
// HTTP front door: volume slot (synchronous) → deploy check → agent online.
// The spec is the job target: "ALL" runs against every declared volume, any
// other value is the comma-joined selection (a bare spec = single volume).
func (e *Engine) RunAppVolumeBackup(appID, spec, note, storageTarget string, retention int) error {
	app, ok := e.store.GetApp(appID)
	if !ok {
		return fmt.Errorf("应用不存在")
	}
	if !volumeBackupSupported(app) {
		return fmt.Errorf("Compose 应用的挂载卷在编排文件中声明，暂不支持应用级备份")
	}
	if e.hostIsWindows(app.HostID) {
		return fmt.Errorf("数据卷备份暂仅支持 Linux 主机")
	}
	specs, all := snapshots.ParseAppVolumeSpecList(spec)
	if !all {
		if len(specs) == 0 {
			return fmt.Errorf("应用数据卷备份必须选择挂载卷")
		}
		for _, s := range specs {
			declared := false
			for _, v := range app.Volumes {
				if strings.TrimSpace(v) == s {
					declared = true
					break
				}
			}
			if !declared {
				return fmt.Errorf("只能备份应用已声明的挂载卷: %s", s)
			}
		}
	} else {
		// "ALL" follows the declared set at run time — volumes added after the
		// job was created are picked up automatically.
		specs = nil
	}
	target, err := e.s3TargetForRef(storageTarget)
	if err != nil {
		return err
	}
	if !e.acquireVolumeSlot(app.ID) {
		return fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)
	if e.IsRunning(app.ID) {
		return fmt.Errorf("应用正在部署，暂不能备份数据卷")
	}
	if e.volumeHub(app) == nil {
		return fmt.Errorf("目标主机 Agent 不在线")
	}
	if retention <= 0 {
		retention = volumeRetention
	}
	_, err = e.runBackupAppVolumesWith(app, specs, note, target, retention)
	return err
}

// ListAppVolumeArchives exposes one app's archive records to the snapshot job
// history. The filter is the job's volume selection: "ALL" matches snapshots
// of the whole declared set, a comma list matches snapshots of exactly that
// selection — so each job's history keeps its own runs even when other jobs
// (or manual backups) cover different volumes of the same app.
func (e *Engine) ListAppVolumeArchives(appID, spec string) []*snapshots.AppVolumeArchive {
	if e.vols == nil {
		return nil
	}
	spec = strings.TrimSpace(spec)
	var want map[string]bool
	if spec != "" {
		specs, all := snapshots.ParseAppVolumeSpecList(spec)
		if all {
			// "ALL" resolves against the declared set at query time: snapshots
			// taken before a volume was added simply age out of this job's
			// history (they stay visible in the app detail tab).
			app, ok := e.store.GetApp(appID)
			if !ok {
				return []*snapshots.AppVolumeArchive{}
			}
			specs = make([]string, 0, len(app.Volumes))
			for _, v := range app.Volumes {
				if v = strings.TrimSpace(v); v != "" {
					specs = append(specs, v)
				}
			}
		}
		want = make(map[string]bool, len(specs))
		for _, s := range specs {
			want[s] = true
		}
	}
	out := make([]*snapshots.AppVolumeArchive, 0, 8)
	for _, a := range e.vols.ListArchives(appID) {
		if want != nil && !appVolumeArchiveMatchesSelection(a, want) {
			continue
		}
		out = append(out, &snapshots.AppVolumeArchive{
			ID:                a.ID,
			HostID:            a.HostID,
			VolumeSpec:        a.VolumeSpec,
			StorageTarget:     a.StorageTarget,
			StorageTargetName: a.StorageTargetName,
			StoragePath:       a.StoragePath,
			Size:              a.Size,
			SHA256:            a.SHA256,
			Status:            a.Status,
			Source:            a.Source,
			Error:             a.Error,
			Note:              a.Note,
			CreatedAt:         a.CreatedAt,
			LastRestoreAt:     a.LastRestoreAt,
			LastRestoreStatus: a.LastRestoreStatus,
			LastRestoreError:  a.LastRestoreError,
		})
	}
	return out
}

// appVolumeArchiveMatchesSelection reports whether the archive snapshots
// exactly the job's requested volume selection (order-free). The item list is
// the authority; records without items (plain upload-restore archives) fall
// back to their single spec label so they keep appearing in a matching job's
// history.
func appVolumeArchiveMatchesSelection(a *VolumeArchive, want map[string]bool) bool {
	if len(want) == 0 {
		return false
	}
	specs := make([]string, 0, len(a.Volumes)+1)
	for _, it := range a.Volumes {
		if s := strings.TrimSpace(it.VolumeSpec); s != "" {
			specs = append(specs, s)
		}
	}
	if len(specs) == 0 {
		if s := strings.TrimSpace(a.VolumeSpec); s != "" && s != snapshots.AppVolumeTargetAll {
			specs = []string{s}
		}
	}
	if len(specs) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(specs))
	for _, s := range specs {
		if !want[s] || seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

// recordVolumeRestoreFailure stamps a failed outcome onto the archive and
// returns err unchanged. The snapshot history answers 202 before the restore
// runs, so without this write-back a preflight rejection (busy/deploying/
// offline) would vanish into the server log while the panel showed nothing.
func (e *Engine) recordVolumeRestoreFailure(arch *VolumeArchive, err error) error {
	if arch != nil && e.vols != nil {
		now := time.Now()
		arch.LastRestoreAt = &now
		arch.LastRestoreStatus = VolumeRestoreFailed
		arch.LastRestoreError = err.Error()
		if perr := e.vols.PutArchive(arch); perr != nil {
			e.log.Warn("record volume restore result failed", "archive", arch.ID, "err", perr)
		}
	}
	return err
}

// CheckAppVolumeRestore reports why a restore cannot start right now without
// doing any work (snapshots.AppVolumeHost). The snapshot history HTTP layer
// calls it synchronously so a doomed restore is rejected up front (409/503)
// instead of answering 202 and silently logging the failure later. Retryable
// conflicts return the snapshots.ErrVolumeRestore* sentinels.
func (e *Engine) CheckAppVolumeRestore(appID string) error {
	app, ok := e.store.GetApp(strings.TrimSpace(appID))
	if !ok {
		return fmt.Errorf("应用不存在")
	}
	if !volumeBackupSupported(app) {
		return fmt.Errorf("Compose 应用的挂载卷在编排文件中声明，暂不支持应用级备份")
	}
	if e.hostIsWindows(app.HostID) {
		return fmt.Errorf("数据卷恢复暂仅支持 Linux 主机")
	}
	if e.VolumeBusy(app.ID) {
		return snapshots.ErrVolumeRestoreBusy
	}
	if e.IsRunning(app.ID) {
		return snapshots.ErrVolumeRestoreDeploying
	}
	if e.volumeHub(app) == nil {
		return snapshots.ErrVolumeRestoreOffline
	}
	return nil
}

// RestoreAppVolumeByID restores an archive chosen from the snapshot history.
func (e *Engine) RestoreAppVolumeByID(appID, archiveID string) error {
	app, ok := e.store.GetApp(appID)
	if !ok {
		return fmt.Errorf("应用不存在")
	}
	if !volumeBackupSupported(app) {
		return fmt.Errorf("Compose 应用的挂载卷在编排文件中声明，暂不支持应用级备份")
	}
	if e.hostIsWindows(app.HostID) {
		return fmt.Errorf("数据卷恢复暂仅支持 Linux 主机")
	}
	arch, ok := e.GetVolumeArchive(appID, archiveID)
	if !ok {
		return fmt.Errorf("归档记录不存在")
	}
	if arch.Status != VolumeBackupSuccess {
		return fmt.Errorf("归档未成功完成，不能用于恢复")
	}
	// Preflight rejections below are recorded on the archive too: the HTTP
	// layer may have raced (state changed between its synchronous check and
	// this goroutine) and the operator must see why nothing happened.
	if !e.acquireVolumeSlot(app.ID) {
		return e.recordVolumeRestoreFailure(arch, snapshots.ErrVolumeRestoreBusy)
	}
	defer e.releaseVolumeSlot(app.ID)
	if e.IsRunning(app.ID) {
		return e.recordVolumeRestoreFailure(arch, snapshots.ErrVolumeRestoreDeploying)
	}
	if e.volumeHub(app) == nil {
		return e.recordVolumeRestoreFailure(arch, snapshots.ErrVolumeRestoreOffline)
	}
	_, err := e.runRestoreAppVolume(app, arch, RestoreVolumeOptions{CleanRestore: true})
	return err
}

// DeleteAppVolumeByID removes an archive chosen from the snapshot history.
func (e *Engine) DeleteAppVolumeByID(appID, archiveID string) error {
	app, ok := e.store.GetApp(appID)
	if !ok {
		return fmt.Errorf("应用不存在")
	}
	arch, ok := e.GetVolumeArchive(appID, archiveID)
	if !ok {
		return fmt.Errorf("归档记录不存在")
	}
	if arch.Status == VolumeBackupRunning {
		return fmt.Errorf("备份任务执行中，不能删除该归档")
	}
	if !e.acquireVolumeSlot(app.ID) {
		return fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)
	return e.deleteAppVolumeArchive(app, arch)
}

// s3TargetForRef maps a snapshot job's storage_target to a destination. App
// volume archives are S3-only (the host keeps no copy), so "local" is rejected.
func (e *Engine) s3TargetForRef(ref string) (*snapshots.S3Target, error) {
	switch strings.TrimSpace(ref) {
	case "", "default":
		return e.defaultS3Target()
	case "local":
		return nil, fmt.Errorf("应用数据卷归档仅支持 S3 存储，请选择默认或指定 S3 目标")
	default:
		t, err := e.s3TargetByID(strings.TrimSpace(ref))
		if err != nil {
			return nil, fmt.Errorf("所选 S3 存储目标不存在")
		}
		return t, nil
	}
}

// hostIsWindows mirrors the HTTP front door's check: the volume scripts are
// POSIX (tar / set -e).
func (e *Engine) hostIsWindows(hostID string) bool {
	if e.reg == nil {
		return false
	}
	ag := e.reg.GetAgent(hostID)
	return ag != nil && strings.EqualFold(ag.OS, "windows")
}
