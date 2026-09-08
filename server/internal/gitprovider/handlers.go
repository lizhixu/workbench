package gitprovider

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Handlers exposes Git provider integration endpoints.
type Handlers struct {
	store  *Store
	client *Client
}

// NewHandlers creates a Git provider handler.
func NewHandlers(store *Store) *Handlers {
	return &Handlers{
		store:  store,
		client: NewClient(),
	}
}

// Register mounts routes.
func (h *Handlers) Register(readRG, writeRG *gin.RouterGroup) {
	// Status & queries
	readRG.GET("/git/providers", h.getProviders)
	readRG.GET("/git/github/repos", h.listRepos)
	readRG.GET("/git/github/repos/:owner/:repo/branches", h.listBranches)

	// Auth management
	writeRG.POST("/git/github/token", h.setToken)
	writeRG.DELETE("/git/github", h.deleteAccount)
}

func (h *Handlers) getProviders(c *gin.Context) {
	acc, hasGitHub := h.store.GetAccount()
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"github": gin.H{
				"connected": hasGitHub,
				"account":   acc.Redacted(),
			},
		},
	})
}

type setTokenReq struct {
	Token string `json:"token" binding:"required"`
}

func (h *Handlers) setToken(c *gin.Context) {
	var req setTokenReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供 GitHub Access Token"})
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Token 不能为空"})
		return
	}

	acc, err := h.client.VerifyToken(c.Request.Context(), token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "GitHub 校验失败: " + err.Error()})
		return
	}

	if err := h.store.SetAccount(acc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":   true,
		"data": acc.Redacted(),
	})
}

func (h *Handlers) deleteAccount(c *gin.Context) {
	if err := h.store.DeleteAccount(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) listRepos(c *gin.Context) {
	token, ok := h.store.GetToken()
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "尚未授权连接 GitHub，请先绑定账号"})
		return
	}

	q := c.Query("q")
	repos, err := h.client.ListRepositories(c.Request.Context(), token, q)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "获取 GitHub 仓库失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": repos})
}

func (h *Handlers) listBranches(c *gin.Context) {
	token, ok := h.store.GetToken()
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "尚未授权连接 GitHub，请先绑定账号"})
		return
	}

	owner := c.Param("owner")
	repo := c.Param("repo")
	if owner == "" || repo == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 owner 或 repo 参数"})
		return
	}

	branches, err := h.client.ListBranches(c.Request.Context(), token, owner, repo)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "获取分支列表失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": branches})
}
