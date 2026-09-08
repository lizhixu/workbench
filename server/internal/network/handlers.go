package network

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/audit"
	"watchman/server/internal/rpc"
)

// Handlers implements REST endpoints for overlay networking (Tailscale/Headscale).
type Handlers struct {
	store *Store
	reg   *rpc.Registry
	audit *audit.Store
}

// NewHandlers creates network Handlers.
func NewHandlers(store *Store, reg *rpc.Registry, auditStore *audit.Store) *Handlers {
	return &Handlers{
		store: store,
		reg:   reg,
		audit: auditStore,
	}
}

// Register mounts routes under /network.
func (h *Handlers) Register(rg, writeRG *gin.RouterGroup) {
	rg.GET("/network/config", h.getConfig)
	rg.GET("/network/nodes", h.listNodes)
	rg.GET("/network/nodes/:id", h.getNode)

	writeRG.PUT("/network/config", h.updateConfig)
	writeRG.POST("/network/nodes/:id/check", h.checkNode)
	writeRG.POST("/network/nodes/:id/install", h.installNode)
	writeRG.POST("/network/nodes/:id/join", h.joinNode)
	writeRG.POST("/network/nodes/:id/leave", h.leaveNode)
	writeRG.POST("/network/nodes/:id/ping", h.pingNode)
}

func (h *Handlers) getConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": h.store.GetConfig()})
}

func (h *Handlers) updateConfig(c *gin.Context) {
	var in NetworkConfig
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.ControlPlane == "" {
		in.ControlPlane = "headscale"
	}
	if in.ControlPlane == "headscale" && in.ServerURL != "" {
		in.ServerURL = strings.TrimRight(in.ServerURL, "/")
	}

	if err := h.store.UpdateConfig(in); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "update", "network_config", "global", fmt.Sprintf("更新组网控制面配置: %s", in.ControlPlane))
	c.JSON(http.StatusOK, gin.H{"data": h.store.GetConfig()})
}

