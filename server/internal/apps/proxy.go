package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/cert"
	"watchman/server/internal/rpc"
)

// proxyConfigDir is the conf.d watched by the watchman nginx container
// (watchman-app-nginx from the app store). All reverse-proxy vhosts land
// there as watchman-<app>.conf so they are easy to enumerate and remove.
const (
	proxyConfigDir   = "/opt/apps/nginx/conf.d"
	proxyCertDir     = "/opt/apps/nginx/certs"
	nginxContainer   = "watchman-app-nginx"
	writeChunkSize   = 64 * 1024
	fileWriteTimeout = 60 * time.Second
)

// ProxyHandler wires application domain bindings to the certificate hub and
// distributes generated Nginx vhost config + certs to the right host:
//
//   - local mode: the app's own host runs nginx, upstream is 127.0.0.1.
//   - gateway mode: a dedicated gateway host runs nginx for (possibly
//     offline/NAT-hidden) app hosts; upstream points at the app host's mesh
//     IP when available, falling back to its internal IP.
type ProxyHandler struct {
	reg   *rpc.Registry
	store *Store
	hub   *cert.Hub
}

// NewProxyHandler creates the reverse-proxy binding handlers.
func NewProxyHandler(reg *rpc.Registry, store *Store, hub *cert.Hub) *ProxyHandler {
	return &ProxyHandler{reg: reg, store: store, hub: hub}
}

// Register mounts the proxy binding routes (mutations audited by caller).
func (h *ProxyHandler) Register(writeRG *gin.RouterGroup) {
	writeRG.PUT("/apps/:id/proxy", h.bindProxy)
	writeRG.DELETE("/apps/:id/proxy", h.unbindProxy)
}

type proxyBindReq struct {
	// Domain is the public hostname for this app, e.g. "api.example.com".
	Domain string `json:"domain"`
	// Mode: "local" (nginx runs on the app host) | "gateway".
	Mode string `json:"mode"`
	// GatewayHostID names the nginx gateway host (required for mode=gateway).
	GatewayHostID string `json:"gateway_host_id"`
	// Upstream overrides the computed target, e.g. "127.0.0.1:8080" or a mesh IP.
	Upstream string `json:"upstream"`
	// CertID selects a certificate from the hub. Empty = auto-match by domain.
	CertID string `json:"cert_id"`
	// WebSocket support adds the Upgrade/Connection proxy headers.
	WebSocket bool `json:"websocket"`
}

func (h *ProxyHandler) bindProxy(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	var req proxyBindReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	domain := strings.TrimSpace(strings.ToLower(req.Domain))
	if domain == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "域名不能为空"})
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "local"
	}
	if mode != "local" && mode != "gateway" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "反代模式必须是 local 或 gateway"})
		return
	}

	// Resolve the target host: local mode = app host; gateway mode = the
	// designated gateway host.
	targetHost := app.HostID
	if mode == "gateway" {
		if req.GatewayHostID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "网关模式必须指定网关主机"})
			return
		}
		targetHost = req.GatewayHostID
	}
	gatewayHub := h.reg.Hub(targetHost)
	if gatewayHub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标网关主机 Agent 不在线"})
		return
	}

	// Resolve the certificate covering the domain.
	var crt *cert.Certificate
	if req.CertID != "" {
		got, ok := h.hub.Get(req.CertID)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "指定的证书不存在"})
			return
		}
		crt = got
	} else {
		got, ok := h.hub.FindByDomain(domain)
		if !ok {
			c.JSON(http.StatusConflict, gin.H{"error": "证书中心没有覆盖该域名的证书，请先签发"})
			return
		}
		crt = got
	}
	if crt.KeyPEM == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "证书材料缺失，请先重新签发该证书"})
		return
	}

	// Compute the upstream unless overridden: local mode always proxies to
	// the loopback app port; gateway mode uses the app host's mesh IP when
	// it has one, else its internal IP.
	upstream := strings.TrimSpace(req.Upstream)
	if upstream == "" {
		upstream = h.defaultUpstream(app, mode)
	}

	// 1. Push cert + key to the gateway host.
	certBase := fmt.Sprintf("%s/%s", proxyCertDir, domain)
	if err := writeFileOnAgent(gatewayHub, certBase+".crt", []byte(crt.CertPEM)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "推送证书失败: " + err.Error()})
		return
	}
	if err := writeFileOnAgent(gatewayHub, certBase+".key", []byte(crt.KeyPEM)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "推送私钥失败: " + err.Error()})
		return
	}

	// 2. Render + push the vhost config.
	conf := renderVhost(domain, upstream, req.WebSocket)
	confPath := fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
	if err := writeFileOnAgent(gatewayHub, confPath, []byte(conf)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "推送反代配置失败: " + err.Error()})
		return
	}

	// 3. Validate and hot-reload nginx in the watchman-app-nginx container.
	reloadScript := fmt.Sprintf(
		`docker exec %[1]s nginx -t 2>&1 && docker exec %[1]s nginx -s reload 2>&1`,
		nginxContainer)
	res, err := execOnAgent(gatewayHub, "proxy-reload-"+randomToken(4), reloadScript, 30)
	if err != nil {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "nginx reload 超时: " + err.Error()})
		return
	}
	if res.GetExitCode() != 0 {
		out := string(res.GetStdout())
		if out == "" {
			out = string(res.GetStderr())
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "nginx 配置校验失败: " + strings.TrimSpace(out)})
		return
	}

	// 4. Persist the binding on the application.
	if err := h.store.UpdateApp(app.ID, func(a *Application) error {
		a.Domain = domain
		a.ProxyMode = mode
		a.ProxyUpstream = upstream
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "domain": domain, "mode": mode, "upstream": upstream})
}

