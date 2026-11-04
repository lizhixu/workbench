package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
	"watchman/server/internal/settings"
)

// volumeNoteMaxRunes bounds the free-form note stored on each archive record.
const volumeNoteMaxRunes = 200

// ---- 应用挂载卷备份与恢复（volbackup.go 的 HTTP 面）----
//
// 备份与恢复都在受管主机上跑 tar，可能持续数分钟，因此统一 202 受理 + 异步
// 执行 + 前端轮询归档列表（与快照引擎 POST /backups/jobs/:id/run 同惯例）。
// 恢复会停机/重启应用容器，属于写操作组并落审计（见 router.go 路由元数据表）。

// volumeBackupSupported reports whether an application supports volume backup.
func volumeBackupSupported(app *Application) bool {
	return app != nil
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
	disc, _ := h.engine.DiscoverAppVolumes(app)
	c.JSON(http.StatusOK, gin.H{
		"data": page, "total": total, "offset": offset, "page_size": pageSize,
		"s3":      h.engine.S3Status(),
		"volumes": disc,
	})
}

func (h *AppHandlers) getAppVolumes(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "deployment engine unavailable"})
		return
	}
	vols, err := h.engine.DiscoverAppVolumes(app)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": vols})
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
		Volumes         []string `json:"volumes"`
		Note            string   `json:"note"`
		TargetID        string   `json:"target_id"`
		ConsistencyMode string   `json:"consistency_mode"`
		Retention       int      `json:"retention"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
		return
	}

	// Retention only matters when the caller asks for a rolling window: 0 or
	// omitted means a plain manual backup, which must not prune anything (the
	// scheduled plans own retention). The upper bound matches the per-app
	// metadata cap so a retention larger than the store itself can never be
	// requested.
	if req.Retention < 0 || req.Retention > volumeArchiveMetaCap {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("最大保留份数须在 1~%d 之间", volumeArchiveMetaCap)})
		return
	}
	keep := req.Retention
	if keep <= 0 {
		keep = noPruneVolumes
	}

	if h.volumeWindowsRejected(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据卷备份暂仅支持 Linux 主机"})
		return
	}

	consistency := strings.TrimSpace(req.ConsistencyMode)
	if consistency == "" {
		consistency = ConsistencyModeOnline
	}
	switch consistency {
	case ConsistencyModeOnline, ConsistencyModePause, ConsistencyModeStop:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的一致性模式，仅支持 online, pause, stop"})
		return
	}

	note := strings.TrimSpace(req.Note)
	if utf8.RuneCountInString(note) > volumeNoteMaxRunes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注不能超过 200 个字符"})
		return
	}

	if !isComposeApp(app) {
		if len(app.Volumes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "应用未配置任何挂载卷"})
			return
		}
		if len(req.Volumes) > 0 {
			declared := make(map[string]bool, len(app.Volumes))
			for _, v := range app.Volumes {
				if v = strings.TrimSpace(v); v != "" {
					declared[v] = true
				}
			}
			for _, s := range req.Volumes {
				if s = strings.TrimSpace(s); s != "" && !declared[s] {
					c.JSON(http.StatusBadRequest, gin.H{"error": "只能备份应用已声明的挂载卷: " + s})
					return
				}
			}
		}
	}
	// The destination is chosen on the app page (Dokploy-style): an explicit
	// target_id wins, an omitted one falls back to the default target.
	// Resolve before the slot so a bad choice fails fast without blocking
	// other work, and so the frozen archive target is fixed up front.
	target, isLocal, err := h.engine.resolveVolumeBackupTarget(req.TargetID)
	if err != nil {
		if errors.Is(err, errVolumeTargetUnknown) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "所选备份位置不存在，请重新选择"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请先选择备份位置；若还没有存储目标，先在「快照备份 → S3 存储」中添加"})
		}
		return
	}
	_ = target
	_ = isLocal

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
		opts := BackupSnapshotOptions{
			Specs:           req.Volumes,
			Note:            note,
			StorageTarget:   req.TargetID,
			ConsistencyMode: consistency,
			Keep:            keep,
		}
		if _, err := h.engine.runBackupAppSnapshot(app, opts); err != nil {
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

	var req struct {
		CleanRestore *bool    `json:"clean_restore"`
		Volumes      []string `json:"volumes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
		return
	}

	cleanRestore := true
	if req.CleanRestore != nil {
		cleanRestore = *req.CleanRestore
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
	opts := RestoreVolumeOptions{
		CleanRestore: cleanRestore,
		Specs:        req.Volumes,
	}
	go func() {
		defer h.engine.releaseVolumeSlot(app.ID)
		if _, err := h.engine.runRestoreAppVolume(app, arch, opts); err != nil {
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

	if arch.StorageTarget == StorageTargetLocal {
		hub := h.appHub(arch.HostID)
		if hub == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "存储主机 Agent 不在线，无法下载本地备份"})
			return
		}
		h.streamLocalArchiveDownload(c, hub, arch)
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

// streamLocalArchiveDownload streams a host-local volume backup directly from the agent.
func (h *AppHandlers) streamLocalArchiveDownload(c *gin.Context, hub *rpc.Hub, arch *VolumeArchive) {
	opID := randomToken(8)
	chunkCh := make(chan *agentpb.FileChunk, 64)
	done := make(chan struct{})
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			close(done)
			return
		}
		if ack := msg.GetAck(); ack != nil && !ack.GetOk() {
			close(done)
			return
		}
		fc := msg.GetFileChunk()
		if fc == nil {
			return
		}
		chunkCh <- fc
		if fc.GetEof() {
			close(done)
		}
	})
	defer hub.SetRespHandler(opID, nil)

	sent := hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_FileOp{
			FileOp: &agentpb.FileOp{OpId: opID, Op: "read", Path: arch.StoragePath},
		},
	})
	if !sent {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "与 Agent 通信失败"})
		return
	}

	filename := fmt.Sprintf("%s-%s.tar.gz", arch.AppID, arch.ID)
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.Status(http.StatusOK)

	timeout := time.After(300 * time.Second)
	for {
		select {
		case fc := <-chunkCh:
			if fc == nil || fc.GetEof() {
				return
			}
			if len(fc.GetData()) > 0 {
				if _, err := c.Writer.Write(fc.GetData()); err != nil {
					return
				}
				c.Writer.Flush()
			}
		case <-done:
			return
		case <-timeout:
			return
		}
	}
}

