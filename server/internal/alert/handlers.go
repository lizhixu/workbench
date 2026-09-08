package alert

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Handlers bundles the alert REST endpoints.
type Handlers struct {
	store *Store
}

// NewHandlers creates alert Handlers.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{store: store}
}

// Register mounts alert routes on the given authenticated group.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/alerts/rules", h.listRules)
	rg.POST("/alerts/rules", h.createRule)
	rg.PUT("/alerts/rules/:id", h.updateRule)
	rg.DELETE("/alerts/rules/:id", h.deleteRule)
	rg.GET("/alerts/events", h.listEvents)
	rg.POST("/alerts/events/ack-all", h.ackAllEvents)
	rg.POST("/alerts/events/:id/ack", h.ackEvent)
	rg.DELETE("/alerts/events/:id", h.deleteEvent)
	rg.DELETE("/alerts/events", h.clearEvents)
		rg.GET("/alerts/webhook", h.getWebhook)
		rg.PUT("/alerts/webhook", h.setWebhook)
		rg.POST("/alerts/webhook/test", h.testWebhook)
}

func (h *Handlers) listRules(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.ListRules()})
}

func (h *Handlers) createRule(c *gin.Context) {
	var r Rule
	if err := c.ShouldBindJSON(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	r.ID = "" // server-generated
	if err := h.store.CreateRule(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": r})
}

func (h *Handlers) updateRule(c *gin.Context) {
	var r Rule
	if err := c.ShouldBindJSON(&r); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	r.ID = c.Param("id")
	if err := h.store.UpdateRule(&r); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": r})
}

func (h *Handlers) deleteRule(c *gin.Context) {
	if err := h.store.DeleteRule(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) listEvents(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.ListEvents(200)})
}

func (h *Handlers) ackEvent(c *gin.Context) {
	if err := h.store.AckEvent(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) ackAllEvents(c *gin.Context) {
	if err := h.store.AckAllEvents(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) deleteEvent(c *gin.Context) {
	if err := h.store.DeleteEvent(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) clearEvents(c *gin.Context) {
	resolvedOnly := c.Query("resolved_only") == "true"
	if err := h.store.ClearEvents(resolvedOnly); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) getWebhook(c *gin.Context) {
	w := h.store.GetWebhook()
	c.JSON(http.StatusOK, gin.H{"data": w})
}

func (h *Handlers) setWebhook(c *gin.Context) {
	var w WebhookConfig
	if err := c.ShouldBindJSON(&w); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// If the secret is the mask, preserve the existing one.
	if w.Secret == "********" {
		existing := h.store.GetWebhook()
		w.Secret = existing.Secret
	}
	if err := h.store.SetWebhook(w); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) testWebhook(c *gin.Context) {
	var body struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
	}
	_ = c.ShouldBindJSON(&body)

	cfg := h.store.GetWebhook()
	if strings.TrimSpace(body.URL) != "" {
		cfg.URL = strings.TrimSpace(body.URL)
	}
	if body.Secret != "" && body.Secret != "********" {
		cfg.Secret = strings.TrimSpace(body.Secret)
	}
	cfg.Enabled = true // override for testing

	if cfg.URL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": "Webhook URL 不能为空"})
		return
	}

	testEvent := &Event{
		ID:               "test-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		RuleID:           "__test__",
		RuleName:         "Webhook 连通性测试",
		Severity:         SeverityInfo,
		HostID:           "control-server",
		Hostname:         "watchman-console",
		Message:          "这是一条来自自研云堡垒机 Watchman 的自动化告警 Webhook 连通性测试消息。配置有效，告警分发机制正常！",
		FiredAt:          time.Now(),
		AIInterpretation: "测试消息发送成功，目标告警通道响应正常，生产告警触发时将自动流转至该地址。",
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 12*time.Second)
	defer cancel()

	statusCode, respBody, err := SendWebhook(ctx, cfg, testEvent)
	durationMs := time.Since(start).Milliseconds()

	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"ok":          false,
			"platform":    string(DetectPlatform(cfg.URL)),
			"status_code": statusCode,
			"duration_ms": durationMs,
			"error":       err.Error(),
			"response":    respBody,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"platform":    string(DetectPlatform(cfg.URL)),
		"status_code": statusCode,
		"duration_ms": durationMs,
		"message":     "Webhook 推送成功",
		"response":    respBody,
	})
}