// listNodes returns combined list of registered hosts with overlay network status.
func (h *Handlers) listNodes(c *gin.Context) {
	agents := h.reg.ListAgents()
	cachedNodes := h.store.GetAllNodes()

	type NodeView struct {
		HostID       string    `json:"host_id"`
		Hostname     string    `json:"hostname"`
		OS           string    `json:"os"`
		Arch         string    `json:"arch"`
		InternalIP   string    `json:"internal_ip"`
		PublicIP     string    `json:"public_ip"`
		AgentOnline  bool      `json:"agent_online"`
		Installed    bool      `json:"installed"`
		Online       bool      `json:"online"`
		IP           string    `json:"ip"`
		IPv6         string    `json:"ipv6"`
		NodeName     string    `json:"node_name"`
		Version      string    `json:"version"`
		Direct       bool      `json:"direct"`
		DERP         string    `json:"derp"`
		LatencyMS    float64   `json:"latency_ms"`
		Subnets      []string  `json:"subnets"`
		LastChecked  time.Time `json:"last_checked"`
		ErrorMessage string    `json:"error_message,omitempty"`
	}

	result := make([]NodeView, 0, len(agents))
	for _, a := range agents {
		st := cachedNodes[a.ID]
		nv := NodeView{
			HostID:       a.ID,
			Hostname:     a.Hostname,
			OS:           a.OS,
			Arch:         a.Arch,
			InternalIP:   a.InternalIP,
			PublicIP:     a.PublicIP,
			AgentOnline:  a.Status == "online",
			Installed:    st.Installed,
			Online:       st.Online,
			IP:           st.IP,
			IPv6:         st.IPv6,
			NodeName:     st.NodeName,
			Version:      st.Version,
			Direct:       st.Direct,
			DERP:         st.DERP,
			LatencyMS:    st.LatencyMS,
			Subnets:      st.Subnets,
			LastChecked:  st.LastChecked,
			ErrorMessage: st.ErrorMessage,
		}
		result = append(result, nv)
	}

	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (h *Handlers) getNode(c *gin.Context) {
	hostID := c.Param("id")
	st, ok := h.store.GetNodeStatus(hostID)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"data": NodeStatus{HostID: hostID}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": st})
}

// checkNode runs `tailscale status --json` or `tailscale status` on the target agent.
func (h *Handlers) checkNode(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "主机离线，无法探测组网状态"})
		return
	}

	// 1. Check version & installation
	resVer, err := h.runExec(hub, "tailscale version", 15)
	if err != nil || resVer.ExitCode != 0 {
		st := NodeStatus{
			HostID:       hostID,
			Installed:    false,
			Online:       false,
			ErrorMessage: "Tailscale 未安装或未加入 PATH",
			LastChecked:  time.Now(),
		}
		_ = h.store.SetNodeStatus(st)
		c.JSON(http.StatusOK, gin.H{"data": st})
		return
	}

	verStr := strings.TrimSpace(string(resVer.Stdout))
	verLines := strings.Split(verStr, "\n")
	cleanVer := ""
	if len(verLines) > 0 {
		cleanVer = strings.TrimSpace(verLines[0])
	}

	// 2. Check tailscale status --json
	resStatus, err := h.runExec(hub, "tailscale status --json", 20)
	if err != nil || resStatus.ExitCode != 0 {
		// Possibly daemon not running
		errMsg := string(resStatus.Stderr)
		if errMsg == "" {
			errMsg = string(resStatus.Stdout)
		}
		st := NodeStatus{
			HostID:       hostID,
			Installed:    true,
			Online:       false,
			Version:      cleanVer,
			ErrorMessage: strings.TrimSpace(errMsg),
			LastChecked:  time.Now(),
		}
		_ = h.store.SetNodeStatus(st)
		c.JSON(http.StatusOK, gin.H{"data": st})
		return
	}

	// Parse Tailscale status JSON
	var tsJSON struct {
		BackendState string `json:"BackendState"` // "Running", "NeedsLogin", "Stopped"
		Self         struct {
			ID           string   `json:"ID"`
			HostName     string   `json:"HostName"`
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
			CurAddr      string   `json:"CurAddr"`
			Relay        string   `json:"Relay"`
		} `json:"Self"`
	}

	if err := json.Unmarshal(resStatus.Stdout, &tsJSON); err != nil {
		// Fallback to text status
		st := NodeStatus{
			HostID:       hostID,
			Installed:    true,
			Online:       false,
			Version:      cleanVer,
			ErrorMessage: "解析 Tailscale 状态失败: " + err.Error(),
			LastChecked:  time.Now(),
		}
		_ = h.store.SetNodeStatus(st)
		c.JSON(http.StatusOK, gin.H{"data": st})
		return
	}

	st := NodeStatus{
		HostID:      hostID,
		Installed:   true,
		Version:     cleanVer,
		NodeName:    tsJSON.Self.HostName,
		Online:      tsJSON.BackendState == "Running",
		DERP:        tsJSON.Self.Relay,
		LastChecked: time.Now(),
	}
	if tsJSON.Self.DNSName != "" {
		st.NodeName = strings.TrimRight(tsJSON.Self.DNSName, ".")
	}
	for _, ip := range tsJSON.Self.TailscaleIPs {
		if strings.Contains(ip, ".") && st.IP == "" {
			st.IP = ip
		} else if strings.Contains(ip, ":") && st.IPv6 == "" {
			st.IPv6 = ip
		}
	}

	_ = h.store.SetNodeStatus(st)
	c.JSON(http.StatusOK, gin.H{"data": st})
}

// installNode executes automated Tailscale installation script on the host.
func (h *Handlers) installNode(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "主机离线，无法安装 Tailscale"})
		return
	}

	agent := h.reg.GetAgent(hostID)
	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到主机信息"})
		return
	}

	var cmd string
	var shell string
	if strings.ToLower(agent.OS) == "windows" {
		shell = "powershell"
		cmd = `$msi = "$env:TEMP\tailscale-setup.msi"; [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; (New-Object System.Net.WebClient).DownloadFile('https://pkgs.tailscale.com/stable/tailscale-setup-latest.msi', $msi); Start-Process msiexec.exe -Wait -ArgumentList "/i ` + "`\"$msi`\"" + ` /qn /norestart"; Start-Sleep -Seconds 3; Start-Service Tailscale`
	} else if strings.Contains(strings.ToLower(agent.Distro), "alpine") {
		shell = "sh"
		cmd = `rm -rf /tmp/tailscale* /tmp/lighter-installer*; apk update && apk add tailscale && rc-update add tailscale default 2>/dev/null; rc-service tailscale restart`
	} else {
		shell = "bash"
		cmd = `curl -fsSL https://tailscale.com/install.sh | sh && systemctl enable --now tailscaled`
	}

	h.recordAudit(c, "install", "network_node", hostID, "一键安装 Tailscale 客户端")

	res, err := h.runExecWithTimeout(hub, cmd, shell, 300)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "执行安装超时或出错: " + err.Error()})
		return
	}

	if res.ExitCode != 0 {
		out := string(res.Stderr)
		if out == "" {
			out = string(res.Stdout)
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":       "安装脚本执行失败 (exit code: " + strconv.Itoa(int(res.ExitCode)) + ")",
			"detail":      strings.TrimSpace(out),
			"duration_ms": res.DurationMs,
		})
		return
	}

	// Recheck status
	time.Sleep(2 * time.Second)
	st := NodeStatus{
		HostID:      hostID,
		Installed:   true,
		Online:      false,
		LastChecked: time.Now(),
	}
	_ = h.store.SetNodeStatus(st)

	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"message":     "Tailscale 安装成功并已启动守护进程",
		"duration_ms": res.DurationMs,
		"output":      string(res.Stdout),
	})
}

