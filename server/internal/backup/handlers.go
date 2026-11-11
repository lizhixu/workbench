package backup

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

const MaxUploadBytes = MaxArchiveBytes

type Handlers struct{ store *Store }

func NewHandlers(store *Store) *Handlers { return &Handlers{store: store} }

func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/system/backups", h.list)
	rg.GET("/system/backups/status", h.status)
	rg.GET("/system/backups/policy", h.getPolicy)
	rg.PUT("/system/backups/policy", h.putPolicy)
	rg.POST("/system/backup", h.create)
	rg.GET("/system/backups/:name/download", h.download)
	rg.DELETE("/system/backups/:name", h.remove)
	rg.POST("/system/backups/:name/restore", h.restoreExisting)
	rg.POST("/system/restore", h.restoreUpload)
	rg.DELETE("/system/restore/pending", h.cancelPending)
}
func (h *Handlers) list(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"data": h.store.List()}) }
func (h *Handlers) status(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.Status(), "pending": h.store.hasPending(), "max_upload_bytes": MaxUploadBytes, "max_data_bytes": MaxRestoreBytes})
}
func (h *Handlers) getPolicy(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"data": h.store.Policy()}) }
func (h *Handlers) putPolicy(c *gin.Context) {
	var req Policy
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "请求体格式错误"})
		return
	}
	if err := h.store.SavePolicy(req); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"data": h.store.Policy()})
}
func (h *Handlers) create(c *gin.Context) {
	var req struct {
		Note     string `json:"note"`
		TargetID string `json:"target_id"`
		Async    bool   `json:"async"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(400, gin.H{"error": "请求体格式错误"})
		return
	}
	if len([]rune(req.Note)) > 200 {
		c.JSON(400, gin.H{"error": "备注不能超过 200 字"})
		return
	}
	if req.Async || req.TargetID != "" {
		task, err := h.store.StartBackup(c.GetString("username"), strings.TrimSpace(req.Note), req.TargetID, false)
		if err != nil {
			h.fail(c, err)
			return
		}
		c.JSON(202, gin.H{"data": task})
		return
	}
	meta, err := h.store.Create(c.GetString("username"), strings.TrimSpace(req.Note), false)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"data": meta})
}
func (h *Handlers) download(c *gin.Context) {
	p, err := h.store.Path(c.Param("name"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
	c.Header("Content-Type", "application/gzip")
	c.File(p)
}
func (h *Handlers) remove(c *gin.Context) {
	warning, err := h.store.Delete(c.Param("name"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true, "warning": warning})
}
func (h *Handlers) restoreExisting(c *gin.Context) {
	var req struct {
		VaultPass string `json:"vault_pass"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(400, gin.H{"error": "请求体格式错误"})
		return
	}
	task, err := h.store.StartRestoreFile(c.Param("name"), c.GetString("username"), req.VaultPass)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(202, gin.H{"data": task})
}
func (h *Handlers) restoreUpload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxUploadBytes+(1<<20))
	var src io.Reader = c.Request.Body
	passphrase := ""
	if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/") {
		mr, err := c.Request.MultipartReader()
		if err != nil {
			c.JSON(400, gin.H{"error": "上传格式错误"})
			return
		}
		// Metadata parts are streamed before the file part, so vault_pass must
		// come first: the loop stops at the file part and hands it straight to
		// the disk-copying task, never scanning what follows it.
		found := false
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				h.fail(c, err)
				return
			}
			if part.FormName() == "file" && part.FileName() != "" {
				src = part
				defer part.Close()
				found = true
				break
			}
			if part.FormName() == "vault_pass" {
				raw, err := io.ReadAll(io.LimitReader(part, 4097))
				if err != nil {
					h.fail(c, err)
					return
				}
				if len(raw) > 4096 {
					c.JSON(400, gin.H{"error": "Vault 密钥过长"})
					return
				}
				passphrase = string(raw)
			}
			part.Close()
		}
		if !found {
			c.JSON(400, gin.H{"error": "请选择备份归档文件"})
			return
		}
	}
	task, err := h.store.StartRestoreUpload(src, c.GetString("username"), passphrase)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(202, gin.H{"data": task})
}
func (h *Handlers) cancelPending(c *gin.Context) {
	if err := h.store.CancelPending(); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
func (h *Handlers) fail(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, ErrDisabled):
		c.JSON(503, gin.H{"error": err.Error()})
	case errors.Is(err, ErrBusy), errors.Is(err, ErrPending):
		c.JSON(409, gin.H{"error": err.Error()})
	case errors.As(err, &tooLarge):
		c.JSON(413, gin.H{"error": "上传归档超过容量上限"})
	case strings.Contains(err.Error(), "not found"):
		c.JSON(404, gin.H{"error": err.Error()})
	default:
		c.JSON(400, gin.H{"error": err.Error()})
	}
}
