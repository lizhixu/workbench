package githubapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/settings"
)

// Handlers serves the GitHub App management API and the public event
// endpoint. Management routes ride the admin-only audited write group;
// reads ride the authed group; the webhook endpoint is public and relies
// solely on HMAC signature verification, mirroring the per-app webhook.
type Handlers struct {
	store    *Store
	client   *Client
	provider *TokenProvider
	settings *settings.Store
	trigger  PushTrigger

	mu        sync.Mutex
	states    map[string]time.Time // manifest CSRF states -> expiry
	delivered map[string]time.Time // GitHub delivery IDs -> seen time
}

func NewHandlers(store *Store, client *Client, provider *TokenProvider, settingsStore *settings.Store, trigger PushTrigger) *Handlers {
	return &Handlers{
		store:     store,
		client:    client,
		provider:  provider,
		settings:  settingsStore,
		trigger:   trigger,
		states:    map[string]time.Time{},
		delivered: map[string]time.Time{},
	}
}

// Register mounts the routes. readRG is the authed group, writeRG the
// admin-only audited group, publicRG the unauthenticated /api/v1 group.
func (h *Handlers) Register(readRG, writeRG, publicRG *gin.RouterGroup) {
	readRG.GET("/github-app/status", h.status)
	readRG.GET("/github-app/installations/repos", h.installationRepos)
	readRG.GET("/github-app/repos/:owner/:repo/branches", h.repoBranches)

	writeRG.POST("/github-app/manifest", h.createManifest)
	writeRG.POST("/github-app/manifest/exchange", h.exchangeManifest)
	writeRG.POST("/github-app/config", h.saveManualConfig)
	writeRG.DELETE("/github-app", h.disconnect)
	writeRG.POST("/github-app/installations/sync", h.syncInstallations)

	publicRG.POST("/github-app/webhook", h.webhook)
}

// panelBaseURL resolves the panel's public base URL: the configured
// server.public_url when set, otherwise the request's own scheme+host
// (same derivation as the per-app webhook URL builder).
func (h *Handlers) panelBaseURL(c *gin.Context) (string, bool) {
	if h.settings != nil {
		if pub := strings.TrimRight(h.settings.PanelSecurity().PublicURL, "/"); pub != "" {
			return pub, true
		}
	}
	scheme := "http"
	if c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := c.Request.Host
	if host == "" {
		return "", false
	}
	return scheme + "://" + host, false
}

func (h *Handlers) webhookURL(c *gin.Context) string {
	base, _ := h.panelBaseURL(c)
	if base == "" {
		return ""
	}
	return base + "/api/v1/github-app/webhook"
}

func (h *Handlers) status(c *gin.Context) {
	cfg, ok := h.store.GetConfig()
	resp := gin.H{
		"configured":    ok,
		"webhook_url":   h.webhookURL(c),
		"installations": h.store.Installations(),
	}
	if ok {
		resp["app_id"] = cfg.AppID
		resp["slug"] = cfg.Slug
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handlers) installationRepos(c *gin.Context) {
	type repoRow struct {
		Account  string `json:"account"`
		FullName string `json:"full_name"`
	}
	var rows []repoRow
	for _, inst := range h.store.Installations() {
		if inst.Suspended {
			continue
		}
		for _, r := range inst.Repos {
			rows = append(rows, repoRow{Account: inst.AccountLogin, FullName: r})
		}
	}
	if rows == nil {
		rows = []repoRow{}
	}
	c.JSON(http.StatusOK, gin.H{"repos": rows})
}

func (h *Handlers) installationToken(ctx context.Context, installationID int64) (string, error) {
	cfg, ok := h.store.GetConfig()
	if !ok {
		return "", errNotConfigured
	}
	jwt, err := MintJWT(cfg.AppID, cfg.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	tok, err := h.client.CreateInstallationToken(ctx, jwt, installationID)
	if err != nil {
		return "", err
	}
	return tok.Token, nil
}

func (h *Handlers) repoBranches(c *gin.Context) {
	owner := strings.ToLower(c.Param("owner"))
	repo := strings.ToLower(c.Param("repo"))
	norm := owner + "/" + repo
	inst, ok := h.store.FindInstallationForRepo(norm)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "仓库不在 GitHub App 安装范围内，请先同步安装"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	tok, err := h.installationToken(ctx, inst.ID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "获取安装令牌失败: " + err.Error()})
		return
	}
	branches, err := h.client.ListRepoBranches(ctx, tok, owner, repo)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "获取分支列表失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"branches": branches})
}

