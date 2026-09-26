package audit

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// Handlers exposes the audit query and export REST API. The routes are
// registered on an authenticated group by the api router; viewing the audit
// trail is restricted to admins at registration time.
type Handlers struct {
	store *Store
}

// NewHandlers creates audit handlers backed by the given store.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts the audit routes on the given (authenticated) group.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/audit", h.list)
	rg.GET("/audit/export", h.exportCSV)
}

func (h *Handlers) list(c *gin.Context) {
	q, err := queryFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	entries, stats := h.store.List(q)
	if entries == nil {
		entries = []Entry{}
	}
	c.JSON(http.StatusOK, gin.H{
		"data":  entries,
		"stats": stats,
		"page": gin.H{
			"offset": q.Offset,
			"limit":  q.Limit,
		},
	})
}

func (h *Handlers) exportCSV(c *gin.Context) {
	q, err := queryFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	filename := fmt.Sprintf("watchman-audit-%s.csv", time.Now().Format("20060102-150405"))
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	// UTF-8 BOM so Excel opens the CSV with proper Chinese rendering.
	body := append([]byte{0xEF, 0xBB, 0xBF}, h.store.ToCSV(q)...)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", body)
}

func queryFromContext(c *gin.Context) (Query, error) {
	q := Query{
		Username: c.Query("username"),
		Action:   c.Query("action"),
		TargetID: c.Query("target"),
		Result:   c.Query("result"),
		Risk:     c.Query("risk"),
	}
	limit, err := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if err != nil || limit <= 0 || limit > 500 {
		limit = 50
	}
	offset, err := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if err != nil || offset < 0 {
		offset = 0
	}
	q.Limit, q.Offset = limit, offset

	if v := c.Query("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, fmt.Errorf("invalid from time: %w", err)
		}
		q.From = t
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, fmt.Errorf("invalid to time: %w", err)
		}
		q.To = t
	}
	return q, nil
}