// volumeUploadPairCap bounds how many files one import may carry: every file
// stages on the panel before the restore starts, so the cap bounds panel disk
// use the same way the per-file 2GB limit does.
const volumeUploadPairCap = 10

// volumeScanItemResp is one scanned upload file returned to the UI.
type volumeScanItemResp struct {
	ArchiveID       string              `json:"archive_id"`
	Filename        string              `json:"filename"`
	Size            int64               `json:"size"`
	SHA256          string              `json:"sha256"`
	Snapshot        bool                `json:"snapshot"`
	TopDirs         []string            `json:"top_dirs,omitempty"`
	ManifestVolumes []VolumeItemArchive `json:"manifest_volumes,omitempty"`
	// Match: "manifest"（快照清单命中）| "unique"（唯一命中）| "ambiguous"
	//（多个候选，操作员选择）| "none"（未识别，操作员选择）。
	Match   string            `json:"match"`
	Matched []volumeScanMatch `json:"matched"`
}

// hashingReader hashes bytes as they stream past, so the staging write and
// the SHA256 the commit later hard-checks come from one pass over the body.
type hashingReader struct {
	r io.Reader
	h hash.Hash
}

func newHashingReader(r io.Reader) *hashingReader {
	return &hashingReader{r: r, h: sha256.New()}
}

func (hr *hashingReader) Read(p []byte) (int, error) {
	n, err := hr.r.Read(p)
	if n > 0 {
		hr.h.Write(p[:n])
	}
	return n, err
}

func (hr *hashingReader) hex() string {
	return hex.EncodeToString(hr.h.Sum(nil))
}

// scanUploadFilename keeps a client-provided filename for display only.
func scanUploadFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "" || name == "." || name == "/" {
		return "backup.tar"
	}
	return name
}

