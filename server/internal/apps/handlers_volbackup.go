package apps

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/rpc"
)

// volumeNoteMaxRunes bounds the free-form note stored on each archive record.
const volumeNoteMaxRunes = 200

// ---- 应用挂载卷备份与恢复（volbackup.go 的 HTTP 面）----
//
// 备份与恢复都在受管主机上跑 tar，可能持续数分钟，因此统一 202 受理 + 异步
// 执行 + 前端轮询归档列表（与快照引擎 POST /backups/jobs/:id/run 同惯例）。
// 恢复会停机/重启应用容器，属于写操作组并落审计（见 router.go 路由元数据表）。

// volumeBackupSupported rejects the sources whose mounts are not app.Volumes:
// compose apps declare theirs inside the YAML, which the record doesn't own —
// archiving "guessed" paths would silently miss or double-count data.
func volumeBackupSupported(app *Application) bool {
	if app.SourceType == "raw_compose" {
		return false
	}
	if app.SourceType == "git" && strings.EqualFold(strings.TrimSpace(app.BuildType), "compose") {
		return false
	}
	return true
}

// volumeWindowsRejected reports whether the app host is a Windows agent. The
// backup/restore scripts are POSIX (tar/set -e); the same front-door rejection
// the reverse-proxy binding uses instead of a cryptic failure deep inside the
// agent exec.
func (h *AppHandlers) volumeWindowsRejected(app *Application) bool {
	if h.reg == nil {
		return false
	}
	ag := h.reg.GetAgent(app.HostID)
	return ag != nil && strings.EqualFold(ag.OS, "windows")
}

// appHub resolves the app host's agent channel. h.reg is nil in unit-test
// wiring (and in any degraded server start), which must surface as "offline"
// rather than a nil dereference.
func (h *AppHandlers) appHub(hostID string) *rpc.Hub {
	if h.reg == nil {
		return nil
	}
	return h.reg.Hub(hostID)
}

func (h *AppHandlers) listVolumeBackups(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment engine unavailable"})
		return
	}
	list := h.engine.VolumeArchives(app.ID)
	if volume := strings.TrimSpace(c.Query("volume")); volume != "" {
		filtered := make([]*VolumeArchive, 0, len(list))
		for _, a := range list {
			if a.VolumeSpec == volume {
				filtered = append(filtered, a)
			}
		}
		list = filtered
	}
	total := len(list)
	offset := intQuery(c, "offset", 0)
	pageSize := intQuery(c, "page_size", 20)
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	if offset > total {
		offset = total
	}
	end := offset + pageSize
	if end > total {
		end = total
	}
	page := list[offset:end]
	if page == nil {
		page = []*VolumeArchive{}
	}
	// The s3 block tells the UI where new archives would go and whether the
	// backup button can work at all (no default target -> 400 on POST).
	c.JSON(http.StatusOK, gin.H{
		"data": page, "total": total, "offset": offset, "page_size": pageSize,
		"s3": h.engine.S3Status(),
	})
}

