package apps

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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