func (h *Handlers) createManifest(c *gin.Context) {
	var req struct {
		Name   string `json:"name"`
		Target string `json:"target"` // personal | organization
		Org    string `json:"org"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体无效"})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		// 名称留空时生成一个带随机后缀的默认值：GitHub App 名称全站唯一，
		// 纯固定名（如 watchman）大概率已被占用，随机后缀才能一次建成。
		name, err := DefaultAppName()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "生成 App 名称失败"})
			return
		}
		req.Name = name
	}
	if req.Target == "organization" && strings.TrimSpace(req.Org) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "组织目标需要填写组织名"})
		return
	}
	if req.Target != "" && req.Target != "personal" && req.Target != "organization" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "目标类型无效"})
		return
	}
	base, configured := h.panelBaseURL(c)
	if base == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无法确定面板公网地址，请先在系统设置中配置 server.public_url"})
		return
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "生成随机状态失败"})
		return
	}
	state := hex.EncodeToString(raw)
	h.mu.Lock()
	h.states[state] = time.Now().Add(15 * time.Minute)
	// Opportunistic cleanup so the map cannot grow without bound.
	for s, exp := range h.states {
		if time.Now().After(exp) {
			delete(h.states, s)
		}
	}
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"manifest":              BuildManifest(base, req.Name, state),
		"name":                  req.Name,
		"state":                 state,
		"submit_url":            ManifestSubmitURL(req.Target, strings.TrimSpace(req.Org)),
		"panel_url":             base,
		"public_url_configured": configured,
	})
}

func (h *Handlers) exchangeManifest(c *gin.Context) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" || req.State == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 code 或 state"})
		return
	}
	h.mu.Lock()
	exp, ok := h.states[req.State]
	if ok {
		delete(h.states, req.State)
	}
	h.mu.Unlock()
	if !ok || time.Now().After(exp) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "state 已过期或无效，请重新创建 GitHub App"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	res, err := h.client.ExchangeManifestCode(ctx, req.Code)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "换取 App 配置失败: " + err.Error()})
		return
	}
	// Validate the private key actually signs before persisting it.
	if _, err := MintJWT(res.ID, res.PEM); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "GitHub 返回的私钥无法使用: " + err.Error()})
		return
	}
	if err := h.store.SetConfig(&Config{
		AppID: res.ID, Slug: res.Slug,
		PrivateKeyPEM: res.PEM, WebhookSecret: res.WebhookSecret,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存 App 配置失败: " + err.Error()})
		return
	}
	// A new identity must never reuse tokens minted under the old one.
	if h.provider != nil {
		h.provider.ResetCache()
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "app_id": res.ID, "slug": res.Slug})
}

func (h *Handlers) saveManualConfig(c *gin.Context) {
	var req struct {
		AppID         int64  `json:"app_id"`
		PrivateKeyPEM string `json:"private_key"`
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体无效"})
		return
	}
	if req.AppID <= 0 || strings.TrimSpace(req.PrivateKeyPEM) == "" || req.WebhookSecret == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写 App ID、私钥与 Webhook Secret"})
		return
	}
	if _, err := MintJWT(req.AppID, req.PrivateKeyPEM); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "私钥无法使用: " + err.Error()})
		return
	}
	if err := h.store.SetConfig(&Config{
		AppID: req.AppID, PrivateKeyPEM: req.PrivateKeyPEM, WebhookSecret: req.WebhookSecret,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存 App 配置失败: " + err.Error()})
		return
	}
	if h.provider != nil {
		h.provider.ResetCache()
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "app_id": req.AppID})
}

func (h *Handlers) disconnect(c *gin.Context) {
	if err := h.store.Clear(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "断开失败: " + err.Error()})
		return
	}
	if h.provider != nil {
		h.provider.ResetCache()
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) syncInstallations(c *gin.Context) {
	if h.provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "GitHub App 服务未初始化"})
		return
	}
	if err := h.provider.RefreshRepos(); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "同步安装失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"installations": h.store.Installations()})
}

func (h *Handlers) webhook(c *gin.Context) {
	cfg, ok := h.store.GetConfig()
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "github app 未配置"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read body"})
		return
	}
	if !VerifySignature(cfg.WebhookSecret, body, c.GetHeader("X-Hub-Signature-256")) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "签名校验失败"})
		return
	}
	// Delivery de-duplication: GitHub redelivers on failures, and a
	// redelivery must not start a second deployment.
	if delivery := c.GetHeader("X-GitHub-Delivery"); delivery != "" {
		h.mu.Lock()
		if _, dup := h.delivered[delivery]; dup {
			h.mu.Unlock()
			c.JSON(http.StatusOK, gin.H{"ok": true, "duplicate": true})
			return
		}
		h.delivered[delivery] = time.Now()
		if len(h.delivered) > 5000 {
			for id, at := range h.delivered {
				if time.Since(at) > time.Hour {
					delete(h.delivered, id)
				}
			}
		}
		h.mu.Unlock()
	}
	switch c.GetHeader("X-GitHub-Event") {
	case "ping":
		c.JSON(http.StatusOK, gin.H{"ok": true, "ping": true})
	case "push":
		h.webhookPush(c, body)
	case "installation", "installation_repositories":
		h.webhookInstallation(c, body)
	default:
		c.JSON(http.StatusOK, gin.H{"ok": true, "ignored": true})
	}
}

func (h *Handlers) webhookPush(c *gin.Context, body []byte) {
	var raw struct {
		Deleted bool `json:"deleted"`
	}
	_ = json.Unmarshal(body, &raw)
	if raw.Deleted {
		c.JSON(http.StatusOK, gin.H{"ok": true, "skipped": "branch deleted"})
		return
	}
	ev, err := ParsePushEvent(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Only branch pushes deploy. Tag pushes (refs/tags/...) must not be
	// mistaken for a branch of the same name, and other refs are ignored.
	if !strings.HasPrefix(ev.Ref, "refs/heads/") {
		c.JSON(http.StatusOK, gin.H{"ok": true, "skipped": "not a branch push"})
		return
	}
	branch := ev.Branch()
	if branch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot determine pushed branch"})
		return
	}
	if h.trigger == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "部署触发器未初始化"})
		return
	}
	results := h.trigger.TriggerPush(ev.RepoFullName, branch)
	var triggered, skipped []TriggerResult
	for _, r := range results {
		if r.Skipped != "" {
			skipped = append(skipped, r)
		} else {
			triggered = append(triggered, r)
		}
	}
	if triggered == nil {
		triggered = []TriggerResult{}
	}
	if skipped == nil {
		skipped = []TriggerResult{}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "triggered": triggered, "skipped": skipped})
}

func (h *Handlers) webhookInstallation(c *gin.Context, body []byte) {
	var payload struct {
		Action       string `json:"action"`
		Installation *struct {
			ID      int64 `json:"id"`
			Account *struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
			TargetType string `json:"target_type"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Installation == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "安装事件解析失败"})
		return
	}
	inst := payload.Installation
	switch payload.Action {
	case "deleted":
		if err := h.store.RemoveInstallation(inst.ID); err != nil {
			// Return 5xx so GitHub redelivers; silently dropping the
			// uninstall would leave stale coverage behind.
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除安装记录失败: " + err.Error()})
			return
		}
	case "suspend", "unsuspend", "created", "new_permissions_accepted":
		rec := Installation{
			ID:         inst.ID,
			TargetType: inst.TargetType,
			Suspended:  payload.Action == "suspend",
		}
		if inst.Account != nil {
			rec.AccountLogin = inst.Account.Login
			rec.AccountType = inst.Account.Type
		}
		if err := h.store.UpsertInstallation(rec); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存安装记录失败: " + err.Error()})
			return
		}
		// Refresh the repo list for this installation so push routing
		// works without waiting for a manual sync. Async: the upsert
		// above is already persisted, and GitHub deliveries time out
		// long before a full multi-installation refresh would finish.
		if h.provider != nil && payload.Action != "suspend" {
			go h.provider.RefreshRepos()
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
