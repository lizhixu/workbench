package apps

import (
	"encoding/hex"
	"errors"
	"fmt"
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
	// The s3 block reports the default target for display; the backup
	// button itself is gated by the destination the user picks on the page
	// (an unknown target or no target at all -> 400 on POST).
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
		Volumes  []string `json:"volumes"`
		Note     string   `json:"note"`
		TargetID string   `json:"target_id"`
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
	// The destination is chosen on the app page (Dokploy-style): an explicit
	// target_id wins, an omitted one falls back to the default target.
	// Resolve before the slot so a bad choice fails fast without blocking
	// other work, and so the frozen archive target is fixed up front.
	target, err := h.engine.resolveVolumeBackupTarget(req.TargetID)
	if err != nil {
		if errors.Is(err, errVolumeTargetUnknown) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "所选备份位置不存在，请重新选择"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请先选择备份位置；若还没有存储目标，先在「快照备份 → S3 存储」中添加"})
		}
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
		if _, err := h.engine.runBackupAppVolumesWith(app, specs, note, target, volumeRetention); err != nil {
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

// uploadVolumeBackupRestore accepts a backup file the operator uploads and
// restores it into one declared volume of the app. The bytes land on the
// panel first (the browser cannot reach the managed host); the agent pulls
// them back over a one-time token URL, then the run follows the shared
// restore core (volume-dir check, stop, safety copy, unpack, restart).
// Preflight order mirrors restoreVolumeBackup: slot, then deploying, then
// offline — and the slot stays held through the whole upload + restore, so a
// second volume operation cannot interleave with either half.
func (h *AppHandlers) uploadVolumeBackupRestore(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil || h.engine.vols == nil || h.engine.volUploads == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "上传恢复暂不可用"})
		return
	}
	if !volumeBackupSupported(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Compose 应用的挂载卷在编排文件中声明，暂不支持应用级上传恢复"})
		return
	}
	if h.volumeWindowsRejected(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据卷恢复暂仅支持 Linux 主机"})
		return
	}
	// Same pre-flight order as restore: atomic slot, then deploy check, then
	// offline (see restoreVolumeBackup).
	if !h.engine.acquireVolumeSlot(app.ID) {
		c.JSON(http.StatusConflict, gin.H{"error": "该应用已有备份或恢复任务在执行"})
		return
	}
	if h.engine.IsRunning(app.ID) {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusConflict, gin.H{"error": "应用正在部署，暂不能恢复数据卷"})
		return
	}
	hub := h.appHub(app.HostID)
	if hub == nil {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标主机 Agent 不在线"})
		return
	}

	// Validate the public URL before reading any uploaded data: an operator
	// uploading a 2GB archive should fail fast if the server cannot form the
	// agent download URL. Do not derive this from Request.Host: behind a proxy
	// that header is client-controlled and may point the agent at an attacker.
	configuredURL := ""
	if h.settings != nil {
		configuredURL = h.settings.PanelSecurity().PublicURL
	}
	if strings.TrimSpace(configuredURL) == "" {
		configuredURL = agentPublicURL
	}
	baseURL, err := uploadRestoreBaseURL(configuredURL)
	if err != nil {
		h.engine.releaseVolumeSlot(app.ID)
		// Surface the concrete reason: "not configured" and "configured but
		// unusable" need different fixes, and the validator's messages are
		// already operator-facing Chinese.
		c.JSON(http.StatusBadRequest, gin.H{"error": "无法生成 Agent 下载地址：" + err.Error()})
		return
	}

	archiveID := "vbk_" + randomToken(8)
	fileSaved := false
	// cleanup drops the staged file (if any) and releases the slot. It runs
	// on every failure path before the 202; after the 202 the restore
	// goroutine owns both.
	cleanup := func() {
		if fileSaved {
			h.engine.volUploads.RemoveFile(archiveID)
		}
		h.engine.releaseVolumeSlot(app.ID)
	}

	// Stream the multipart body straight to disk: a 2GB file must never sit
	// in panel memory. Fields may arrive in any order, so the file is saved
	// as it streams and validated after the body is fully read.
	mr, err := c.Request.MultipartReader()
	if err != nil {
		cleanup()
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求不是 multipart 上传"})
		return
	}
	var spec, sha string
	var fileSize int64
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传内容失败"})
			return
		}
		switch part.FormName() {
		case "volume_spec":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			spec = strings.TrimSpace(string(b))
		case "sha256":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			sha = strings.TrimSpace(strings.ToLower(string(b)))
		case "file":
			if fileSaved {
				cleanup()
				c.JSON(http.StatusBadRequest, gin.H{"error": "只能上传一个备份文件"})
				return
			}
			size, err := h.engine.volUploads.Save(archiveID, part)
			if err != nil {
				cleanup()
				if errors.Is(err, errVolumeUploadTooLarge) {
					c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "保存上传文件失败"})
				}
				return
			}
			fileSaved = true
			fileSize = size
		default:
			io.Copy(io.Discard, part)
		}
	}
	if !fileSaved {
		cleanup()
		c.JSON(http.StatusBadRequest, gin.H{"error": "请上传备份文件"})
		return
	}

	// Only declared volumes may be restored into: the upload + restore path
	// must not become a way to write arbitrary host directories.
	declared := false
	for _, v := range app.Volumes {
		if strings.TrimSpace(v) == spec {
			declared = true
			break
		}
	}
	if spec == "" || !declared {
		cleanup()
		c.JSON(http.StatusBadRequest, gin.H{"error": "只能恢复到应用已声明的挂载卷"})
		return
	}
	src, dst, _, specOK := parseVolumeSpec(spec)
	if !specOK || src == "" || dst == "" {
		cleanup()
		c.JSON(http.StatusBadRequest, gin.H{"error": "挂载卷规格无效"})
		return
	}
	if sha != "" {
		if len(sha) != 64 {
			cleanup()
			c.JSON(http.StatusBadRequest, gin.H{"error": "SHA256 格式无效（应为 64 位十六进制）"})
			return
		}
		if _, err := hex.DecodeString(sha); err != nil {
			cleanup()
			c.JSON(http.StatusBadRequest, gin.H{"error": "SHA256 格式无效（应为 64 位十六进制）"})
			return
		}
	}
	// Resolve the host directory up front so the record carries the same
	// HostPath the restore core re-validates against (bind: the declared
	// path; named: the current docker mountpoint).
	hostPath := src
	if !volumeIsBindMount(src) {
		res, err := execOnAgent(hub, "volupath-"+randomToken(4), volumeResolveMountpointScript(src), 30)
		if err != nil {
			cleanup()
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "查询 Docker 卷挂载点失败"})
			return
		}
		if res.GetExitCode() != 0 || strings.TrimSpace(string(res.GetStdout())) == "" {
			cleanup()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Docker 卷 %s 不存在，卷配置已变更", src)})
			return
		}
		hostPath = strings.TrimSpace(string(res.GetStdout()))
	}

	arch := &VolumeArchive{
		ID:            archiveID,
		AppID:         app.ID,
		HostID:        app.HostID,
		VolumeSpec:    spec,
		HostPath:      hostPath,
		ContainerPath: dst,
		Size:          fileSize,
		SHA256:        sha,
		Status:        VolumeBackupUploaded,
		Source:        VolumeArchiveSourceUpload,
		Note:          "上传文件恢复",
		CreatedAt:     time.Now(),
	}
	if err := h.engine.vols.AddArchive(arch); err != nil {
		cleanup()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存上传恢复记录失败"})
		return
	}
	tok, err := h.engine.volUploads.IssueToken(app.ID, archiveID)
	if err != nil {
		cleanup()
		if derr := h.engine.vols.DeleteArchive(app.ID, archiveID); derr != nil {
			h.engine.log.Warn("cleanup volume archive failed", "app", app.ID, "archive", archiveID, "err", derr)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成下载链接失败"})
		return
	}
	downloadURL := fmt.Sprintf("%s/api/v1/volume-uploads/%s", baseURL, tok)

	go func() {
		defer h.engine.releaseVolumeSlot(app.ID)
		if _, err := h.engine.runRestoreUploadedVolume(app, arch, downloadURL, sha); err != nil {
			h.engine.log.Warn("volume upload restore failed", "app", app.ID, "archive", arch.ID, "err", err)
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{
		"ok": true, "archive_id": archiveID,
		"message": "上传完成，恢复任务已开始：将自动停机、恢复数据并在完成后重启应用容器",
	})
}

// downloadVolumeUpload serves an operator-uploaded restore file to the
// managed host's agent: the token in the path is the only credential
// (single-use, 2h TTL), mirroring the webhook token-in-path pattern. No JWT:
// the agent holds no panel session. The secure-entry guard exempts this
// prefix (see secentry.exemptPath); without the exemption an enabled entry
// gate would 404 the agent's download mid-restore.
func (h *AppHandlers) downloadVolumeUpload(c *gin.Context) {
	if h.engine == nil || h.engine.volUploads == nil || h.engine.vols == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "上传恢复暂不可用"})
		return
	}
	token := c.Param("token")
	archiveID, appID, source, status, ok := h.engine.volUploads.peekToken(token)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "下载链接无效或已过期"})
		return
	}
	arch, ok := h.engine.GetVolumeArchive(appID, archiveID)
	if !ok || source != VolumeArchiveSourceUpload || status != VolumeBackupUploaded || arch.AppID != appID || arch.ID != archiveID || arch.Source != VolumeArchiveSourceUpload || arch.Status != VolumeBackupUploaded {
		// Burn a token that points at an invalid lifecycle record so it cannot be
		// replayed after an operator deletes or changes the archive.
		h.engine.volUploads.ConsumeToken(token)
		c.JSON(http.StatusNotFound, gin.H{"error": "下载链接无效或已过期"})
		return
	}
	if !h.engine.volUploads.ConsumeTokenForRecord(token, appID, archiveID, arch.Source, arch.Status) {
		c.JSON(http.StatusNotFound, gin.H{"error": "下载链接无效或已过期"})
		return
	}
	if err := h.engine.volUploads.Serve(c.Writer, archiveID); err != nil {
		h.engine.log.Warn("volume upload serve failed", "archive", archiveID, "err", err)
		if !c.Writer.Written() {
			c.JSON(http.StatusNotFound, gin.H{"error": "上传文件不存在或已被清理"})
		}
	}
}
