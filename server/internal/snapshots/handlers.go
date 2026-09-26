package snapshots

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/rpc"
)

// Handlers mounts the backup job REST API.
type Handlers struct {
	reg    *rpc.Registry
	store  *Store
	engine *Engine
}

// NewHandlers creates the snapshot handlers.
func NewHandlers(reg *rpc.Registry, store *Store, engine *Engine) *Handlers {
	return &Handlers{reg: reg, store: store, engine: engine}
}

// Register mounts routes. Reads are open to every role; job mutations and
// restore require a host-writing role and are audited by the caller.
func (h *Handlers) Register(readRG, writeRG *gin.RouterGroup) {
	readRG.GET("/backups/jobs", h.listJobs)
	readRG.GET("/backups/jobs/:id/archives", h.listArchives)
	readRG.GET("/backups/s3-targets", h.listS3Targets)

	writeRG.POST("/backups/jobs", h.createJob)
	writeRG.PUT("/backups/jobs/:id", h.updateJob)
	writeRG.DELETE("/backups/jobs/:id", h.deleteJob)
	writeRG.POST("/backups/jobs/:id/run", h.runJob)
	writeRG.POST("/backups/jobs/:id/archives/:archiveID/restore", h.restoreArchive)
	writeRG.DELETE("/backups/jobs/:id/archives/:archiveID", h.deleteArchive)

	// S3 storage targets
	writeRG.POST("/backups/s3-targets", h.createS3Target)
	writeRG.PUT("/backups/s3-targets/:id", h.updateS3Target)
	writeRG.DELETE("/backups/s3-targets/:id", h.deleteS3Target)
	writeRG.POST("/backups/s3-targets/:id/test", h.testS3Target)
}

type jobReq struct {
	Name          string `json:"name"`
	HostID        string `json:"host_id"`
	Kind          string `json:"kind"` // dir | volume | database
	Target        string `json:"target"`
	DBType        string `json:"db_type"`
	DBName        string `json:"db_name"`
	StorageTarget string `json:"storage_target"` // "default" | "local" | "s3_xxxx"
	Retention     int    `json:"retention"`
	Cron          string `json:"cron"`
	Enabled       bool   `json:"enabled"`
}

func (r *jobReq) validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("任务名称不能为空")
	}
	if r.HostID == "" {
		return fmt.Errorf("必须选择目标主机")
	}
	switch JobKind(r.Kind) {
	case KindDir:
		if r.Target == "" {
			return fmt.Errorf("目录备份必须填写目录路径")
		}
		if strings.Contains(r.Target, "..") || !strings.HasPrefix(r.Target, "/") {
			return fmt.Errorf("目录必须是绝对路径且不能包含 ..")
		}
	case KindVolume:
		if r.Target == "" {
			return fmt.Errorf("卷备份必须填写卷名")
		}
	case KindDatabase:
		if r.Target == "" {
			return fmt.Errorf("数据库备份必须填写容器名")
		}
		switch strings.ToLower(r.DBType) {
		case "mysql", "postgres", "redis", "mongo":
		default:
			return fmt.Errorf("数据库类型必须是 mysql/postgres/redis/mongo")
		}
	default:
		return fmt.Errorf("备份类型必须是 dir/volume/database")
	}
	if r.Retention < 0 || r.Retention > 100 {
		return fmt.Errorf("保留份数必须在 0-100 之间")
	}
	return nil
}

func (h *Handlers) createJob(c *gin.Context) {
	var req jobReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := req.validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.reg.Hub(req.HostID) == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标主机 Agent 不在线"})
		return
	}
	retention := req.Retention
	if retention == 0 {
		retention = 7
	}
		job := &Job{
			ID:            "bk_" + randomToken(8),
			Name:          strings.TrimSpace(req.Name),
			HostID:        req.HostID,
			Kind:          JobKind(req.Kind),
			Target:        strings.TrimSpace(req.Target),
			DBType:        strings.ToLower(req.DBType),
			DBName:        strings.TrimSpace(req.DBName),
			StorageTarget: strings.TrimSpace(req.StorageTarget),
			Retention:     retention,
			Schedule:      Schedule{Cron: strings.TrimSpace(req.Cron)},
			Enabled:       req.Enabled,
			CreatedAt:     time.Now(),
		}
	if err := h.store.PutJob(job); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": job})
}

func (h *Handlers) updateJob(c *gin.Context) {
	var req jobReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.UpdateJob(c.Param("id"), func(j *Job) error {
		if strings.TrimSpace(req.Name) != "" {
			j.Name = strings.TrimSpace(req.Name)
		}
		j.Target = strings.TrimSpace(req.Target)
		j.DBType = strings.ToLower(req.DBType)
		j.DBName = strings.TrimSpace(req.DBName)
		if req.StorageTarget != "" {
			j.StorageTarget = req.StorageTarget
		}
		if req.Retention > 0 {
			j.Retention = req.Retention
		}
		j.Schedule = Schedule{Cron: strings.TrimSpace(req.Cron)}
		j.Enabled = req.Enabled
		return nil
	}); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	job, _ := h.store.GetJob(c.Param("id"))
	c.JSON(http.StatusOK, gin.H{"data": job})
}

