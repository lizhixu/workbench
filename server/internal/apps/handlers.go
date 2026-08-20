// Package apps provides application templates (Nginx, Redis, MySQL, etc.) and
// one-click parameterized Docker container installation on managed hosts.
package apps

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// EnvField defines a configurable parameter for an application template.
type EnvField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     string `json:"default"`
	Required    bool   `json:"required"`
	IsSecret    bool   `json:"is_secret"`
}

// AppTemplate defines an application available in the App Store catalog.
type AppTemplate struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Category       string     `json:"category"` // web | database | security | tool
	Icon           string     `json:"icon"`     // icon identifier
	Version        string     `json:"version"`
	Description    string     `json:"description"`
	Image          string     `json:"image"`
	DefaultPort    int        `json:"default_port"`
	ContainerPort  int        `json:"container_port"`
	DefaultVolume  string     `json:"default_volume"`
	EnvFields      []EnvField `json:"env_fields"`
}

// Catalog contains standard templates for one-click installation.
var Catalog = []AppTemplate{
	{
		ID:            "nginx",
		Name:          "Nginx Web Server",
		Category:      "web",
		Icon:          "GlobeOutline",
		Version:       "latest",
		Description:   "高性能 HTTP 与反向代理 Web 服务器，适用于静态网页、反向代理与负载均衡。",
		Image:         "nginx:alpine",
		DefaultPort:   80,
		ContainerPort: 80,
		DefaultVolume: "/opt/apps/nginx/html:/usr/share/nginx/html",
		EnvFields:     []EnvField{},
	},
	{
		ID:            "redis",
		Name:          "Redis In-Memory Database",
		Category:      "database",
		Icon:          "LayersOutline",
		Version:       "7.2-alpine",
		Description:   "超高性能内存键值数据库，支持持久化、缓存加速与消息队列。",
		Image:         "redis:7.2-alpine",
		DefaultPort:   6379,
		ContainerPort: 6379,
		DefaultVolume: "/opt/apps/redis/data:/data",
		EnvFields: []EnvField{
			{
				Key:         "PASSWORD",
				Label:       "访问密码 (可选)",
				Description: "留空则免密连接；填写则配置 requirepass 认证",
				Default:     "",
				IsSecret:    true,
			},
		},
	},
	{
		ID:            "mysql",
		Name:          "MySQL Relational Database",
		Category:      "database",
		Icon:          "ServerOutline",
		Version:       "8.0",
		Description:   "全球主流开源关系型数据库系统，支持高并发事务与企业级持久化存储。",
		Image:         "mysql:8.0",
		DefaultPort:   3306,
		ContainerPort: 3306,
		DefaultVolume: "/opt/apps/mysql/data:/var/lib/mysql",
		EnvFields: []EnvField{
			{
				Key:         "MYSQL_ROOT_PASSWORD",
				Label:       "Root 密码",
				Description: "root 超级管理员登录密码",
				Default:     "root123456",
				Required:    true,
				IsSecret:    true,
			},
			{
				Key:         "MYSQL_DATABASE",
				Label:       "默认数据库名称",
				Description: "容器首次启动时自动创建的数据库名",
				Default:     "app_db",
			},
		},
	},
	{
		ID:            "postgres",
		Name:          "PostgreSQL Database",
		Category:      "database",
		Icon:          "FileTrayStackedOutline",
		Version:       "16-alpine",
		Description:   "强大可靠的开源对象关系型数据库，支持丰富的数据类型与 JSON 扩展。",
		Image:         "postgres:16-alpine",
		DefaultPort:   5432,
		ContainerPort: 5432,
		DefaultVolume: "/opt/apps/postgres/data:/var/lib/postgresql/data",
		EnvFields: []EnvField{
			{
				Key:         "POSTGRES_PASSWORD",
				Label:       "Postgres 密码",
				Description: "默认管理员 postgres 用户的登录密码",
				Default:     "postgres123",
				Required:    true,
				IsSecret:    true,
			},
			{
				Key:         "POSTGRES_DB",
				Label:       "默认数据库名称",
				Description: "容器首次启动时创建的数据库",
				Default:     "watchman_db",
			},
		},
	},
	{
		ID:            "safeline",
		Name:          "雷池 SafeLine WAF 社区版",
		Category:      "security",
		Icon:          "ShieldCheckmarkOutline",
		Version:       "latest",
		Description:   "长亭科技开源 Web 应用防火墙 (WAF)，阻断 SQL 注入、XSS 等攻击。",
		Image:         "chaitin/safeline-tengine:latest",
		DefaultPort:   9443,
		ContainerPort: 9443,
		DefaultVolume: "/opt/apps/safeline/data:/app/data",
		EnvFields:     []EnvField{},
	},
}

// Handlers manages App Store API endpoints.
type Handlers struct {
	reg *rpc.Registry
}

// NewHandlers creates a new apps handler instance.
func NewHandlers(reg *rpc.Registry) *Handlers {
	return &Handlers{reg: reg}
}

// Register registers App Store routes into the gin router.
func (h *Handlers) Register(rg *gin.RouterGroup) {
	rg.GET("/apps/catalog", h.getCatalog)
	rg.GET("/hosts/:id/apps", h.getInstalledApps)
	rg.POST("/hosts/:id/apps/install", h.installApp)
	rg.DELETE("/hosts/:id/apps/:name", h.uninstallApp)
	rg.POST("/hosts/:id/docker/install-script", h.installDockerScript)
}

func (h *Handlers) getCatalog(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": Catalog})
}

