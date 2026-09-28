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

// networkConfigView is the API-facing shape of NetworkConfig.
//
// The auth key itself is NEVER sent to clients: this endpoint is reachable by
// any authenticated user (including read-only viewers), and the key is an
// enrollment credential — whoever holds it can join their own device to the
// tailnet and reach every mesh node. Callers only learn whether a key is
// configured.
type networkConfigView struct {
	ControlPlane      string    `json:"control_plane"`
	ServerURL         string    `json:"server_url"`
	AuthKey           string    `json:"auth_key"` // always "" — never exposed
	AuthKeySet        bool      `json:"auth_key_set"`
	AcceptRoutes      bool      `json:"accept_routes"`
	AdvertiseExitNode bool      `json:"advertise_exit_node"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func toConfigView(cfg NetworkConfig) networkConfigView {
	return networkConfigView{
		ControlPlane:      cfg.ControlPlane,
		ServerURL:         cfg.ServerURL,
		AuthKey:           "",
		AuthKeySet:        cfg.AuthKey != "",
		AcceptRoutes:      cfg.AcceptRoutes,
		AdvertiseExitNode: cfg.AdvertiseExitNode,
		UpdatedAt:         cfg.UpdatedAt,
	}
}

func (h *Handlers) getConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": toConfigView(h.store.GetConfig())})
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
	// An empty auth key means "keep the stored one": GET never reveals the key,
	// so the frontend resubmits "" when the operator didn't type a new one.
	// (There is deliberately no way to clear the key through this endpoint —
	// rotate it by writing a new value.)
	if in.AuthKey == "" {
		in.AuthKey = h.store.GetConfig().AuthKey
	}

	if err := h.store.UpdateConfig(in); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.recordAudit(c, "update", "network_config", "global", fmt.Sprintf("更新组网控制面配置: %s", in.ControlPlane))
	c.JSON(http.StatusOK, gin.H{"data": toConfigView(h.store.GetConfig())})
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
	resVer, err := h.runExec(hub, hostID, "tailscale version", 15)
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
	resStatus, err := h.runExec(hub, hostID, "tailscale status --json", 20)
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

	shell := agentShellFor(agent)
	var cmd string
	switch shell {
	case "powershell":
		cmd = `$msi = "$env:TEMP\tailscale-setup.msi"; [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; (New-Object System.Net.WebClient).DownloadFile('https://pkgs.tailscale.com/stable/tailscale-setup-latest.msi', $msi); Start-Process msiexec.exe -Wait -ArgumentList "/i ` + "`\"$msi`\"" + ` /qn /norestart"; Start-Sleep -Seconds 3; Start-Service Tailscale`
	case "sh":
		cmd = `rm -rf /tmp/tailscale* /tmp/lighter-installer*; apk update && apk add tailscale && rc-update add tailscale default 2>/dev/null; rc-service tailscale restart`
	default:
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
	AdvertiseRoutes   string `json:"advertise_routes"`    // e.g. "192.168.1.0/24"
	AdvertiseExitNode bool   `json:"advertise_exit_node"` // e.g. "--advertise-exit-node"
	Hostname          string `json:"hostname"`
	Reset             bool   `json:"reset"`
}

// buildJoinArgs assembles the `tailscale up` argument list.
//
// Every user- or config-supplied value is quoted for the target shell: the
// final command is a single string executed by the agent's shell, so an
// unquoted value such as --hostname='x; rm -rf / #' would run arbitrary
// commands on the remote host (the audit record only says "join network",
// hiding the real command).
func buildJoinArgs(shell, authKey, serverURL string, acceptRoutes bool, advertiseRoutes string, advertiseExitNode bool, hostname string, reset bool) []string {
	args := []string{"tailscale", "up", "--authkey=" + shellQuoteArg(shell, authKey)}

	if serverURL != "" {
		args = append(args, "--login-server="+shellQuoteArg(shell, serverURL))
	}
	if acceptRoutes {
		args = append(args, "--accept-routes=true")
	}
	// Always disable MagicDNS override on managed servers to prevent host DNS hijacking
	// and avoid deadlock loops with Docker containers relying on public DNS.
	args = append(args, "--accept-dns=false")

	if advertiseRoutes != "" {
		args = append(args, "--advertise-routes="+shellQuoteArg(shell, advertiseRoutes))
	}
	if advertiseExitNode {
		args = append(args, "--advertise-exit-node")
	}
	if hostname != "" {
		args = append(args, "--hostname="+shellQuoteArg(shell, hostname))
	}
	// `--reset` discards the local node state and re-enrolls the node with a NEW
	// node key, so the control plane treats it as a brand new node and hands out
	// a different 100.x.y.z address. That is why re-joining after a failure used
	// to silently change the mesh IP. Only reset when explicitly requested
	// (e.g. the node key is genuinely corrupted or the host was re-imaged).
	if reset {
		args = append(args, "--reset")
	}
	return args
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

	agentShell := agentShellFor(agent)
	args := buildJoinArgs(agentShell, authKey, serverURL, acceptRoutes, req.AdvertiseRoutes, req.AdvertiseExitNode, req.Hostname, req.Reset)
	cmd := strings.Join(args, " ")

	// Remember the previous address so we can report whether it changed.
	prevStatus, _ := h.store.GetNodeStatus(hostID)
	prevIP := prevStatus.IP

	h.recordAudit(c, "join", "network_node", hostID, "执行 tailscale up 加入异地组网")

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

	// Give it a brief moment to assign IP, then read it back.
	time.Sleep(2 * time.Second)
	ipStr, ip6Str := h.querySelfIPs(hub, agentShell, hostID)

	st := prevStatus
	st.HostID = hostID
	st.Installed = true
	st.Online = true
	if ipStr != "" {
		st.IP = ipStr
	}
	if ip6Str != "" {
		st.IPv6 = ip6Str
	}
	st.LastChecked = time.Now()
	_ = h.store.SetNodeStatus(st)

	ipChanged := prevIP != "" && ipStr != "" && prevIP != ipStr

	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"message":     "成功加入异地虚拟网络！",
		"ip":          ipStr,
		"previous_ip": prevIP,
		"ip_changed":  ipChanged,
		"data":        st,
	})
}