type JoinRequest struct {
	AuthKey           string `json:"auth_key"`
	ServerURL         string `json:"server_url"`
	AcceptRoutes      *bool  `json:"accept_routes"`
	AdvertiseRoutes   string `json:"advertise_routes"`   // e.g. "192.168.1.0/24"
	AdvertiseExitNode bool   `json:"advertise_exit_node"` // e.g. "--advertise-exit-node"
	Hostname          string `json:"hostname"`
	Reset             bool   `json:"reset"`
}

// joinNode executes `tailscale up` with params to enroll node into the mesh.
func (h *Handlers) joinNode(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "主机离线，无法加入组网"})
		return
	}

	agent := h.reg.GetAgent(hostID)
	if agent == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "未找到主机信息"})
		return
	}

	var req JoinRequest
	_ = c.ShouldBindJSON(&req)

	globalCfg := h.store.GetConfig()

	// Pick parameters (request override or global default)
	authKey := req.AuthKey
	if authKey == "" {
		authKey = globalCfg.AuthKey
	}
	if authKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未提供 Auth Key，请在全局组网设置中预设或在此处填入"})
		return
	}

	serverURL := req.ServerURL
	if serverURL == "" && globalCfg.ControlPlane == "headscale" {
		serverURL = globalCfg.ServerURL
	}

	acceptRoutes := true
	if req.AcceptRoutes != nil {
		acceptRoutes = *req.AcceptRoutes
	} else {
		acceptRoutes = globalCfg.AcceptRoutes
	}

	var args []string
	args = append(args, "tailscale", "up", fmt.Sprintf("--authkey=%s", authKey))

	if serverURL != "" {
		args = append(args, fmt.Sprintf("--login-server=%s", serverURL))
	}
	if acceptRoutes {
		args = append(args, "--accept-routes=true")
	}
	if req.AdvertiseRoutes != "" {
		args = append(args, fmt.Sprintf("--advertise-routes=%s", req.AdvertiseRoutes))
	}
	if req.AdvertiseExitNode {
		args = append(args, "--advertise-exit-node")
	}
	if req.Hostname != "" {
		args = append(args, fmt.Sprintf("--hostname=%s", req.Hostname))
	}
	args = append(args, "--reset")

	cmd := strings.Join(args, " ")

	h.recordAudit(c, "join", "network_node", hostID, "执行 tailscale up 加入异地组网")

	agentShell := "bash"
	if strings.ToLower(agent.OS) == "windows" {
		agentShell = "powershell"
	} else if strings.Contains(strings.ToLower(agent.Distro), "alpine") {
		agentShell = "sh"
	}

	res, err := h.runExecWithTimeout(hub, cmd, agentShell, 90)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":  "加入组网失败: 执行命令超时 (90s)",
			"detail": "超时通常由于目标客户端版本过旧被 Headscale 拒绝握手、网络不通或未启动 tailscaled",
		})
		return
	}

	if res.ExitCode != 0 {
		out := string(res.Stderr)
		if out == "" {
			out = string(res.Stdout)
		}
		detailMsg := strings.TrimSpace(out)
		if res.Error != "" {
			detailMsg = res.Error + ": " + detailMsg
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "加入组网失败 (exit code: " + strconv.Itoa(int(res.ExitCode)) + ")",
			"detail": detailMsg,
		})
		return
	}

	// Give it a brief moment to assign IP, then read IP
	time.Sleep(2 * time.Second)
	resIP, _ := h.runExec(hub, "tailscale ip -4", 10)
	ipStr := ""
	if resIP != nil && resIP.ExitCode == 0 {
		ipStr = strings.TrimSpace(string(resIP.Stdout))
	}

	st, _ := h.store.GetNodeStatus(hostID)
	st.HostID = hostID
	st.Installed = true
	st.Online = true
	if ipStr != "" {
		st.IP = ipStr
	}
	st.LastChecked = time.Now()
	_ = h.store.SetNodeStatus(st)

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "成功加入异地虚拟网络！",
		"ip":      ipStr,
		"data":    st,
	})
}