// containerInfo is a typed subset of `docker ps --format {{json .}}`.
type containerInfo struct {
	ID     string `json:"ID"`
	Image  string `json:"Image"`
	Name   string `json:"Names"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
}

// parseContainers parses newline-delimited JSON output from docker ps.
func parseContainers(raw []byte) []containerInfo {
	var list []containerInfo
	for _, line := range splitLines(raw) {
		if len(line) == 0 {
			continue
		}
		var c containerInfo
		if err := json.Unmarshal(line, &c); err == nil {
			list = append(list, c)
		}
	}
	return list
}

func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}

type InstallReq struct {
	AppID     string            `json:"app_id"`
	HostPort  int               `json:"host_port"`
	Volume    string            `json:"volume"`
	EnvParams map[string]string `json:"env_params"`
}

func (h *Handlers) installApp(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	var req InstallReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var target *AppTemplate
	for i := range Catalog {
		if Catalog[i].ID == req.AppID {
			target = &Catalog[i]
			break
		}
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "app template not found"})
		return
	}

	port := req.HostPort
	if port <= 0 {
		port = target.DefaultPort
	}
	vol := req.Volume
	if vol == "" {
		vol = target.DefaultVolume
	}

	containerName := fmt.Sprintf("watchman-app-%s", target.ID)

	// Construct docker run command
	args := []string{
		"docker", "run", "-d",
		"--name", containerName,
		"--restart", "unless-stopped",
		"-p", fmt.Sprintf("%d:%d", port, target.ContainerPort),
		"-v", vol,
		"--label", fmt.Sprintf("watchman.app=%s", target.ID),
		"--label", fmt.Sprintf("watchman.name=%s", strings.ReplaceAll(target.Name, " ", "-")),
	}

	if target.ID == "redis" {
		pass := req.EnvParams["PASSWORD"]
		if pass != "" {
			args = append(args, target.Image, "redis-server", "--requirepass", pass)
		} else {
			args = append(args, target.Image)
		}
	} else {
		for k, v := range req.EnvParams {
			if v != "" {
				args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
			}
		}
		args = append(args, target.Image)
	}

	cmdStr := strings.Join(args, " ")

	// Execute on agent via ExecRequest
	opID := "app-inst-" + hostID[:4]
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
				Command:    cmdStr,
				IsScript:   false,
				TimeoutSec: 180,
			},
		},
	})

	select {
	case res := <-resultCh:
		if res == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		if res.GetExitCode() != 0 {
			errOutput := string(res.GetStderr())
			if errOutput == "" {
				errOutput = string(res.GetStdout())
			}
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":     fmt.Sprintf("install failed (code %d): %s", res.GetExitCode(), errOutput),
				"exit_code": res.GetExitCode(),
				"command":   cmdStr,
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"ok":             true,
			"container_name": containerName,
			"output":         string(res.GetStdout()),
		})
	case <-c.Request.Context().Done():
		hub.SetRespHandler(opID, nil)
	}
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

	opID := "dk-inst-" + hostID[:4]
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

// getInstalledApps lists running containers with the watchman.app label.
func (h *Handlers) getInstalledApps(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	opID := randomToken(8)
	resultCh := make(chan *agentpb.DockerEvent, 1)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(opID, nil)
		if msg == nil {
			resultCh <- nil
			return
		}
		resultCh <- msg.GetDocker()
	})

	filters := []string{
		"label=watchman.app",
	}
	argsJSON, _ := json.Marshal(map[string]any{"filters": filters})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_DockerOp{
			DockerOp: &agentpb.DockerOp{
				OpId:     opID,
				Op:       "ps",
				ArgsJson: argsJSON,
			},
		},
	})
	defer hub.SetRespHandler(opID, nil)
	select {
	case ev := <-resultCh:
		if ev == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
			return
		}
		if !ev.GetOk() {
			c.JSON(http.StatusInternalServerError, gin.H{"error": ev.GetError()})
			return
		}

		containers := parseContainers(ev.GetPayloadJson())
		instances := make([]gin.H, 0, len(containers))
		for _, cinfo := range containers {
			instances = append(instances, gin.H{
				"name":    cinfo.Name,
				"app_id":  "", // label value not available in standard ps json
				"image":   cinfo.Image,
				"status":  cinfo.Status,
				"state":   cinfo.Status,
				"ports":   cinfo.Ports,
				"running": strings.Contains(cinfo.Status, "Up"),
			})
		}
		c.JSON(http.StatusOK, gin.H{"data": instances})
	case <-time.After(30 * time.Second):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "docker timeout"})
	}
}

// uninstallApp stops and removes an application container.
func (h *Handlers) uninstallApp(c *gin.Context) {
	hostID := c.Param("id")
	appName := c.Param("name")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent offline"})
		return
	}

	doOp := func(op string) (*agentpb.DockerEvent, error) {
		opID := randomToken(8)
		resultCh := make(chan *agentpb.DockerEvent, 1)
		hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
			hub.SetRespHandler(opID, nil)
			if msg == nil {
				resultCh <- nil
				return
			}
			resultCh <- msg.GetDocker()
		})
		hub.Send(&agentpb.ServerMessage{
			Payload: &agentpb.ServerMessage_DockerOp{
				DockerOp: &agentpb.DockerOp{
					OpId:      opID,
					Op:        op,
					Container: appName,
				},
			},
		})
		defer hub.SetRespHandler(opID, nil)
		select {
		case ev := <-resultCh:
			return ev, nil
		case <-time.After(30 * time.Second):
			return nil, fmt.Errorf("%s timeout", op)
		}
	}

	ev, err := doOp("stop")
	if err != nil {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
		return
	}
	if ev == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
		return
	}

	ev, err = doOp("rm")
	if err != nil {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": err.Error()})
		return
	}
	if ev == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
		return
	}
	if !ev.GetOk() {
		c.JSON(http.StatusInternalServerError, gin.H{"error": ev.GetError()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
