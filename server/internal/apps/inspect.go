package apps

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/auth"
	"watchman/server/internal/gitprovider"
	"watchman/server/internal/registry"
)

// ImageInspectFunc probes a container registry for an image's true runtime
// metadata (exposed ports, env, cmd, volumes). Injected from router wiring so
// handlers stay transport-agnostic.
type ImageInspectFunc func(ctx context.Context, image string) (*registry.ImageMetadata, error)

// RepoInspectFunc probes a GitHub repository's checked-in deployment files
// (docker-compose.yml / Dockerfile / .env.example) without cloning.
type RepoInspectFunc func(ctx context.Context, repoURL string) (*gitprovider.RepoMetadata, error)

// SetProbers injects the image / repository metadata probes used by the
// read-only inspect endpoints and the create-page auto-fill hints.
func (h *AppHandlers) SetProbers(image ImageInspectFunc, repo RepoInspectFunc) {
	h.imageInspector = image
	h.repoInspector = repo
}

// probeRateLimiter is a per-user fixed-window limiter for the metadata probe
// endpoints. Probing is anonymous-pull / token-backed GitHub API traffic, so
// an unauthenticated-by-role script must not be able to burn the stored
// GitHub token's rate quota: 30 probes per user per minute is far above the
// create page's blur-driven usage and far below scripted abuse.
type probeRateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*probeBucket
}

type probeBucket struct {
	count       int
	windowStart time.Time
}

func newProbeRateLimiter(limit int, window time.Duration) *probeRateLimiter {
	return &probeRateLimiter{limit: limit, window: window, buckets: map[string]*probeBucket{}}
}

func (l *probeRateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, b := range l.buckets {
		if now.Sub(b.windowStart) > 2*l.window {
			delete(l.buckets, k)
		}
	}
	b, ok := l.buckets[key]
	if !ok || now.Sub(b.windowStart) >= l.window {
		l.buckets[key] = &probeBucket{count: 1, windowStart: now}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}

var probeLimiter = newProbeRateLimiter(30, time.Minute)

// requireProbeRole rejects viewers (and unknown roles) from the metadata
// probe endpoints. The repo probe runs with the admin's stored GitHub token,
// so a viewer could otherwise read private repositories' compose files and
// env defaults through a guessable github.com/<owner>/<repo> URL. Fail
// closed: only admin and operator — the roles allowed to create apps —
// may probe.
func requireProbeRole(c *gin.Context) bool {
	role := auth.RoleOf(c)
	if role != auth.RoleAdmin && role != auth.RoleOperator {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅管理员与运维员可使用探测功能"})
		return false
	}
	return true
}

// checkProbeRate applies the per-user probe quota; it runs after the role
// gate so denied viewers do not consume quota.
func checkProbeRate(c *gin.Context) bool {
	key := auth.Username(c)
	if key == "" {
		key = c.ClientIP()
	}
	if !probeLimiter.allow(key) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "探测过于频繁，请稍后再试"})
		return false
	}
	return true
}

// inspectImageReq is the payload for POST /apps/inspect-image.
type inspectImageReq struct {
	Image string `json:"image" binding:"required"`
}

// inspectRepoReq is the payload for POST /apps/inspect-repo.
type inspectRepoReq struct {
	RepoURL string `json:"repo_url" binding:"required"`
}

// inspectImage probes the registry for the image's config blob and returns
// the extracted runtime metadata. Read-only and non-mutating, hence exempt
// from the operation audit (diagnostic class, AGENTS.md §7).
func (h *AppHandlers) inspectImage(c *gin.Context) {
	if !requireProbeRole(c) || !checkProbeRate(c) {
		return
	}
	var req inspectImageReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供镜像地址"})
		return
	}
	image := strings.TrimSpace(req.Image)
	if image == "" || len(image) > 512 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像地址为空或过长"})
		return
	}
	if h.imageInspector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "镜像探测能力未启用"})
		return
	}
	// Bound the whole probe independent of client disconnects.
	pctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	meta, err := h.imageInspector(pctx, image)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": meta})
}

// inspectRepo reads deployment-relevant files from a GitHub repository
// (compose / Dockerfile / env example) so the create form and the AI assistant
// can work with the repo's real ports and environment keys.
func (h *AppHandlers) inspectRepo(c *gin.Context) {
	if !requireProbeRole(c) || !checkProbeRate(c) {
		return
	}
	var req inspectRepoReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请提供仓库地址"})
		return
	}
	repoURL := strings.TrimSpace(req.RepoURL)
	if repoURL == "" || len(repoURL) > 512 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仓库地址为空或过长"})
		return
	}
	if h.repoInspector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "仓库探测能力未启用"})
		return
	}
	pctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	meta, err := h.repoInspector(pctx, repoURL)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": meta})
}