// scanVolumeUploads is phase 1 of the two-phase import: it stages the files on
// the panel and returns, per file, which target volume(s) the content was
// auto-recognized as. No slot is taken and no agent is strictly required — a
// scan only reads app metadata plus the staged bytes, so Compose volume
// discovery degrades to "no candidates" when the host is offline and the UI
// falls back to manual selection. Anything staged here is inert until the
// operator confirms phase 2.
func (h *AppHandlers) scanVolumeUploads(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil || h.engine.vols == nil || h.engine.volUploads == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "上传恢复暂不可用"})
		return
	}
	if h.volumeWindowsRejected(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据卷恢复暂仅支持 Linux 主机"})
		return
	}
	us := h.engine.volUploads
	// Expired scans are dropped here instead of on a timer: the only thing
	// that creates staged scan files is this endpoint itself.
	us.SweepScans()
	candidates := volumeScanCandidates(h, app)

	mr, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求不是 multipart 上传"})
		return
	}
	type scanned struct {
		scan *stagedScan
		res  *tarScanResult
	}
	var done []scanned
	rollback := func() {
		for _, s := range done {
			us.DropScan(s.scan.ArchiveID)
		}
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传内容失败"})
			return
		}
		// Only file parts (parts carrying a filename) matter here; anything
		// else is a stray field and is drained.
		if part.FileName() == "" {
			io.Copy(io.Discard, part)
			continue
		}
		if len(done) >= volumeUploadPairCap {
			rollback()
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("一次最多上传 %d 个文件", volumeUploadPairCap)})
			return
		}
		id := "vbk_" + randomToken(8)
		hr := newHashingReader(part)
		size, serr := us.Save(id, hr)
		if serr != nil {
			rollback()
			us.RemoveFile(id)
			if errors.Is(serr, errVolumeUploadTooLarge) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": serr.Error()})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "保存上传文件失败"})
			}
			return
		}
		res, terr := scanUploadTar(us.Path(id))
		if terr != nil {
			rollback()
			us.RemoveFile(id)
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("第 %d 个文件「%s」无法识别：%v", len(done)+1, scanUploadFilename(part.FileName()), terr)})
			return
		}
		sc := &stagedScan{
			ArchiveID: id, AppID: app.ID,
			Filename: scanUploadFilename(part.FileName()),
			Size:     size, SHA256: hr.hex(),
			Snapshot: res.manifest != nil, Manifest: res.manifest,
			StagedAt: time.Now(),
		}
		us.PutScan(sc)
		done = append(done, scanned{scan: sc, res: res})
	}
	if len(done) == 0 {
		rollback()
		c.JSON(http.StatusBadRequest, gin.H{"error": "请上传备份文件"})
		return
	}

	items := make([]volumeScanItemResp, 0, len(done))
	for _, s := range done {
		match, matched := matchScanVolumes(s.res, candidates, s.scan.Filename)
		item := volumeScanItemResp{
			ArchiveID: s.scan.ArchiveID, Filename: s.scan.Filename,
			Size: s.scan.Size, SHA256: s.scan.SHA256,
			Snapshot: s.scan.Snapshot, TopDirs: s.res.topDirs,
			Match:   match,
			Matched: []volumeScanMatch{},
		}
		if s.res.manifest != nil {
			item.ManifestVolumes = s.res.manifest.Volumes
		}
		if matched != nil {
			item.Matched = matched
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "items": items})
}