func (h *AppHandlers) createVolumeBackup(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment engine unavailable"})
		return
	}
	var req struct {
		Volumes []string `json:"volumes"`
		Note    string   `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
		return
	}

	if !volumeBackupSupported(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Compose 应用的挂载卷在编排文件中声明，暂不支持应用级备份；可前往「快照备份」按目录或 Docker 卷备份"})
		return
	}
	if h.volumeWindowsRejected(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据卷备份暂仅支持 Linux 主机"})
		return
	}
	// Only declared volumes may be archived: the backup + host file download
	// path must not become a way to exfiltrate arbitrary host directories.
	declared := make(map[string]bool, len(app.Volumes))
	for _, v := range app.Volumes {
		v = strings.TrimSpace(v)
		if v != "" {
			declared[v] = true
		}
	}
	if len(declared) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "应用未配置任何挂载卷"})
		return
	}
	specs := make([]string, 0, len(req.Volumes))
	for _, s := range req.Volumes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !declared[s] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "只能备份应用已声明的挂载卷: " + s})
			return
		}
		specs = append(specs, s)
	}
	note := strings.TrimSpace(req.Note)
	if utf8.RuneCountInString(note) > volumeNoteMaxRunes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注不能超过 200 个字符"})
		return
	}
	// Archives live in S3 only; without a default target there is nowhere to
	// upload them, so reject before any agent work.
	if !h.engine.S3Status().Ready {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未配置默认 S3 存储目标，请先在「快照备份 → S3 存储」中添加目标并设为默认"})
		return
	}
	// Acquire the slot synchronously: a check-then-goroutine race would answer
	// 202 to a double-clicked request whose actual backup then silently no-ops.
	if !h.engine.acquireVolumeSlot(app.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "该应用已有备份或恢复任务在执行"})
		return
	}
	// Busy is checked before offline: an in-flight run means the host was just
	// reachable, so "busy" is the more accurate answer than "offline".
	if h.engine.IsRunning(app.ID) {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusConflict, gin.H{"error": "应用正在部署，暂不能备份数据卷"})
		return
	}
	if h.appHub(app.HostID) == nil {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标主机 Agent 不在线"})
		return
	}

	go func() {
		defer h.engine.releaseVolumeSlot(app.ID)
		if _, err := h.engine.runBackupAppVolumes(app, specs, note); err != nil {
			h.engine.log.Warn("volume backup failed", "app", app.ID, "err", err)
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "备份任务已开始，请稍后查看归档列表"})
}

func (h *AppHandlers) restoreVolumeBackup(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment engine unavailable"})
		return
	}
	arch, ok := h.engine.GetVolumeArchive(app.ID, c.Param("archiveID"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "归档记录不存在"})
		return
	}
	if h.volumeWindowsRejected(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据卷恢复暂仅支持 Linux 主机"})
		return
	}
	if arch.Status != VolumeBackupSuccess {
		c.JSON(http.StatusBadRequest, gin.H{"error": "归档未成功完成，不能用于恢复"})
		return
	}
	// Same pre-flight order as backup: atomic slot, then deploy check, then
	// offline (see above).
	if !h.engine.acquireVolumeSlot(app.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "该应用已有备份或恢复任务在执行"})
		return
	}
	if h.engine.IsRunning(app.ID) {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusConflict, gin.H{"error": "应用正在部署，暂不能恢复数据卷"})
		return
	}
	if h.appHub(app.HostID) == nil {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标主机 Agent 不在线"})
		return
	}
	go func() {
		defer h.engine.releaseVolumeSlot(app.ID)
		if _, err := h.engine.runRestoreAppVolume(app, arch); err != nil {
			h.engine.log.Warn("volume restore failed", "app", app.ID, "archive", arch.ID, "err", err)
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "恢复任务已开始：将自动停机、恢复数据并在完成后重启应用容器"})
}

func (h *AppHandlers) deleteVolumeBackup(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment engine unavailable"})
		return
	}
	arch, ok := h.engine.GetVolumeArchive(app.ID, c.Param("archiveID"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "归档记录不存在"})
		return
	}
	if arch.Status == VolumeBackupRunning {
		c.JSON(http.StatusConflict, gin.H{"error": "备份任务执行中，不能删除该归档"})
		return
	}
	if !h.engine.acquireVolumeSlot(app.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "该应用已有备份或恢复任务在执行"})
		return
	}
	defer h.engine.releaseVolumeSlot(app.ID)
	if err := h.engine.deleteAppVolumeArchive(app, arch); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// downloadVolumeBackup redirects the browser to a short-lived pre-signed S3
// GET: the archive bytes flow from S3 to the client without traversing the
// control plane (same direct-to-storage principle as the upload). Read-only,
// so no audit entry; the JWT can arrive as ?token= for navigation downloads.
func (h *AppHandlers) downloadVolumeBackup(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment engine unavailable"})
		return
	}
	arch, ok := h.engine.GetVolumeArchive(app.ID, c.Param("archiveID"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "归档记录不存在"})
		return
	}
	if arch.Status != VolumeBackupSuccess {
		c.JSON(http.StatusBadRequest, gin.H{"error": "归档未成功完成，无法下载"})
		return
	}
	url, err := h.engine.VolumeArchiveDownloadURL(arch, 15*time.Minute)
	if err != nil {
		if errors.Is(err, errVolumeS3TargetMissing) {
			c.JSON(http.StatusGone, gin.H{"error": "归档的 S3 存储目标已不存在，无法下载"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.Redirect(http.StatusFound, url)
}