// querySelfIPs reads the node's own Tailscale addresses.
//
// `tailscale status --json` is preferred because it returns IPv4 and IPv6 in one
// round-trip and also works when `tailscale ip` is missing from older builds.
func (h *Handlers) querySelfIPs(hub *rpc.Hub, shell, hostID string) (string, string) {
	if res, err := h.runExecWithTimeout(hub, "tailscale status --json", shell, 20); err == nil && res != nil && res.ExitCode == 0 {
		var st struct {
			Self struct {
				TailscaleIPs []string `json:"TailscaleIPs"`
			} `json:"Self"`
		}
		if err := json.Unmarshal(res.Stdout, &st); err == nil {
			var v4, v6 string
			for _, ip := range st.Self.TailscaleIPs {
				ip = strings.TrimSpace(ip)
				if strings.Contains(ip, ":") {
					if v6 == "" {
						v6 = ip
					}
				} else if v4 == "" {
					v4 = ip
				}
			}
			if v4 != "" || v6 != "" {
				return v4, v6
			}
		}
	}
	if res, err := h.runExecWithTimeout(hub, "tailscale ip -4", shell, 10); err == nil && res != nil && res.ExitCode == 0 {
		return strings.TrimSpace(string(res.Stdout)), ""
	}
	return "", ""
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

	res, err := h.runExec(hub, hostID, cmd, 30)
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

	shell := agentShellFor(h.reg.GetAgent(hostID))
	target := shellQuoteArg(shell, req.Target)
	// `--until-direct=false`: since v1.24 `tailscale ping` defaults to
	// until-direct=true, i.e. it keeps probing until a direct path is
	// established. The first probes almost always traverse DERP, so the command
	// hangs well past its budget and returns nothing useful.
	// `--timeout`: bound each probe so a black-holed target cannot stall the call.
	cmd := fmt.Sprintf("tailscale ping --until-direct=false -c %d --timeout=5s %s", req.Count, target)

	res, err := h.runExecWithTimeout(hub, cmd, shell, 40)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Ping 测速失败: " + err.Error(),
			"hint":  "Agent 未在超时时间内返回结果，通常是 tailscaled 未运行或主机负载过高",
		})
		return
	}

	output := strings.TrimSpace(string(res.Stdout))
	stderr := strings.TrimSpace(string(res.Stderr))
	if output == "" {
		output = stderr
	}

	// Older Tailscale builds (<1.24) do not know --until-direct/--timeout.
	// Retry with the minimal flag set instead of failing outright.
	if res.ExitCode != 0 && looksLikeUnknownFlag(output) {
		legacy := fmt.Sprintf("tailscale ping -c %d %s", req.Count, target)
		if res2, err2 := h.runExecWithTimeout(hub, legacy, shell, 40); err2 == nil && res2 != nil {
			res = res2
			output = strings.TrimSpace(string(res2.Stdout))
			if output == "" {
				output = strings.TrimSpace(string(res2.Stderr))
			}
		}
	}

	direct, derp, latency := parsePingOutput(output)

	resp := pingResult{
		OK:        res.ExitCode == 0 && strings.Contains(output, "pong from"),
		Output:    output,
		Direct:    direct,
		DERP:      derp,
		LatencyMS: latency,
	}
	if res.Error != "" {
		resp.Error = res.Error
	}
	if !resp.OK {
		resp.Hint = pingFailureHint(output, res.Error)
	} else if !direct {
		resp.Hint = "当前经 DERP 中继转发，尚未建立 P2P 直连（检查 UDP 41641 是否被防火墙/NAT 拦截）"
	}
	c.JSON(http.StatusOK, resp)
}

