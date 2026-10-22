package apps

// 应用挂载卷备份与恢复（2026-10-22）。
//
// 每个应用声明的卷（Application.Volumes，docker -v 规格）是业务数据的落点，
// 平台此前只有删除应用时才接触它们（purge 时区分 bind mount 与具名卷），既不能
// 备份也不能找回。本文件补齐卷数据生命周期：把卷的数据目录打成 tar.gz 归档，
// 直存应用所在主机的 /var/backups/watchman/apps/<appID>/（与快照引擎同一根目录、
// 同一 direct-to-storage 原则——GB 级归档过控制端会打满上行带宽），元数据落控制端
// dataDir 下的 app_volume_archives.json，供详情页列举、恢复与删除。
//
// 卷规格两种形态都支持：
//   - bind mount：/opt/apps/xxx/data:/data —— 直接 tar 宿主机路径；
//   - Docker 具名卷：volname:/data —— 先 docker volume inspect 解析挂载点再 tar
//     （与 purge.go「src 含 / 即宿主机目录，绝不误删」的区分规则同源）。
//
// 恢复遵循安全停机语义：先 docker stop 应用容器防止在线写盘与文件锁，安全副本
// 后解压覆盖，最后无论成败都把容器拉回原状态（先停必起，绝不在线服务停在停止态）。

import (
	"encoding/json"
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
)

const (
	// volumeBackupRoot is where app volume archives live on the managed host.
	// Same root as the snapshot engine (/var/backups/watchman) so operators
	// look in one place; per-app sub-dirs keep archive names from colliding.
	volumeBackupRoot = "/var/backups/watchman/apps"

	// volumeRetention keeps the newest N archives per volume spec (host files
	// plus their metadata). Mirrors the snapshot engine's default so the two
	// backup surfaces behave alike.
	volumeRetention = 5

	// volumeArchiveMetaCap bounds stored metadata per application; the host
	// retention prune keeps this far below the cap in normal operation.
	volumeArchiveMetaCap = 100

	// appVolumeArchivesFile persists archive metadata under the data dir. A
	// full-file atomic rewrite (not append-only JSONL) because a record
	// migrates running -> success/failed after its agent run returns.
	appVolumeArchivesFile = "app_volume_archives.json"

	// Archive lifecycle states. Running is visible to the UI (polled while the
	// agent tar runs); failed keeps its record so the operator can see why.
	VolumeBackupRunning = "running"
	VolumeBackupSuccess = "success"
	VolumeBackupFailed  = "failed"
)