func (h *Handlers) deleteJob(c *gin.Context) {
	if err := h.store.DeleteJob(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) listJobs(c *gin.Context) {
	jobs := h.store.ListJobs()
	if jobs == nil {
		jobs = []*Job{}
	}
	c.JSON(http.StatusOK, gin.H{"data": jobs})
}

// runJob triggers one immediate backup run. Backups can take many minutes
// (DB dumps), so the HTTP call queues it and returns at once; the job's
// LastRun/LastError and the archives list reflect the outcome.
func (h *Handlers) runJob(c *gin.Context) {
	job, ok := h.store.GetJob(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	go func() {
		if _, err := h.engine.Run(job); err != nil {
			// Recorded on the job by the engine; nothing else to do here.
			_ = err
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "message": "备份任务已开始，请稍后查看历史归档"})
}

func (h *Handlers) listArchives(c *gin.Context) {
	offset := intQuery(c, "offset", 0)
	pageSize := intQuery(c, "page_size", 20)
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	list, total := h.store.ListArchives(c.Param("id"), offset, pageSize)
	if list == nil {
		list = []*Archive{}
	}
	c.JSON(http.StatusOK, gin.H{"data": list, "total": total, "offset": offset, "page_size": pageSize})
}

// restoreArchive restores one archive. Restoring overwrites live data, so it
// requires the same host-writing role as the other mutations (the caller
// wraps the route with the audit middleware).
func (h *Handlers) restoreArchive(c *gin.Context) {
	job, ok := h.store.GetJob(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	archives, _ := h.store.ListArchives(job.ID, 0, 0)
	var target *Archive
	for _, a := range archives {
		if a.ID == c.Param("archiveID") {
			target = a
			break
		}
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "archive not found"})
		return
	}
	go func() {
		if err := h.engine.Restore(job, target); err != nil {
			_ = err
		}
	}()
	c.JSON(http.StatusAccepted, gin.H{"ok": true,
		"message": "恢复任务已开始，恢复前会自动创建当前状态的安全副本"})
}

func (h *Handlers) deleteArchive(c *gin.Context) {
	if err := h.store.DeleteArchive(c.Param("id"), c.Param("archiveID")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- S3 Storage Targets Handlers ----

func (h *Handlers) listS3Targets(c *gin.Context) {
	targets := h.store.ListS3Targets()
	if targets == nil {
		targets = []*S3Target{}
	}
	c.JSON(http.StatusOK, gin.H{"data": targets})
}

type s3TargetReq struct {
	Name           string `json:"name" binding:"required"`
	Provider       string `json:"provider"`
	Endpoint       string `json:"endpoint" binding:"required"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket" binding:"required"`
	Prefix         string `json:"prefix"`
	AccessKey      string `json:"access_key" binding:"required"`
	SecretKey      string `json:"secret_key"`
	ForcePathStyle bool   `json:"force_path_style"`
	IsDefault      bool   `json:"is_default"`
}

func (h *Handlers) createS3Target(c *gin.Context) {
	var req s3TargetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写完整 S3 配置参数"})
		return
	}
	if strings.TrimSpace(req.SecretKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "SecretKey 不能为空"})
		return
	}

	target := &S3Target{
		ID:             "s3_" + randomToken(6),
		Name:           strings.TrimSpace(req.Name),
		Provider:       firstNonEmpty(req.Provider, "custom"),
		Endpoint:       strings.TrimSpace(req.Endpoint),
		Region:         firstNonEmpty(req.Region, "auto"),
		Bucket:         strings.TrimSpace(req.Bucket),
		Prefix:         strings.Trim(strings.TrimSpace(req.Prefix), "/"),
		AccessKey:      strings.TrimSpace(req.AccessKey),
		SecretKey:      strings.TrimSpace(req.SecretKey),
		ForcePathStyle: req.ForcePathStyle,
		IsDefault:      req.IsDefault,
		CreatedAt:      time.Now(),
	}

	if err := h.store.PutS3Target(target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": target.Redacted()})
}

func (h *Handlers) updateS3Target(c *gin.Context) {
	id := c.Param("id")
	target, ok := h.store.GetS3Target(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "s3 target not found"})
		return
	}

	var req s3TargetReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	target.Name = strings.TrimSpace(req.Name)
	if req.Provider != "" {
		target.Provider = req.Provider
	}
	target.Endpoint = strings.TrimSpace(req.Endpoint)
	if req.Region != "" {
		target.Region = req.Region
	}
	target.Bucket = strings.TrimSpace(req.Bucket)
	target.Prefix = strings.Trim(strings.TrimSpace(req.Prefix), "/")
	target.AccessKey = strings.TrimSpace(req.AccessKey)
	if req.SecretKey != "" && !strings.Contains(req.SecretKey, "••") {
		target.SecretKey = strings.TrimSpace(req.SecretKey)
	}
	target.ForcePathStyle = req.ForcePathStyle
	target.IsDefault = req.IsDefault

	if err := h.store.PutS3Target(target); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": target.Redacted()})
}

func (h *Handlers) deleteS3Target(c *gin.Context) {
	if err := h.store.DeleteS3Target(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) testS3Target(c *gin.Context) {
	target, ok := h.store.GetS3Target(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "s3 target not found"})
		return
	}
	client := NewS3Client(target)
	if err := client.TestBucket(c.Request.Context()); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "S3 存储桶连通性测试成功"})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func intQuery(c *gin.Context, name string, def int) int {
	raw := c.Query(name)
	if raw == "" {
		return def
	}
	n := 0
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
		if n > 1_000_000 {
			return def
		}
	}
	return n
}