// pingResult is the payload returned by the connectivity probe.
type pingResult struct {
	OK        bool    `json:"ok"`
	Output    string  `json:"output"`
	Direct    bool    `json:"direct"`
	DERP      string  `json:"derp"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
	Hint      string  `json:"hint,omitempty"`
}

var (
	rePingPong   = regexp.MustCompile(`pong\s+from\s+.*`)
	rePingDERP   = regexp.MustCompile(`DERP\(([^)]+)\)`)
	rePingLatMS  = regexp.MustCompile(`in\s+([0-9]+(?:\.[0-9]+)?)\s*ms`)
	rePingLatSec = regexp.MustCompile(`in\s+([0-9]+(?:\.[0-9]+)?)\s*s\b`)
)

// looksLikeUnknownFlag reports whether a CLI failure was caused by an
// unsupported flag, so we can retry with a compatible command.
func looksLikeUnknownFlag(output string) bool {
	low := strings.ToLower(output)
	return strings.Contains(low, "unknown flag") ||
		strings.Contains(low, "flag provided but not defined") ||
		strings.Contains(low, "unknown long flag") ||
		strings.Contains(low, "unknown shorthand flag")
}

// parsePingOutput extracts link type and latency from `tailscale ping` output.
//
// Real-world shapes it must handle:
//
//	pong from 100.64.1.2 via DERP(syd) in 120ms                 (legacy)
//	pong from 100.64.1.2 via 1.2.3.4:41641 in 15ms              (direct)
//	pong from node-a (100.64.1.2) via DERP(nyc) in 30ms         (modern)
//	pong from node-a (100.64.1.2) via direct in 12ms
func parsePingOutput(output string) (direct bool, derp string, latency float64) {
	line := ""
	for _, l := range strings.Split(output, "\n") {
		if rePingPong.MatchString(l) {
			line = strings.TrimSpace(l) // keep the LAST successful probe
		}
	}
	if line == "" {
		return false, "", 0
	}

	if m := rePingDERP.FindStringSubmatch(line); len(m) > 1 {
		derp = "DERP(" + m[1] + ") 中继"
		direct = false
	} else if strings.Contains(line, "via") {
		direct = true
		derp = "Direct (P2P 直连)"
	}

	if m := rePingLatMS.FindStringSubmatch(line); len(m) > 1 {
		latency, _ = strconv.ParseFloat(m[1], 64)
	} else if m := rePingLatSec.FindStringSubmatch(line); len(m) > 1 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			latency = v * 1000
		}
	}
	return direct, derp, latency
}

// pingFailureHint turns an empty/cryptic failure into an actionable message.
func pingFailureHint(output, execErr string) string {
	low := strings.ToLower(output + " " + execErr)
	switch {
	case strings.Contains(low, "timeout") || strings.Contains(low, "timed out"):
		return "命令超时：tailscaled 可能未运行，或目标节点长时间无响应"
	case strings.Contains(low, "no reply"), strings.Contains(low, "no response"):
		return "目标无响应：对方可能离线、ACL 未放行，或 UDP 被防火墙拦截"
	case strings.Contains(low, "unknown node"), strings.Contains(low, "not found"), strings.Contains(low, "no such host"):
		return "目标 IP/节点名不在当前 Tailnet 内，请确认地址是否正确且对方已加入组网"
	case strings.Contains(low, "not logged in"), strings.Contains(low, "needslogin"):
		return "本机 Tailscale 尚未登录，请先执行「加入组网」"
	case strings.Contains(low, "not found"), strings.Contains(low, "command not found"):
		return "本机未找到 tailscale 命令，请先安装客户端"
	case strings.TrimSpace(output) == "":
		return "命令无任何输出：请确认本机已安装 tailscale 且已加入组网（可用「探测」按钮复核状态）"
	default:
		return "请展开原始输出查看 tailscale 的报错详情"
	}
}

// shellQuoteArg quotes a value so it survives being embedded in a shell
// command string executed by the agent. Quoting follows the target shell:
// PowerShell single-quoted strings escape an embedded quote by doubling it
// (”), POSIX shells use '\”. Values without special characters are returned
// unchanged.
func shellQuoteArg(shell, s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"$&|;<>()\\`*?[]#!{}~") {
		return s
	}
	if shell == "powershell" {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// agentShellFor returns the shell used to run commands on a given agent.
//
// Hardcoding "bash" breaks Windows (no bash) and Alpine/BusyBox hosts (bash not
// installed by default): the command fails instantly with "executable file not
// found", which made status/ping probes look like a network problem.
func agentShellFor(agent *rpc.Agent) string {
	if agent == nil {
		return "bash"
	}
	if strings.Contains(strings.ToLower(agent.OS), "windows") {
		return "powershell"
	}
	distro := strings.ToLower(agent.Distro)
	if strings.Contains(distro, "alpine") || strings.Contains(distro, "busybox") {
		return "sh"
	}
	return "bash"
}

// runExec runs cmd on hostID using a shell that matches the host OS.
func (h *Handlers) runExec(hub *rpc.Hub, hostID, cmd string, timeoutSec int) (*agentpb.ExecResult, error) {
	return h.runExecWithTimeout(hub, cmd, agentShellFor(h.reg.GetAgent(hostID)), timeoutSec)
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
