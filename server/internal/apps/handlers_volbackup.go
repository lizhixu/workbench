package apps

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/rpc"
)

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
	c.JSON(http.StatusOK, gin.H{"data": page, "total": total, "offset": offset, "page_size": pageSize})
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
	_ = c.ShouldBindJSON(&req)

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
	// Pre-flight the busy check before the online check: an in-flight run means
	// the host was just reachable, so "busy" is the more accurate answer than
	// "offline" (and a double-clicked 开始备份 must not silently no-op).
	if h.engine.VolumeBusy(app.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "该应用已有备份或恢复任务在执行"})
		return
	}
	if h.appHub(app.HostID) == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标主机 Agent 不在线"})
		return
	}

	note := strings.TrimSpace(req.Note)
	go func() {
		if _, err := h.engine.BackupAppVolumes(app, specs, note); err != nil {
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
	// Same pre-flight order as backup: busy before offline (see above).
	if h.engine.VolumeBusy(app.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "该应用已有备份或恢复任务在执行"})
		return
	}
	if h.appHub(app.HostID) == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标主机 Agent 不在线"})
		return
	}
	go func() {
		if _, err := h.engine.RestoreAppVolume(app, arch); err != nil {
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
	if err := h.engine.DeleteAppVolumeArchive(app, arch); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
