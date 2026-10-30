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
)

// VolumeArchive is one tar.gz backup of an application volume. The object is
// stored in S3; ID carries the "vbk_" prefix.
type VolumeArchive struct {
	ID            string `json:"id"`
	AppID         string `json:"app_id"`
	HostID        string `json:"host_id"`
	VolumeSpec    string `json:"volume_spec"`    // original "host:container[:ro]" spec
	HostPath      string `json:"host_path"`      // resolved data dir (bind path or volume mountpoint)
	ContainerPath string `json:"container_path"` // path inside the container
	// StorageTarget/StorageTargetName are frozen at backup time: switching the
	// default target later must not orphan older archives.
	StorageTarget     string `json:"storage_target"`
	StorageTargetName string `json:"storage_target_name,omitempty"`
	StoragePath       string `json:"storage_path"` // full S3 object key, target prefix included
	Size              int64  `json:"size"`         // bytes
	SHA256            string `json:"sha256,omitempty"`
	Status            string `json:"status"` // running | success | failed | uploaded
	// Source distinguishes panel backups ("", historical) from records
	// created by an upload-restore ("upload"): the latter have no S3 object
	// and their bytes live only as a temporary panel upload until restored.
	Source    string    `json:"source,omitempty"`
	Error     string    `json:"error,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`

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
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
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

// volumeTarProbeScript lists a tar.gz without extracting it: an upload with
// no operator-supplied SHA is still rejected before any downtime when it is
// not a gzip tar at all.
func volumeTarProbeScript(localPath string) string {
	return fmt.Sprintf("tar -tzf %s >/dev/null", shellQuote(localPath))
}

// volumeRestoreScript takes a safety copy of the current data, then unpacks the
// archive over it. The safety copy is best-effort (a fresh volume has nothing
// to preserve but the restore itself must not be blocked by that).
func volumeRestoreScript(hostDir, archivePath, safetyPath string) string {
	return fmt.Sprintf(`set -e
[ -f %s ] || { echo "归档文件不存在: %s" >&2; exit 1; }
mkdir -p %s
tar -C %s -czf %s . 2>/dev/null || true
tar -C %s -xzf %s`,
		shellQuote(archivePath), archivePath,
		shellQuote(hostDir),
		shellQuote(hostDir), shellQuote(safetyPath),
		shellQuote(hostDir), shellQuote(archivePath))
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

// resolveVolumeBackupTarget picks the destination for a manual backup,
// Dokploy-style: an explicit target_id wins, an omitted one falls back to
// the default target. Snapshot jobs bypass this — they pass their own target
// to runBackupAppVolumesWith directly.
func (e *Engine) resolveVolumeBackupTarget(targetID string) (*snapshots.S3Target, error) {
	if id := strings.TrimSpace(targetID); id != "" {
		t, err := e.s3TargetByID(id)
		if err != nil {
			return nil, errVolumeTargetUnknown
		}
		return t, nil
	}
	t, err := e.defaultS3Target()
	if err != nil {
		return nil, errVolumeTargetNone
	}
	return t, nil
}

// deleteVolumeObject removes one archive's S3 object. A record created before
// the S3 migration (empty path) has nothing to delete.
func (e *Engine) deleteVolumeObject(arch *VolumeArchive) error {
	if arch == nil || arch.StoragePath == "" {
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
// straight from S3 (the control plane only issues the redirect).
func (e *Engine) VolumeArchiveDownloadURL(arch *VolumeArchive, expires time.Duration) (string, error) {
	if arch == nil || arch.StoragePath == "" {
		return "", fmt.Errorf("归档缺少 S3 存储信息")
	}
	target, err := e.s3TargetByID(arch.StorageTarget)
	if err != nil {
		return "", err
	}
	u, err := snapshots.NewS3Client(target).PresignGet(arch.StoragePath, expires)
	if err != nil {
		return "", fmt.Errorf("生成 S3 下载地址失败: %w", err)
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

// BackupAppVolumes archives one or more volumes of an application. specs empty
// means every declared volume. Volumes run serially: each holds a busy slot on
// the host's staging area and a parallel tar of the same app adds no value.
func (e *Engine) BackupAppVolumes(app *Application, specs []string, note string) ([]*VolumeArchive, error) {
	if !e.acquireVolumeSlot(app.ID) {
		return nil, fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)
	return e.runBackupAppVolumes(app, specs, note)
}

// runBackupAppVolumes assumes the caller already holds the app's volume slot.
// The HTTP front door acquires it synchronously so a double-clicked request is
// rejected with 409 instead of two goroutines racing for the slot.
func (e *Engine) runBackupAppVolumes(app *Application, specs []string, note string) ([]*VolumeArchive, error) {
	target, err := e.defaultS3Target()
	if err != nil {
		return nil, err
	}
	return e.runBackupAppVolumesWith(app, specs, note, target, volumeRetention)
}

// runBackupAppVolumesWith backs the same flow with an explicit S3 target and
// retention count, which is what a snapshot job provides. The caller must hold
// the app's volume slot.
func (e *Engine) runBackupAppVolumesWith(app *Application, specs []string, note string, target *snapshots.S3Target, keep int) ([]*VolumeArchive, error) {
	// A deployment is rewriting the container and may be about to recreate the
	// volume mounts; tarring concurrently produces an inconsistent archive.
	if e.IsRunning(app.ID) {
		return nil, fmt.Errorf("应用正在部署，暂不能备份数据卷")
	}

	list := make([]string, 0, len(specs))
	if len(specs) == 0 {
		list = append(list, app.Volumes...)
	} else {
		list = append(list, specs...)
	}
	seen := make(map[string]bool, len(list))
	deduped := make([]string, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		deduped = append(deduped, s)
	}
	if len(deduped) == 0 {
		return nil, fmt.Errorf("应用未配置任何挂载卷")
	}
	// Fail before any agent work when there is nowhere to store the archive.
	if target == nil {
		return nil, fmt.Errorf("未配置默认 S3 存储目标，请先在「快照备份 → S3 存储」中添加目标并设为默认")
	}

	hub := e.volumeHub(app)
	if hub == nil {
		return nil, fmt.Errorf("目标主机 Agent 不在线")
	}
	out := make([]*VolumeArchive, 0, len(deduped))
	var firstErr error
	for _, spec := range deduped {
		arch, err := e.backupAppVolume(hub, app, spec, note, target)
		if arch != nil {
			out = append(out, arch)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Retention is per volume and must run even when another volume failed. A
	// partial multi-volume backup still creates successful archives that need
	// pruning; otherwise failed runs can permanently bypass the retention cap.
	for _, spec := range deduped {
		e.pruneVolumeArchives(hub, app.ID, spec, keep)
	}
	if firstErr != nil {
		return out, firstErr
	}
	return out, nil
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
		if a.VolumeSpec != spec || a.Status != VolumeBackupSuccess {
			continue
		}
		kept++
		if kept > keep {
			stale = append(stale, a)
		}
	}
	return stale
}

// pruneVolumeArchives keeps the newest keep successful archives of one spec and
// deletes the rest. Metadata is only dropped after the S3 object is gone (or
// its destination no longer exists): a transient S3 failure must not orphan an
// object that no record points at any more.
func (e *Engine) pruneVolumeArchives(hub *rpc.Hub, appID, spec string, keep int) {
	if e.vols == nil {
		return
	}
	stale := staleVolumeArchives(e.vols.ListArchives(appID), spec, keep)
	for _, a := range stale {
		err := e.deleteVolumeObject(a)
		if err != nil && !errors.Is(err, errVolumeS3TargetMissing) {
			e.log.Warn("volume archive prune skipped: S3 delete failed", "app", appID, "spec", spec, "archive", a.ID, "err", err)
			continue
		}
		if err != nil {
			e.log.Warn("volume archive prune drops record of deleted S3 target", "app", appID, "archive", a.ID)
		}
		e.cleanupHostPaths(hub, volumeStagingPath(appID, a.ID), volumeRestorePath(appID, a.ID), volumeSafetyPath(appID, a.ID))
		if err := e.vols.DeleteArchive(appID, a.ID); err != nil {
			e.log.Warn("volume archive metadata delete failed", "app", appID, "archive", a.ID, "err", err)
		}
	}
}

// RestoreAppVolume unpacks an archive back onto its volume. The app container
// is stopped first when running (a live container holds file locks and keeps
// writing), restored, then started again — a stop that fails aborts the
// restore so live data is never overwritten underneath a running service.
func (e *Engine) RestoreAppVolume(app *Application, arch *VolumeArchive) (restarted bool, err error) {
	if !e.acquireVolumeSlot(app.ID) {
		return false, snapshots.ErrVolumeRestoreBusy
	}
	defer e.releaseVolumeSlot(app.ID)
	return e.runRestoreAppVolume(app, arch)
}

// resolveRestoreVolumeDir validates that the archive's volume spec is still
// declared on the app (trim-exact match, the same rule the backup front door
// enforces) and returns the host directory the archive must be unpacked into.
// Bind mounts compare against the recorded path (the declared source is the
// path); named volumes are re-resolved through docker volume inspect so a
// recreated volume's new mountpoint is caught instead of blindly trusting the
// recorded path. Any mismatch means the volume configuration changed since
// the backup was taken and the archive must not be unpacked anywhere.
// The caller must have verified the hub is online: named-volume resolution
// executes on the agent (bind mounts return before any agent call).
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

// recordVolumeRestoreOutcome writes the restore result back to the archive:
// the HTTP layer answers 202 and the operator's only way to learn the result
// is polling this record.
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

// runRestoreAppVolume restores an S3-backed archive: S3-specific preflight
// (complete success record, frozen target, mandatory SHA) plus a presigned
// download, then the shared stop/restore core. The caller already holds the
// app's volume slot.
func (e *Engine) runRestoreAppVolume(app *Application, arch *VolumeArchive) (restarted bool, err error) {
	defer func() { e.recordVolumeRestoreOutcome(app, arch, err) }()
	// Offline wins over every record-level complaint (same precedence as
	// before the core split): a transient outage must not mask itself as a
	// permanent record defect. The core re-checks for the race where the
	// host drops between spawn and execution.
	if e.volumeHub(app) == nil {
		return false, snapshots.ErrVolumeRestoreOffline
	}
	if arch == nil || arch.StoragePath == "" || arch.HostPath == "" {
		return false, fmt.Errorf("归档记录不完整（缺少 S3 存储信息）")
	}
	if arch.Status != VolumeBackupSuccess {
		return false, fmt.Errorf("归档未成功完成，不能用于恢复")
	}
	if arch.HostID != app.HostID {
		return false, fmt.Errorf("归档所属主机与应用不一致")
	}
	if arch.SHA256 == "" {
		// A success record without a SHA cannot be verified after download,
		// so refusing before any downtime is the only safe option. Such
		// records can only exist from backups taken before marker validation
		// was enforced in backupAppVolume — re-backup instead of trusting them.
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
	return e.runRestoreCore(app, arch, getURL, arch.SHA256, "从 S3 下载归档失败")
}

// runRestoreUploadedVolume restores a record created by an upload-restore:
// the bytes come from the panel's one-time download URL instead of S3, and
// the SHA check is optional (only when the operator supplied one); without
// it the download is probed as a gzip tar before any downtime. The panel
// temporary file and its download token are dropped when the run ends,
// whatever the outcome — a failed run is retried by re-uploading. The caller
// already holds the app's volume slot.
func (e *Engine) runRestoreUploadedVolume(app *Application, arch *VolumeArchive, downloadURL, expectedSHA string) (restarted bool, err error) {
	defer func() { e.recordVolumeRestoreOutcome(app, arch, err) }()
	if arch == nil || arch.HostPath == "" {
		return false, fmt.Errorf("上传恢复记录不完整，无法恢复")
	}
	// Registered after the nil check: the cleanup below dereferences arch.
	defer func() {
		if e.volUploads != nil {
			e.volUploads.RemoveFile(arch.ID)
			e.volUploads.DropTokenByArchive(arch.ID)
		}
	}()
	if arch.HostID != app.HostID {
		return false, fmt.Errorf("归档所属主机与应用不一致")
	}
	return e.runRestoreCore(app, arch, downloadURL, expectedSHA, "从面板下载归档失败")
}

// runRestoreCore is the shared tail of both restore paths: volume-dir
// resolution, deploy mutual exclusion, download + verification, then
// stop → safety copy → unpack → restart. Download/verification failures
// never cost downtime: the container is only touched after they pass.
func (e *Engine) runRestoreCore(app *Application, arch *VolumeArchive, downloadURL, expectedSHA, dlFail string) (restarted bool, err error) {
	hub := e.volumeHub(app)
	if hub == nil {
		return false, snapshots.ErrVolumeRestoreOffline
	}
	// The archive must still point at a volume the app declares: restoring
	// into a path the app no longer owns would overwrite another
	// application's data (e.g. the bind path was re-pointed after the backup
	// was taken). Named volumes are re-resolved through docker volume inspect
	// rather than trusting the recorded mountpoint.
	restoreDir, err := e.resolveRestoreVolumeDir(hub, app, arch)
	if err != nil {
		return false, err
	}
	// The deployment pipeline recreates the container; stopping it underneath
	// the pipeline would make both operations fight over the same app.
	if e.IsRunning(app.ID) {
		return false, snapshots.ErrVolumeRestoreDeploying
	}
	container := app.ContainerName
	if container == "" {
		return false, fmt.Errorf("应用尚未部署过，无法恢复数据卷")
	}

	// Download and verify before touching the container: a missing or corrupt
	// object must not cost any downtime.
	staging := volumeRestorePath(app.ID, arch.ID)
	defer e.cleanupHostPaths(hub, staging)
	dlRes, dlErr := execOnAgent(hub, "voldl-"+arch.ID, volumeDownloadScript(downloadURL, staging), volumeS3OpTimeout)
	if dlErr != nil {
		return false, fmt.Errorf("%s: %v", dlFail, dlErr)
	}
	if dlRes.GetExitCode() != 0 {
		msg := strings.TrimSpace(string(dlRes.GetStderr()))
		if msg == "" {
			msg = fmt.Sprintf("%s (exit %d)", dlFail, dlRes.GetExitCode())
		}
		return false, fmt.Errorf("%s", msg)
	}
	if expectedSHA != "" {
		// S3 restores always carry a SHA (the entry preflight enforces it);
		// upload restores verify only when the operator supplied one.
		shaRes, shaErr := execOnAgent(hub, "volsha-"+arch.ID, volumeSHA256Script(staging), 120)
		if shaErr != nil {
			return false, fmt.Errorf("校验下载归档失败: %v", shaErr)
		}
		if got := strings.TrimSpace(string(shaRes.GetStdout())); got != expectedSHA {
			return false, fmt.Errorf("归档校验失败：SHA256 不匹配（期望 %s，实际 %s）", expectedSHA, got)
		}
	} else {
		// No digest to compare against: at least prove the bytes are a gzip
		// tar before stopping the container for them.
		probeRes, probeErr := execOnAgent(hub, "volprobe-"+arch.ID, volumeTarProbeScript(staging), 120)
		if probeErr != nil {
			return false, fmt.Errorf("校验上传文件失败: %v", probeErr)
		}
		if probeRes.GetExitCode() != 0 {
			msg := strings.TrimSpace(string(probeRes.GetStderr()))
			if msg == "" {
				msg = "不是有效的 tar.gz 归档"
			}
			return false, fmt.Errorf("上传文件校验失败：%s", msg)
		}
	}

	stateRes, serr := execOnAgent(hub, "volst-"+randomToken(4), volumeContainerStateScript(container), 30)
	if serr != nil {
		return false, fmt.Errorf("查询容器状态失败: %v", serr)
	}
	if stateRes.GetExitCode() != 0 {
		msg := strings.TrimSpace(string(stateRes.GetStderr()))
		if msg == "" {
			msg = fmt.Sprintf("Agent 返回状态检查失败 (exit %d)", stateRes.GetExitCode())
		}
		return false, fmt.Errorf("查询容器状态失败: %s", msg)
	}
	wasRunning, stateErr := parseVolumeContainerState(stateRes.GetStdout())
	if stateErr != nil {
		return false, fmt.Errorf("查询容器状态失败: %v", stateErr)
	}

	if wasRunning {
		stopRes, stopErr := execOnAgent(hub, "volstop-"+randomToken(4), "docker stop "+shellQuote(container), 300)
		if stopErr != nil {
			return false, fmt.Errorf("停止应用容器失败: %v", stopErr)
		}
		if stopRes.GetExitCode() != 0 {
			return false, fmt.Errorf("停止应用容器失败(exit %d): %s", stopRes.GetExitCode(), strings.TrimSpace(string(stopRes.GetStderr())))
		}
		// Structural guarantee: once stopped, every return path below restarts
		// the container before the slot is released (a process crash between
		// the two calls can still leave it stopped — documented in AGENTS.md).
		defer func() {
			startRes, startErr := execOnAgent(hub, "volstart-"+randomToken(4), "docker start "+shellQuote(container), 120)
			if startErr != nil {
				restartErr := fmt.Errorf("恢复后重启应用容器失败: %v", startErr)
				e.log.Error("volume restore: container restart failed", "app", app.ID, "container", container, "err", restartErr)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				return
			}
			if startRes.GetExitCode() != 0 {
				msg := strings.TrimSpace(string(startRes.GetStderr()))
				if msg == "" {
					msg = fmt.Sprintf("exit %d", startRes.GetExitCode())
				}
				restartErr := fmt.Errorf("恢复后重启应用容器失败: %s", msg)
				e.log.Error("volume restore: container restart exited non-zero", "app", app.ID, "container", container, "exit", startRes.GetExitCode(), "err", restartErr)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				return
			}
			// docker start may return zero and still leave a container that exits
			// immediately. Check the resulting state; failure is part of the
			// restore outcome rather than a warning-only condition.
			checkRes, checkErr := execOnAgent(hub, "volstate-"+randomToken(4), volumeContainerStateScript(container), 30)
			if checkErr != nil {
				restartErr := fmt.Errorf("恢复后检查应用容器状态失败: %v", checkErr)
				e.log.Error("volume restore: container state check failed", "app", app.ID, "container", container, "err", restartErr)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				return
			}
			if checkRes.GetExitCode() != 0 {
				msg := strings.TrimSpace(string(checkRes.GetStderr()))
				if msg == "" {
					msg = fmt.Sprintf("exit %d", checkRes.GetExitCode())
				}
				restartErr := fmt.Errorf("恢复后检查应用容器状态失败: %s", msg)
				e.log.Error("volume restore: container state check exited non-zero", "app", app.ID, "container", container, "exit", checkRes.GetExitCode(), "err", restartErr)
				if err == nil {
					err = restartErr
				} else {
					err = fmt.Errorf("%v；%w", err, restartErr)
				}
				return
			}
			running, parseErr := parseVolumeContainerState(checkRes.GetStdout())
			if parseErr != nil || !running {
				if parseErr == nil {
					parseErr = errors.New("容器启动后立即退出")
				}
				restartErr := fmt.Errorf("恢复后应用容器未保持运行: %v", parseErr)
				e.log.Error("volume restore: container exited after restart", "app", app.ID, "container", container, "err", restartErr)
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

	res, rerr := execOnAgent(hub, "volre-"+arch.ID,
		volumeRestoreScript(restoreDir, staging, volumeSafetyPath(app.ID, arch.ID)), volumeS3OpTimeout)
	if rerr != nil {
		return restarted, fmt.Errorf("恢复执行失败: %v", rerr)
	}
	if res.GetExitCode() != 0 {
		return restarted, fmt.Errorf("恢复失败(exit %d): %s", res.GetExitCode(), strings.TrimSpace(string(res.GetStderr())))
	}
	e.log.Info("volume restored", "app", app.ID, "archive", arch.ID, "restarted", restarted)
	return restarted, nil
}

// DeleteAppVolumeArchive removes an archive's S3 object and metadata, plus the
// host-side staging/safety files when the agent is reachable. A transient S3
// failure keeps the record so the operator can retry; a deleted S3 target
// cannot ever be cleaned, so its record is dropped with a warning.
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
	err := e.deleteVolumeObject(arch)
	if err != nil && !errors.Is(err, errVolumeS3TargetMissing) {
		return err
	}
	if err != nil {
		e.log.Warn("volume archive S3 target gone, dropping record", "app", app.ID, "archive", arch.ID)
	}
	// Uploaded records have no S3 object (deleteVolumeObject already no-ops
	// on the empty StoragePath); their bytes are the panel's temporary file
	// plus its download token, dropped here when the operator deletes the
	// record instead of restoring it.
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
	spec = strings.TrimSpace(spec)
	declared := false
	for _, v := range app.Volumes {
		if strings.TrimSpace(v) == spec {
			declared = true
			break
		}
	}
	if !declared {
		return fmt.Errorf("只能备份应用已声明的挂载卷: %s", spec)
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
	_, err = e.runBackupAppVolumesWith(app, []string{spec}, note, target, retention)
	return err
}

// ListAppVolumeArchives exposes one app's archive records to the snapshot job
// history (filtered by the job's volume spec).
func (e *Engine) ListAppVolumeArchives(appID, spec string) []*snapshots.AppVolumeArchive {
	if e.vols == nil {
		return nil
	}
	spec = strings.TrimSpace(spec)
	out := make([]*snapshots.AppVolumeArchive, 0, 8)
	for _, a := range e.vols.ListArchives(appID) {
		if spec != "" && a.VolumeSpec != spec {
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
	_, err := e.runRestoreAppVolume(app, arch)
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