// VolumeArchive is one tar.gz backup of an application volume's data directory
// on the application host. ID carries the "vbk_" prefix.
type VolumeArchive struct {
	ID            string    `json:"id"`
	AppID         string    `json:"app_id"`
	HostID        string    `json:"host_id"`
	VolumeSpec    string    `json:"volume_spec"`    // original "host:container[:ro]" spec
	HostPath      string    `json:"host_path"`      // resolved data dir (bind path or volume mountpoint)
	ContainerPath string    `json:"container_path"` // path inside the container
	FilePath      string    `json:"file_path"`      // /var/backups/watchman/apps/<appID>/<id>.tar.gz
	Size          int64     `json:"size"`           // bytes
	SHA256        string    `json:"sha256,omitempty"`
	Status        string    `json:"status"` // running | success | failed
	Error         string    `json:"error,omitempty"`
	Note          string    `json:"note,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
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
		// Corrupt metadata must not block booting: start empty, the host-side
		// archives remain discoverable through the detail view's next backup.
		s.log.Warn("app_volume_archives.json corrupted, starting empty", "err", err)
		return s, nil
	}
	for _, a := range doc.Archives {
		if a == nil || a.ID == "" {
			continue
		}
		s.byID[a.AppID+"|"+a.ID] = a
	}
	return s, nil
}

func volumeArchiveKey(appID, id string) string { return appID + "|" + id }

// AddArchive records a freshly created archive (status running).
func (s *VolumeStore) AddArchive(a *VolumeArchive) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[volumeArchiveKey(a.AppID, a.ID)] = a
	return s.saveLocked()
}

// PutArchive updates a stored record (status migration, size/sha, error).
func (s *VolumeStore) PutArchive(a *VolumeArchive) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[volumeArchiveKey(a.AppID, a.ID)] = a
	return s.saveLocked()
}

// GetArchive returns one archive of an application.
func (s *VolumeStore) GetArchive(appID, id string) (*VolumeArchive, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.byID[volumeArchiveKey(appID, id)]
	return a, ok
}

// ListArchives returns an application's archives, newest first.
func (s *VolumeStore) ListArchives(appID string) []*VolumeArchive {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*VolumeArchive, 0, 8)
	for _, a := range s.byID {
		if a.AppID == appID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// DeleteArchive drops one archive record.
func (s *VolumeStore) DeleteArchive(appID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, volumeArchiveKey(appID, id))
	return s.saveLocked()
}

func (s *VolumeStore) saveLocked() error {
	doc := appVolumeArchives{Archives: make([]*VolumeArchive, 0, len(s.byID))}
	for _, a := range s.byID {
		doc.Archives = append(doc.Archives, a)
	}
	sort.Slice(doc.Archives, func(i, j int) bool { return doc.Archives[i].CreatedAt.After(doc.Archives[j].CreatedAt) })
	// Metadata cap per app: drop the oldest overflow so a long-lived app cannot
	// grow the file without bound (host archives are pruned separately).
	perApp := make(map[string]int)
	kept := make([]*VolumeArchive, 0, len(doc.Archives))
	for _, a := range doc.Archives {
		perApp[a.AppID]++
		if perApp[a.AppID] > volumeArchiveMetaCap {
			continue
		}
		kept = append(kept, a)
	}
	doc.Archives = kept
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, appVolumeArchivesFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, appVolumeArchivesFile))
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
// a container path or a traversing source.
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

// ---- agent-side scripts ----

// volumeResolveMountpointScript prints a Docker volume's host mountpoint, then
// fails (set -e) when the volume does not exist.
func volumeResolveMountpointScript(name string) string {
	return fmt.Sprintf(`set -e
docker volume inspect --format '{{.Mountpoint}}' %s`, shellQuote(name))
}

// volumeBackupScript packs the volume's data dir into one tar.gz on the host,
// then prints the size/sha markers the engine parses (same contract as the
// snapshot engine's buildScript).
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

// volumeRestoreScript takes a safety copy of the current data, then unpacks the
// archive over it. The safety copy is best-effort (a fresh volume has nothing
// to preserve but the restore itself must not be blocked by that).
func volumeRestoreScript(hostDir, archivePath string) string {
	return fmt.Sprintf(`set -e
[ -f %s ] || { echo "归档文件不存在: %s" >&2; exit 1; }
mkdir -p %s
tar -C %s -czf %s.pre-restore.tar.gz . 2>/dev/null || true
tar -C %s -xzf %s`,
		shellQuote(archivePath), archivePath,
		shellQuote(hostDir),
		shellQuote(hostDir), shellQuote(archivePath),
		shellQuote(hostDir), shellQuote(archivePath))
}

// volumeContainerStateScript reports whether a container is running, without
// failing when it does not exist (docker inspect exits non-zero then).
func volumeContainerStateScript(container string) string {
	return fmt.Sprintf(`if docker inspect --format '{{.State.Running}}' %s 2>/dev/null | grep -qi '^true$'; then echo RUNNING; else echo STOPPED; fi`,
		shellQuote(container))
}

// volumeRemoveScript deletes archive files from the host (best effort).
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

// ---- engine ----

// backupAppVolume archives one volume spec of an application synchronously and
// returns the stored record. Called on a goroutine by the HTTP handler.
func (e *Engine) backupAppVolume(hub *rpc.Hub, app *Application, spec, note string) (*VolumeArchive, error) {
	src, dst, _, ok := parseVolumeSpec(spec)
	if !ok {
		return nil, fmt.Errorf("挂载卷格式非法: %s（应为 宿主路径:容器路径）", spec)
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
		ID:            archiveID,
		AppID:         app.ID,
		HostID:        app.HostID,
		VolumeSpec:    spec,
		HostPath:      hostDir,
		ContainerPath: dst,
		FilePath:      path.Join(volumeBackupRoot, app.ID, archiveID+".tar.gz"),
		Note:          note,
		Status:        VolumeBackupRunning,
		CreatedAt:     time.Now(),
	}
	if e.vols != nil {
		if err := e.vols.AddArchive(arch); err != nil {
			return nil, fmt.Errorf("记录备份元数据失败: %v", err)
		}
	}

	res, err := execOnAgent(hub, "volbk-"+arch.ID, volumeBackupScript(hostDir, arch.FilePath), 1800)
	if err != nil {
		e.failVolumeArchive(arch, err.Error())
		return arch, fmt.Errorf("备份执行失败: %v", err)
	}
	if res.GetExitCode() != 0 {
		msg := strings.TrimSpace(string(res.GetStderr()))
		if msg == "" {
			msg = fmt.Sprintf("备份失败 (exit %d)", res.GetExitCode())
		}
		e.failVolumeArchive(arch, msg)
		return arch, fmt.Errorf("%s", msg)
	}
	size, sha := parseVolumeArchiveMeta(res.GetStdout())
	arch.Size = size
	arch.SHA256 = sha
	arch.Status = VolumeBackupSuccess
	if e.vols != nil {
		_ = e.vols.PutArchive(arch)
	}
	e.log.Info("volume backup completed", "app", app.ID, "spec", spec, "archive", arch.ID, "bytes", size)
	return arch, nil
}

// failVolumeArchive marks an in-flight record failed (kept for forensics).
func (e *Engine) failVolumeArchive(arch *VolumeArchive, msg string) {
	arch.Status = VolumeBackupFailed
	arch.Error = msg
	if e.vols != nil {
		_ = e.vols.PutArchive(arch)
	}
}

// BackupAppVolumes archives one or more volumes of an application. specs empty
// means every declared volume. Volumes run serially: each holds a busy slot on
// the host's disk and a parallel tar of the same app adds no value.
func (e *Engine) BackupAppVolumes(app *Application, specs []string, note string) ([]*VolumeArchive, error) {
	if !e.acquireVolumeSlot(app.ID) {
		return nil, fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)

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

	hub := e.volumeHub(app)
	if hub == nil {
		return nil, fmt.Errorf("目标主机 Agent 不在线")
	}
	out := make([]*VolumeArchive, 0, len(deduped))
	var firstErr error
	for _, spec := range deduped {
		arch, err := e.backupAppVolume(hub, app, spec, note)
		if arch != nil {
			out = append(out, arch)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return out, firstErr
	}
	// Retention prune per volume spec: older successful archives beyond the
	// keep-count lose both host files and metadata.
	for _, spec := range deduped {
		e.pruneVolumeArchives(hub, app.ID, spec)
	}
	return out, nil
}

// pruneVolumeArchives keeps the newest volumeRetention archives of one spec and
// deletes the rest (host files first, then metadata).
func (e *Engine) pruneVolumeArchives(hub *rpc.Hub, appID, spec string) {
	if e.vols == nil {
		return
	}
	all := e.vols.ListArchives(appID)
	var stale []*VolumeArchive
	seen := 0
	for _, a := range all {
		if a.VolumeSpec != spec {
			continue
		}
		seen++
		if seen > volumeRetention {
			stale = append(stale, a)
		}
	}
	if len(stale) == 0 {
		return
	}
	if hub != nil {
		paths := make([]string, 0, len(stale))
		for _, a := range stale {
			paths = append(paths, a.FilePath)
		}
		if _, err := execOnAgent(hub, "volrm-"+randomToken(4), volumeRemoveScript(paths), 60); err != nil {
			e.log.Warn("volume archive prune failed", "app", appID, "spec", spec, "err", err)
		}
	}
	for _, a := range stale {
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
		return false, fmt.Errorf("该应用已有备份或恢复任务在执行")
	}
	defer e.releaseVolumeSlot(app.ID)

	hub := e.volumeHub(app)
	if hub == nil {
		return false, fmt.Errorf("目标主机 Agent 不在线")
	}
	container := app.ContainerName
	if container == "" {
		return false, fmt.Errorf("应用尚未部署过，无法恢复数据卷")
	}
	if arch == nil || arch.FilePath == "" || arch.HostPath == "" {
		return false, fmt.Errorf("归档记录不完整")
	}
	if arch.HostID != app.HostID {
		return false, fmt.Errorf("归档所属主机与应用不一致")
	}

	stateRes, serr := execOnAgent(hub, "volst-"+randomToken(4), volumeContainerStateScript(container), 30)
	if serr != nil {
		return false, fmt.Errorf("查询容器状态失败: %v", serr)
	}
	wasRunning := strings.Contains(strings.ToUpper(string(stateRes.GetStdout())), "RUNNING")

	if wasRunning {
		stopRes, stopErr := execOnAgent(hub, "volstop-"+randomToken(4), "docker stop "+shellQuote(container), 300)
		if stopErr != nil {
			return false, fmt.Errorf("停止应用容器失败: %v", stopErr)
		}
		if stopRes.GetExitCode() != 0 {
			return false, fmt.Errorf("停止应用容器失败(exit %d): %s", stopRes.GetExitCode(), strings.TrimSpace(string(stopRes.GetStderr())))
		}
	}

	// Restore, then always bring the container back to the state it was in.
	res, rerr := execOnAgent(hub, "volre-"+arch.ID, volumeRestoreScript(arch.HostPath, arch.FilePath), 1800)
	if wasRunning {
		startRes, startErr := execOnAgent(hub, "volstart-"+randomToken(4), "docker start "+shellQuote(container), 120)
		if startErr != nil {
			e.log.Error("volume restore: container restart failed", "app", app.ID, "container", container, "err", startErr)
		} else if startRes.GetExitCode() != 0 {
			e.log.Error("volume restore: container restart exited non-zero", "app", app.ID, "container", container, "exit", startRes.GetExitCode())
		} else {
			restarted = true
		}
	}
	if rerr != nil {
		return restarted, fmt.Errorf("恢复执行失败: %v", rerr)
	}
	if res.GetExitCode() != 0 {
		return restarted, fmt.Errorf("恢复失败(exit %d): %s", res.GetExitCode(), strings.TrimSpace(string(res.GetStderr())))
	}
	e.log.Info("volume restored", "app", app.ID, "archive", arch.ID, "restarted", restarted)
	return restarted, nil
}

// DeleteAppVolumeArchive removes an archive's host files (best effort — the
// host may be offline) and its metadata. Metadata deletion is authoritative:
// an unreachable host's leftovers are cleaned by the operator or the host's
// own backup retention.
func (e *Engine) DeleteAppVolumeArchive(app *Application, arch *VolumeArchive) error {
	if arch == nil {
		return fmt.Errorf("归档记录不存在")
	}
	hub := e.volumeHub(app)
	if hub != nil && arch.FilePath != "" {
		if _, err := execOnAgent(hub, "voldel-"+arch.ID,
			volumeRemoveScript([]string{arch.FilePath, arch.FilePath + ".pre-restore.tar.gz"}), 120); err != nil {
			// Not fatal: the record is dropped either way.
			e.log.Warn("volume archive host file delete failed", "app", app.ID, "archive", arch.ID, "err", err)
		}
	}
	if e.vols == nil {
		return nil
	}
	return e.vols.DeleteArchive(app.ID, arch.ID)
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
