package ai

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handlers bundles the AI diagnostics REST endpoints.
type Handlers struct {
	assistant *Assistant
}

// NewHandlers creates AI Handlers.
func NewHandlers(assistant *Assistant) *Handlers {
	return &Handlers{assistant: assistant}
}

// Register mounts AI routes on the given authenticated group.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.POST("/ai/diagnose", h.diagnose)
	rg.POST("/ai/analyze-exec", h.analyzeExec)
	rg.POST("/ai/nl2command", h.nl2command)
	rg.GET("/ai/config", h.getConfig)
	rg.PUT("/ai/config", h.setConfig)
	rg.POST("/ai/test", h.testConnection)
	rg.POST("/ai/scan-report", h.analyzeScanReport)
	rg.POST("/ai/scan-report/followup", h.scanReportFollowup)
	rg.POST("/ai/chat", h.chat)
	rg.POST("/ai/ops-report", h.generateOpsReport)
	rg.GET("/ai/ops-reports", h.listOpsReports)
	rg.GET("/ai/ops-reports/:id", h.getOpsReport)
}

func (h *Handlers) diagnose(c *gin.Context) {
	var req DiagnoseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.assistant.Diagnose(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func (h *Handlers) analyzeExec(c *gin.Context) {
	var body struct {
		Command  string `json:"command"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int32  `json:"exit_code"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := h.assistant.AnalyzeExecResult(c.Request.Context(), body.Command, body.Stdout, body.Stderr, body.ExitCode)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func (h *Handlers) nl2command(c *gin.Context) {
	var body struct {
		Prompt string `json:"prompt"`
		HostID string `json:"host_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt required"})
		return
	}
	resp, err := h.assistant.Nl2Command(c.Request.Context(), body.Prompt, body.HostID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func (h *Handlers) getConfig(c *gin.Context) {
	cfg := h.assistant.Config()
	// Don't expose the API key in full.
	if cfg.APIKey != "" {
		cfg.APIKey = "********"
	}
	c.JSON(http.StatusOK, gin.H{"data": cfg})
}

func (h *Handlers) setConfig(c *gin.Context) {
	var cfg Config
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Preserve API key if masked.
	if cfg.APIKey == "********" {
		cfg.APIKey = h.assistant.Config().APIKey
	}
	if err := h.assistant.SetConfig(cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) testConnection(c *gin.Context) {
	var cfg Config
	_ = c.ShouldBindJSON(&cfg)

	// If unprovided or key is masked, merge with current config
	current := h.assistant.Config()
	if cfg.BaseURL == "" {
		cfg.BaseURL = current.BaseURL
	}
	if cfg.Model == "" {
		cfg.Model = current.Model
	}
	if cfg.APIKey == "" || cfg.APIKey == "********" {
		cfg.APIKey = current.APIKey
	}

	result, err := h.assistant.TestConnection(c.Request.Context(), cfg)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"ok": false, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": result})
}

func (h *Handlers) analyzeScanReport(c *gin.Context) {
	var body struct {
		ScanType     string `json:"scan_type"`
		FindingsJSON string `json:"findings_json"`
		HostID       string `json:"host_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.FindingsJSON == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "findings_json required"})
		return
	}
	resp, err := h.assistant.AnalyzeScanReport(c.Request.Context(), body.ScanType, body.FindingsJSON, body.HostID)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func (h *Handlers) scanReportFollowup(c *gin.Context) {
	var body struct {
		ReportContext string `json:"report_context"`
		Question      string `json:"question"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Question == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "question required"})
		return
	}
	resp, err := h.assistant.ChatFollowup(c.Request.Context(), body.ReportContext, body.Question)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func (h *Handlers) chat(c *gin.Context) {
	var body struct {
		Question string        `json:"question"`
		History  []ChatMessage `json:"history"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Question == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "question required"})
		return
	}
	resp, err := h.assistant.Chat(c.Request.Context(), body.Question, body.History)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func (h *Handlers) generateOpsReport(c *gin.Context) {
	var req OpsReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Allow empty body (defaults: all hosts, 24h).
		req = OpsReportRequest{}
	}
	resp, err := h.assistant.GenerateOpsReport(c.Request.Context(), req)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": resp})
}

func (h *Handlers) listOpsReports(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.assistant.ListOpsReports()})
}

func (h *Handlers) getOpsReport(c *gin.Context) {
	report, ok := h.assistant.GetOpsReport(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "report not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": report})
}