// commitVolumeUploadRestore is phase 2 of the two-phase import: it consumes
// the scan results the operator just reviewed (auto-identified or hand-picked
// per file) and runs the shared batch restore — all files pulled onto the host
// and verified BEFORE any downtime, one stop, per-pair safety copy / wipe /
// unpack, one restart. Preflight order mirrors restoreVolumeBackup: slot, then
// deploying, then offline — and the slot stays held through record creation
// and the whole restore, so a second volume operation cannot interleave.
func (h *AppHandlers) commitVolumeUploadRestore(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if h.engine == nil || h.engine.vols == nil || h.engine.volUploads == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "上传恢复暂不可用"})
		return
	}
	if h.volumeWindowsRejected(app) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "数据卷恢复暂仅支持 Linux 主机"})
		return
	}
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
	// Validate the public URL before touching any staged data: an operator
	// uploading GB-sized archives should fail fast if the server cannot form
	// the agent download URL. The request Host is deliberately refused here:
	// behind a proxy that header is client-controlled and may point the agent
	// at an attacker, so only a bound panel domain is accepted.
	base, configured := settings.RequestBaseURL(c, h.settings)
	if !configured {
		base = ""
	}
	baseURL, err := uploadRestoreBaseURL(base)
	if err != nil {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "无法生成 Agent 下载地址：" + err.Error()})
		return
	}

	var req struct {
		Pairs []struct {
			ArchiveID string   `json:"archive_id"`
			Specs     []string `json:"specs"`
		} `json:"pairs"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误"})
		return
	}
	if len(req.Pairs) == 0 {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先上传并识别备份文件"})
		return
	}
	if len(req.Pairs) > volumeUploadPairCap {
		h.engine.releaseVolumeSlot(app.ID)
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("一次最多导入 %d 份文件", volumeUploadPairCap)})
		return
	}

	// Phase A — validate every pair against the staged scans before consuming
	// anything. The scan registry is only mutated after the whole request is
	// known good, so a rejected commit leaves every upload restageable.
	type commitPair struct {
		scan   *stagedScan
		chosen []volumeScanCandidate
	}
	candidates := volumeScanCandidates(h, app)
	pairs := make([]commitPair, 0, len(req.Pairs))
	seenSpecs := make(map[string]bool, len(req.Pairs))
	for i, rp := range req.Pairs {
		id := strings.TrimSpace(rp.ArchiveID)
		if id == "" {
			h.engine.releaseVolumeSlot(app.ID)
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("第 %d 份文件缺少上传标识", i+1)})
			return
		}
		sc, ok := h.engine.volUploads.PeekScan(app.ID, id)
		if !ok {
			h.engine.releaseVolumeSlot(app.ID)
			c.JSON(http.StatusGone, gin.H{"error": "上传文件已失效（面板可能重启过或确认超时），请重新上传识别"})
			return
		}
		if _, serr := os.Stat(h.engine.volUploads.Path(id)); serr != nil {
			h.engine.releaseVolumeSlot(app.ID)
			c.JSON(http.StatusGone, gin.H{"error": "上传文件已失效，请重新上传识别"})
			return
		}
		chosen, rerr := resolveScanCandidates(rp.Specs, candidates)
		if rerr != nil {
			h.engine.releaseVolumeSlot(app.ID)
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("第 %d 份文件「%s」：%v", i+1, sc.Filename, rerr)})
			return
		}
		if len(chosen) == 0 {
			h.engine.releaseVolumeSlot(app.ID)
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("第 %d 份文件「%s」未选择任何目标挂载卷", i+1, sc.Filename)})
			return
		}
		if !sc.Snapshot && len(chosen) != 1 {
			h.engine.releaseVolumeSlot(app.ID)
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("第 %d 份文件「%s」不是多卷快照，只能恢复到一个目标卷", i+1, sc.Filename)})
			return
		}
		if sc.Snapshot {
			for _, ch := range chosen {
				inManifest := false
				for _, it := range sc.Manifest.Volumes {
					if it.VolumeSpec == ch.Spec || it.ContainerPath == ch.ContainerPath {
						inManifest = true
						break
					}
				}
				if !inManifest {
					h.engine.releaseVolumeSlot(app.ID)
					c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("快照「%s」中不包含挂载卷 %s", sc.Filename, ch.Spec)})
					return
				}
			}
		}
		for _, ch := range chosen {
			if seenSpecs[ch.Spec] {
				h.engine.releaseVolumeSlot(app.ID)
				c.JSON(http.StatusBadRequest, gin.H{"error": "同一挂载卷在一次导入中只能出现一次: " + ch.Spec})
				return
			}
			seenSpecs[ch.Spec] = true
		}
		pairs = append(pairs, commitPair{scan: sc, chosen: chosen})
	}

	// Resolve host directories up front (bind: declared source; named: the
	// current docker mountpoint) so records carry the same HostPath the restore
	// core re-validates against, and so an outdated volume config fails before
	// any record exists.
	for i := range pairs {
		for j := range pairs[i].chosen {
			ch := &pairs[i].chosen[j]
			if ch.HostPath != "" {
				continue
			}
			src, _, _, specOK := parseVolumeSpec(ch.Spec)
			if !specOK {
				h.engine.releaseVolumeSlot(app.ID)
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("第 %d 份文件的挂载卷规格无效", i+1)})
				return
			}
			res, merr := execOnAgent(hub, "volupath-"+randomToken(4), volumeResolveMountpointScript(src), 30)
			if merr != nil {
				h.engine.releaseVolumeSlot(app.ID)
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "查询 Docker 卷挂载点失败"})
				return
			}
			if res.GetExitCode() != 0 || strings.TrimSpace(string(res.GetStdout())) == "" {
				h.engine.releaseVolumeSlot(app.ID)
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("Docker 卷 %s 不存在，卷配置已变更", src)})
				return
			}
			ch.HostPath = strings.TrimSpace(string(res.GetStdout()))
		}
	}

	// Phase B — consume the scans and build the records. From here on the
	// request owns the staged files; rollback drops everything it created.
	rollback := func(ids []string, recorded map[string]bool) {
		for _, id := range ids {
			if recorded[id] {
				if derr := h.engine.vols.DeleteArchive(app.ID, id); derr != nil {
					h.engine.log.Warn("cleanup volume archive failed", "app", app.ID, "archive", id, "err", derr)
				}
			}
			h.engine.volUploads.DropTokenByArchive(id)
			h.engine.volUploads.DropScan(id)
		}
		h.engine.releaseVolumeSlot(app.ID)
	}
	ids := make([]string, 0, len(pairs))
	recorded := make(map[string]bool, len(pairs))
	fail := func(code int, msg string) {
		rollback(ids, recorded)
		c.JSON(code, gin.H{"error": msg})
	}
	items := make([]*uploadedRestoreItem, 0, len(pairs))
	for i := range pairs {
		p := &pairs[i]
		id := p.scan.ArchiveID
		sc, ok := h.engine.volUploads.TakeScan(app.ID, id)
		if !ok {
			fail(http.StatusGone, "上传文件已失效，请重新上传识别")
			return
		}
		ids = append(ids, id)
		arch := &VolumeArchive{
			ID:        id,
			AppID:     app.ID,
			HostID:    app.HostID,
			Size:      sc.Size,
			SHA256:    sc.SHA256,
			Status:    VolumeBackupUploaded,
			Source:    VolumeArchiveSourceUpload,
			Note:      "上传文件恢复",
			CreatedAt: time.Now(),
		}
		specs := make([]string, 0, len(p.chosen))
		if sc.Snapshot {
			// The manifest items keep their original order: the tar unpacks
			// volumes/vol_<Index> by the item's position, so the chosen subset
			// is expressed through the restore filter, never by reindexing.
			// Items that map to a chosen live volume are rewritten onto the
			// app's current coordinates (the strict same-host validation the
			// restore core applies would reject the manifest's stale spec);
			// unchosen items keep their original values and are filtered out.
			for _, it := range sc.Manifest.Volumes {
				out := it
				for _, ch := range p.chosen {
					if it.VolumeSpec == ch.Spec || (it.ContainerPath != "" && it.ContainerPath == ch.ContainerPath) {
						out = VolumeItemArchive{
							VolumeSpec: ch.Spec, VolumeType: ch.VolumeType,
							HostPath: ch.HostPath, ContainerPath: ch.ContainerPath,
							Service: it.Service,
						}
						break
					}
				}
				arch.Volumes = append(arch.Volumes, out)
			}
			arch.VolumeSpec = "ALL"
			if len(p.chosen) == 1 {
				arch.VolumeSpec = p.chosen[0].Spec
			}
			arch.HostPath = p.chosen[0].HostPath
			arch.ContainerPath = p.chosen[0].ContainerPath
		} else {
			arch.VolumeSpec = p.chosen[0].Spec
			arch.HostPath = p.chosen[0].HostPath
			arch.ContainerPath = p.chosen[0].ContainerPath
		}
		for _, ch := range p.chosen {
			specs = append(specs, ch.Spec)
		}
		if aerr := h.engine.vols.AddArchive(arch); aerr != nil {
			fail(http.StatusInternalServerError, "保存上传恢复记录失败")
			return
		}
		recorded[id] = true
		tok, terr := h.engine.volUploads.IssueToken(app.ID, id)
		if terr != nil {
			fail(http.StatusInternalServerError, "生成下载链接失败")
			return
		}
		items = append(items, &uploadedRestoreItem{
			Arch:        arch,
			DownloadURL: fmt.Sprintf("%s/api/v1/volume-uploads/%s", baseURL, tok),
			ExpectedSHA: sc.SHA256,
			Specs:       specs,
		})
	}

	go func() {
		defer h.engine.releaseVolumeSlot(app.ID)
		if _, rerr := h.engine.runRestoreUploadedBatch(app, items, RestoreVolumeOptions{CleanRestore: true}); rerr != nil {
			h.engine.log.Warn("volume upload restore failed", "app", app.ID, "archives", len(items), "err", rerr)
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{
		"ok": true, "archive_id": ids[0], "archive_ids": ids,
		"message": "恢复任务已开始：校验全部文件后将自动停机、一次性恢复全部目标卷并在完成后重启应用容器",
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
