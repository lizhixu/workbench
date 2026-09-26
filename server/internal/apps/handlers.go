// Package apps provides application templates and Docker installation utilities.
package apps

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Handlers manages legacy App Store read-only routes and host-level Docker utilities.
type Handlers struct {
	reg *rpc.Registry
}

// NewHandlers creates a new apps handler instance.
func NewHandlers(reg *rpc.Registry) *Handlers {
	return &Handlers{reg: reg}
}

// Register mounts read-only routes on rg and mutating routes on writeRG.
func (h *Handlers) Register(rg, writeRG *gin.RouterGroup) {
	rg.GET("/apps/catalog", h.getCatalog)
	writeRG.POST("/hosts/:id/docker/install-script", h.installDockerScript)
}

func (h *Handlers) getCatalog(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": Catalog})
}

// installDockerScript triggers official one-click Docker installation script.
func (h *Handlers) installDockerScript(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	installScript := `#!/bin/sh
if command -v docker >/dev/null 2>&1; then
    echo "Docker is already installed."
    docker --version
    exit 0
fi

echo "Starting one-click Docker installation..."
curl -fsSL https://get.docker.com | sh
systemctl enable --now docker || service docker start || true
echo "Docker installation complete."
docker --version
`

	opID := "dk-inst-" + randomToken(4)
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(opID, nil)
		if msg != nil {
			resultCh <- msg.GetExecResult()
		} else {
			resultCh <- nil
		}
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     opID,
				Command:    installScript,
				IsScript:   true,
				TimeoutSec: 300,
			},
		},
	})

	select {
	case res := <-resultCh:
		if res == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":        res.GetExitCode() == 0,
			"exit_code": res.GetExitCode(),
			"stdout":    string(res.GetStdout()),
			"stderr":    string(res.GetStderr()),
		})
	case <-c.Request.Context().Done():
		hub.SetRespHandler(opID, nil)
	}
}