// leaveNode executes `tailscale down` or `tailscale logout`.
func (h *Handlers) leaveNode(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "主机离线"})
		return
	}

	action := c.DefaultQuery("action", "down") // "down" or "logout"
	var cmd string
	if action == "logout" {
		cmd = "tailscale logout"
	} else {
		cmd = "tailscale down"
	}

	h.recordAudit(c, "leave", "network_node", hostID, fmt.Sprintf("执行 %s 退出/断开异地组网", cmd))

	res, err := h.runExec(hub, cmd, 30)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	st, _ := h.store.GetNodeStatus(hostID)
	st.Online = false
	if action == "logout" {
		st.IP = ""
		st.IPv6 = ""
		st.NodeName = ""
	}
	st.LastChecked = time.Now()
	_ = h.store.SetNodeStatus(st)

	c.JSON(http.StatusOK, gin.H{
		"ok":      res.ExitCode == 0,
		"message": fmt.Sprintf("已成功执行 %s", cmd),
		"detail":  strings.TrimSpace(string(res.Stdout)),
		"data":    st,
	})
}

type PingRequest struct {
	Target string `json:"target"` // 100.x.y.z IP or FQDN node name
	Count  int    `json:"count"`  // default 3
}

// pingNode executes `tailscale ping` from this host to another node in the mesh.
func (h *Handlers) pingNode(c *gin.Context) {
	hostID := c.Param("id")
	hub := h.reg.Hub(hostID)
	if hub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "发起端主机离线"})
		return
	}

	var req PingRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "必须指定目标虚拟 IP 或节点名"})
		return
	}
	if req.Count <= 0 || req.Count > 10 {
		req.Count = 3
	}

	cmd := fmt.Sprintf("tailscale ping -c %d %s", req.Count, req.Target)

	res, err := h.runExecWithTimeout(hub, cmd, "bash", 30)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Ping 测速超时: " + err.Error()})
		return
	}

	stdout := string(res.Stdout)
	stderr := string(res.Stderr)
	output := strings.TrimSpace(stdout)
	if output == "" {
		output = strings.TrimSpace(stderr)
	}

	// Parse direct or relay info
	// Example: "pong from 100.64.1.2 via DERP(syd) in 120ms" or "pong from 100.64.1.2 via 1.2.3.4:41641 in 15ms"
	direct := strings.Contains(output, "direct") || (!strings.Contains(output, "DERP") && strings.Contains(output, "via"))
	derp := ""
	if strings.Contains(output, "DERP(") {
		re := regexp.MustCompile(`DERP\(([a-zA-Z0-9]+)\)`)
		if matches := re.FindStringSubmatch(output); len(matches) > 1 {
			derp = "DERP(" + matches[1] + ")"
		}
	} else if direct {
		derp = "Direct (P2P 直连)"
	}

	// Extract latency ms
	var latency float64
	reLat := regexp.MustCompile(`in\s+([0-9\.]+)\s*ms`)
	if m := reLat.FindStringSubmatch(output); len(m) > 1 {
		latency, _ = strconv.ParseFloat(m[1], 64)
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":         res.ExitCode == 0,
		"output":     output,
		"direct":     direct,
		"derp":       derp,
		"latency_ms": latency,
	})
}

func (h *Handlers) runExec(hub *rpc.Hub, cmd string, timeoutSec int) (*agentpb.ExecResult, error) {
	return h.runExecWithTimeout(hub, cmd, "bash", timeoutSec)
}

func (h *Handlers) runExecWithTimeout(hub *rpc.Hub, cmd, shell string, timeoutSec int) (*agentpb.ExecResult, error) {
	execID := randomToken(8)
	resultCh := make(chan *agentpb.ExecResult, 1)

	hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(execID, nil)
		if msg != nil {
			resultCh <- msg.GetExecResult()
		} else {
			resultCh <- nil
		}
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     execID,
				Shell:      shell,
				Command:    cmd,
				IsScript:   false,
				TimeoutSec: int32(timeoutSec),
			},
		},
	})

	select {
	case res := <-resultCh:
		if res == nil {
			return nil, fmt.Errorf("agent closed connection during execution")
		}
		return res, nil
	case <-time.After(time.Duration(timeoutSec+15) * time.Second):
		hub.SetRespHandler(execID, nil)
		return nil, fmt.Errorf("execution timed out on agent")
	}
}

func (h *Handlers) recordAudit(c *gin.Context, action, targetType, targetID, detail string) {
	if h.audit == nil {
		return
	}
	user := ""
	if u, exists := c.Get("username"); exists {
		if s, ok := u.(string); ok {
			user = s
		}
	}
	h.audit.Record(audit.Entry{
		Timestamp:  time.Now(),
		Username:   user,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     detail,
		IP:         c.ClientIP(),
		UserAgent:  c.Request.UserAgent(),
		RiskLevel:  audit.RiskMedium,
		Result:     audit.ResultSuccess,
	})
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