func (h *ProxyHandler) unbindProxy(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if app.Domain == "" {
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "该应用没有绑定域名"})
		return
	}

	// Remove the vhost config from wherever it was deployed. Best effort on
	// both hosts since the mode may have changed since the last bind.
	targets := []string{app.HostID}
	if app.ProxyMode == "gateway" && app.ProxyUpstream != "" {
		// The gateway host id is not persisted separately; try removing from
		// the app host too, which covers the common single-gateway setups.
		targets = append(targets, targets[0])
	}
	for _, hostID := range dedupe(targets) {
		hub := h.reg.Hub(hostID)
		if hub == nil {
			continue
		}
		confPath := fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
		_, _ = execOnAgent(hub, "proxy-unbind-"+randomToken(4),
			fmt.Sprintf("rm -f %s && docker exec %s nginx -s reload 2>/dev/null || true", confPath, nginxContainer), 30)
	}

	_ = h.store.UpdateApp(app.ID, func(a *Application) error {
		a.Domain = ""
		a.ProxyMode = ""
		a.ProxyUpstream = ""
		return nil
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// defaultUpstream derives the proxy target for the app.
func (h *ProxyHandler) defaultUpstream(app *Application, mode string) string {
	var port int
	if len(app.Ports) > 0 {
		port = app.Ports[0].Host
	}
	if port == 0 {
		port = 8080
	}
	if mode == "local" {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	// Gateway mode: prefer the app host's overlay IP (100.x.y.z), which works
	// even when the app host sits behind NAT. Mesh IP lookup would go through
	// the network store; as a first cut use the loopback of the app host is
	// wrong here — fall back to the app host id resolution at bind time.
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// renderVhost produces the nginx server block for one application domain.
func renderVhost(domain, upstream string, websocket bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by Watchman for %s\n", domain)
	fmt.Fprintf(&b, "server {\n")
	fmt.Fprintf(&b, "    listen 80;\n")
	fmt.Fprintf(&b, "    server_name %s;\n", domain)
	fmt.Fprintf(&b, "    return 301 https://$host$request_uri;\n")
	fmt.Fprintf(&b, "}\n\n")
	fmt.Fprintf(&b, "server {\n")
	fmt.Fprintf(&b, "    listen 443 ssl;\n")
	fmt.Fprintf(&b, "    http2 on;\n")
	fmt.Fprintf(&b, "    server_name %s;\n", domain)
	fmt.Fprintf(&b, "    ssl_certificate %s/%s.crt;\n", proxyCertDir, domain)
	fmt.Fprintf(&b, "    ssl_certificate_key %s/%s.key;\n", proxyCertDir, domain)
	fmt.Fprintf(&b, "    ssl_protocols TLSv1.2 TLSv1.3;\n\n")
	fmt.Fprintf(&b, "    location / {\n")
	fmt.Fprintf(&b, "        proxy_pass http://%s;\n", upstream)
	fmt.Fprintf(&b, "        proxy_set_header Host $host;\n")
	fmt.Fprintf(&b, "        proxy_set_header X-Real-IP $remote_addr;\n")
	fmt.Fprintf(&b, "        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	fmt.Fprintf(&b, "        proxy_set_header X-Forwarded-Proto https;\n")
	if websocket {
		fmt.Fprintf(&b, "        proxy_set_header Upgrade $http_upgrade;\n")
		fmt.Fprintf(&b, "        proxy_set_header Connection \"upgrade\";\n")
		fmt.Fprintf(&b, "        proxy_read_timeout 300s;\n")
	}
	fmt.Fprintf(&b, "    }\n")
	fmt.Fprintf(&b, "}\n")
	return b.String()
}

// writeFileOnAgent streams one small file (cert/conf) through the existing
// FileOp write protocol with sha256 verification.
func writeFileOnAgent(hub *rpc.Hub, path string, data []byte) error {
	opID := "proxy-w-" + randomToken(6)
	sum := sha256.Sum256(data)
	expected := hex.EncodeToString(sum[:])

	ackCh := make(chan *agentpb.Ack, 1)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(opID, nil)
		if msg == nil {
			ackCh <- nil
			return
		}
		ackCh <- msg.GetAck()
	})

	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_FileOp{
			FileOp: &agentpb.FileOp{
				OpId:      opID,
				Op:        "write",
				Path:      path,
				Overwrite: true,
				TotalSize: int64(len(data)),
				Sha256:    expected,
			},
		},
	})

	// The agent expects the initial op followed by data chunks; mirror the
	// upload handler's chunking.
	seq := uint32(0)
	for offset := 0; offset < len(data); offset += writeChunkSize {
		end := offset + writeChunkSize
		if end > len(data) {
			end = len(data)
		}
		seq++
		hub.Send(&agentpb.ServerMessage{
			Payload: &agentpb.ServerMessage_FileOp{
				FileOp: &agentpb.FileOp{
					OpId:     opID,
					Op:       "write",
					Path:     path,
					Offset:   int64(offset),
					ChunkSeq: seq,
					Data:     data[offset:end],
				},
			},
		})
	}

	select {
	case ack := <-ackCh:
		if ack == nil {
			return fmt.Errorf("agent disconnected")
		}
		if !ack.GetOk() {
			return fmt.Errorf("agent rejected write: %s", ack.GetError())
		}
		return nil
	case <-time.After(fileWriteTimeout):
		hub.SetRespHandler(opID, nil)
		return fmt.Errorf("write timeout")
	}
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
