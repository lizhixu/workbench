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

// MaxUploadBytes caps an uploaded archive. Restores are admin-only, but a
// bounded reader is still what keeps a truncated upload from filling the disk.
const MaxUploadBytes int64 = 2 << 30

// Handlers exposes the backup/restore REST endpoints (AGENTS.md B.8.2).
type Handlers struct {
	store *Store
}

// NewHandlers creates backup Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts the backup routes. Every route here reads or rewrites the
// whole control-plane dataset, so the caller is expected to pass an
// admin-gated, audited group.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/system/backups", h.list)
	rg.POST("/system/backup", h.create)
	rg.GET("/system/backups/:name/download", h.download)
	rg.DELETE("/system/backups/:name", h.remove)
	rg.POST("/system/backups/:name/restore", h.restoreExisting)
	rg.POST("/system/restore", h.restoreUpload)
}

func (h *Handlers) list(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.List()})
}

func (h *Handlers) create(c *gin.Context) {
	var body struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&body)

	meta, err := h.store.Create(c.GetString("username"), strings.TrimSpace(body.Note), false)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": meta})
}

func (h *Handlers) download(c *gin.Context) {
	name := c.Param("name")
	p, err := h.store.Path(name)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
	c.Header("Content-Type", "application/gzip")
	c.File(p)
}

func (h *Handlers) remove(c *gin.Context) {
	if err := h.store.Delete(c.Param("name")); err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) restoreExisting(c *gin.Context) {
	res, err := h.store.RestoreFile(c.Param("name"), c.GetString("username"))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":    res,
		"message": "恢复完成，请重启控制端进程以加载恢复后的数据",
	})
}

// restoreUpload accepts an archive uploaded from the browser, either as a
// multipart "file" field or as a raw request body.
func (h *Handlers) restoreUpload(c *gin.Context) {
	var (
		src  io.Reader
		note string
	)

	fh, err := c.FormFile("file")
	switch {
	case err == nil:
		f, openErr := fh.Open()
		if openErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传文件失败: " + openErr.Error()})
			return
		}
		defer f.Close()
		src = f
		note = "upload:" + filepath.Base(fh.Filename)
	case errors.Is(err, http.ErrMissingFile), errors.Is(err, http.ErrNotMultipart):
		src = c.Request.Body
		note = "upload:raw-body"
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "读取上传文件失败: " + err.Error()})
		return
	}

	res, err := h.store.Restore(io.LimitReader(src, MaxUploadBytes), c.GetString("username"), note)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":    res,
		"message": "恢复完成，请重启控制端进程以加载恢复后的数据",
	})
}

// fail maps store errors onto status codes: a disabled store is a server
// configuration problem, a bad name or archive is the caller's.
func (h *Handlers) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrDisabled):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case strings.Contains(err.Error(), "not found"):
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
	case strings.Contains(err.Error(), "invalid backup name"),
		strings.Contains(err.Error(), "refusing"),
		strings.Contains(err.Error(), "not a gzip"),
		strings.Contains(err.Error(), "truncated"):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
