package apps

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/cert"
	"watchman/server/internal/network"
	"watchman/server/internal/rpc"
)

// proxyConfigDir / proxyCertDir are host-side directories bind-mounted into
// the system-managed reverse-proxy container: vhosts land in conf.d as
// watchman-<app>.conf, certs in certs/<domain>.{crt,key}. Both are created by
// ensureProxyNginx — nothing assumes the operator prepared them by hand.
const (
	proxyConfigDir = "/opt/apps/nginx/conf.d"
	proxyCertDir   = "/opt/apps/nginx/certs"
	// nginxContainer names the system-managed reverse proxy — deliberately
	// OUTSIDE the user app namespace (watchman-app-*) so a user-deployed
	// nginx application can never shadow or collide with it, the same
	// reserved-name philosophy as dokploy-traefik. The container is created
	// and kept healthy by ensureProxyNginx (1Panel OpenResty model), never
	// expected to pre-exist.
	nginxContainer   = "watchman-proxy-nginx"
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
	reg          *rpc.Registry
	store        *Store
	hub          *cert.Hub
	networkStore *network.Store
	log          *slog.Logger
	// pushMu serializes cert/conf file pushes + nginx reloads across the
	// bind flow and the renewal re-push. The agent-side write session is
	// tracked per op id, so two concurrent writes to the same path could
	// interleave chunks; every push sequence takes this lock (binds are
	// rare user actions, re-pushes rarer still — contention is negligible).
	pushMu sync.Mutex
}

// NewProxyHandler creates the reverse-proxy binding handlers.
func NewProxyHandler(reg *rpc.Registry, store *Store, hub *cert.Hub) *ProxyHandler {
	return &ProxyHandler{reg: reg, store: store, hub: hub, log: slog.Default()}
}

// SetLogger overrides the structured logger (defaults to slog.Default()).
func (h *ProxyHandler) SetLogger(log *slog.Logger) {
	if log != nil {
		h.log = log
	}
}

// SetNetworkStore injects the overlay network store to resolve Tailscale mesh IPs.
func (h *ProxyHandler) SetNetworkStore(ns *network.Store) {
	h.networkStore = ns
}

// Register mounts the proxy binding routes (mutations audited by caller).
func (h *ProxyHandler) Register(writeRG *gin.RouterGroup) {
	writeRG.PUT("/apps/:id/proxy", h.bindProxy)
	writeRG.DELETE("/apps/:id/proxy", h.unbindProxy)
}

// proxyDomainReq is one domain of a bind request: the hostname plus the
// certificate that serves it. CertID is mandatory — there is no auto-match
// and no auto-issue on the bind path.
type proxyDomainReq struct {
	Domain string `json:"domain"`
	CertID string `json:"cert_id"`
}

type proxyBindReq struct {
	// Domains is the complete binding set (full-replace semantics: what is
	// submitted is what the app serves afterwards).
	Domains []proxyDomainReq `json:"domains"`
	// Legacy single-domain fields for pre multi-domain clients; parsed into a
	// one-element Domains list. CertID is required here too.
	Domain string `json:"domain"`
	CertID string `json:"cert_id"`
	// Mode: "local" (nginx runs on the app host) | "gateway".
	Mode string `json:"mode"`
	// GatewayHostID names the nginx gateway host (required for mode=gateway).
	GatewayHostID string `json:"gateway_host_id"`
	// Upstream overrides the computed target, e.g. "127.0.0.1:8080" or a mesh IP.
	Upstream string `json:"upstream"`
	// WebSocket support adds the Upgrade/Connection proxy headers.
	WebSocket bool `json:"websocket"`
}

// domainCertPair is one resolved (binding, certificate) pair, normalized to
// the lowercase hostname that lands in the vhost.
type domainCertPair struct {
	binding DomainBinding
	crt     *cert.Certificate
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
	// Legacy single-domain requests become a one-element list.
	if len(req.Domains) == 0 && req.Domain != "" {
		req.Domains = []proxyDomainReq{{Domain: req.Domain, CertID: req.CertID}}
	}
	if len(req.Domains) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "至少绑定一个域名"})
		return
	}

	// Normalize + validate every domain up front.
	domains := make([]DomainBinding, 0, len(req.Domains))
	seen := make(map[string]bool, len(req.Domains))
	for i := range req.Domains {
		domain := strings.TrimSpace(strings.ToLower(req.Domains[i].Domain))
		if domain == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "域名不能为空"})
			return
		}
		// The domain is interpolated verbatim into an nginx server block that
		// is written as root on the gateway host. A strict hostname check keeps
		// metacharacters (`;{}#$\n` …) from breaking out of the server_name
		// directive — `nginx -t` only validates syntax, not intent.
		if !validProxyDomain(domain) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("域名 %s 格式非法：仅允许小写字母、数字、连字符与点分隔", domain)})
			return
		}
		if seen[domain] {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("域名 %s 在请求中重复", domain)})
			return
		}
		seen[domain] = true
		certID := strings.TrimSpace(req.Domains[i].CertID)
		if certID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("域名 %s 未选择证书：请先在证书中心签发或选择覆盖该域名的证书，再进行绑定", domain)})
			return
		}
		domains = append(domains, DomainBinding{Domain: domain, CertID: certID})
	}

	// Resolve every binding's certificate up front — explicit CertID only,
	// no auto-match and no auto-issue. Nothing is pushed to any host before
	// the whole request is known good.
	pairs := make([]domainCertPair, 0, len(domains))
	for _, d := range domains {
		if h.hub == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "证书中心未初始化"})
			return
		}
		crt, ok := h.hub.Get(d.CertID)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("域名 %s 指定的证书不存在", d.Domain)})
			return
		}
		if !certCoversDomain(crt, d.Domain) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("所选证书不覆盖域名 %s（证书 SAN：%s），请选择其他证书或先签发覆盖该域名的证书", d.Domain, strings.Join(crt.Domains, ", "))})
			return
		}
		if crt.KeyPEM == "" {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("域名 %s 所选证书材料缺失，请先重新签发该证书", d.Domain)})
			return
		}
		pairs = append(pairs, domainCertPair{binding: d, crt: crt})
	}

	// One domain maps to exactly one app: two vhosts with the same
	// server_name would both pass `nginx -t` and route unpredictably.
	for _, a := range h.store.ListApps() {
		if a.ID == app.ID {
			continue
		}
		for _, b := range a.ProxyDomains {
			for _, d := range domains {
				if b.Domain != "" && strings.EqualFold(b.Domain, d.Domain) {
					c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("域名 %s 已被应用 %q 绑定，请先解绑", d.Domain, a.Name)})
					return
				}
			}
		}
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
		if req.GatewayHostID == app.HostID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "网关主机不能与应用主机相同（相同场景请改用 local 模式）"})
			return
		}
		targetHost = req.GatewayHostID
	}
	gatewayHub := h.reg.Hub(targetHost)
	if gatewayHub == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "目标网关主机 Agent 不在线"})
		return
	}

	// The reverse proxy is a system-managed nginx companion container whose
	// lifecycle scripts (docker run --network host, /opt/apps/nginx bind
	// mounts) assume a Linux host. Reject Windows up front instead of
	// letting it fail deep inside a docker exec with a cryptic message.
	if ag := h.reg.GetAgent(targetHost); ag != nil && strings.EqualFold(ag.OS, "windows") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "反代绑定暂仅支持 Linux 主机（系统托管的反代 Nginx 容器依赖 Linux 宿主）"})
		return
	}

	// Guarantee the proxy container BEFORE pushing anything: a broken host
	// (no docker, 80/443 occupied) must fail fast here instead of after half
	// the cert files landed. Serialized against the push/re-push flow (rebuild
	// never races an in-flight reload), but released before the push loop.
	h.pushMu.Lock()
	ensureErr := h.ensureProxyNginx(gatewayHub)
	h.pushMu.Unlock()
	if ensureErr != nil {
		if errors.Is(ensureErr, errProxyDockerUnavailable) ||
			errors.Is(ensureErr, errProxyPortConflict) ||
			errors.Is(ensureErr, errProxyImagePull) {
			c.JSON(http.StatusConflict, gin.H{"error": ensureErr.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": ensureErr.Error()})
		}
		return
	}

	// Compute the upstream unless overridden: local mode always proxies to
	// the loopback app port; gateway mode prefers the overlay (100.x) path
	// when both hosts are in the mesh, else 直连 the app host directly.
	upstream := strings.TrimSpace(req.Upstream)
	if upstream == "" {
		var err error
		upstream, err = h.defaultUpstream(app, mode, req.GatewayHostID)
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
	}

	// 1. Push every cert + key to the nginx host, 2. push the vhost config,
	// 3. validate + hot-reload nginx. Serialized against the renewal
	// re-push (pushMu) so two write sessions never interleave chunks on the
	// same file path.
	bindings := make([]DomainBinding, 0, len(pairs))
	for _, p := range pairs {
		bindings = append(bindings, p.binding)
	}
	h.pushMu.Lock()
	pushErr := func() error {
		for _, p := range pairs {
			if err := h.pushCertFiles(gatewayHub, p.binding.Domain, p.crt); err != nil {
				return err
			}
		}
		conf := renderVhosts(bindings, upstream, req.WebSocket)
		confPath := fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
		if err := writeFileOnAgent(gatewayHub, confPath, []byte(conf)); err != nil {
			return fmt.Errorf("推送反代配置失败: %w", err)
		}
		return h.reloadNginx(gatewayHub)
	}()
	h.pushMu.Unlock()
	if pushErr != nil {
		if errors.Is(pushErr, errReloadTimeout) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": pushErr.Error()})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": pushErr.Error()})
		}
		return
	}

	// 4. Persist the binding on the application.
	if err := h.store.UpdateApp(app.ID, func(a *Application) error {
		a.ProxyDomains = bindings
		a.ProxyMode = mode
		a.ProxyUpstream = upstream
		if mode == "gateway" {
			a.ProxyGatewayHostID = req.GatewayHostID
		} else {
			a.ProxyGatewayHostID = ""
		}
		return nil
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":       true,
		"domains":  bindings,
		"mode":     mode,
		"upstream": upstream,
	})
}

// certCoversDomain reports whether any SAN of the certificate covers the
// domain (cert.DomainCovers — the same rule FindByDomain applies).
func certCoversDomain(crt *cert.Certificate, domain string) bool {
	for _, san := range crt.Domains {
		if cert.DomainCovers(san, domain) {
			return true
		}
	}
	return false
}

func (h *ProxyHandler) unbindProxy(c *gin.Context) {
	app, ok := h.store.GetApp(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "app not found"})
		return
	}
	if len(app.ProxyDomains) == 0 {
		c.JSON(http.StatusOK, gin.H{"ok": true, "message": "该应用没有绑定域名"})
		return
	}

	// Remove the vhost config plus every domain's cert files from wherever
	// they were deployed (certs would otherwise linger as stale key material).
	targets := []string{app.HostID}
	if app.ProxyMode == "gateway" && app.ProxyGatewayHostID != "" {
		targets = append(targets, app.ProxyGatewayHostID)
	}
	for _, hostID := range dedupe(targets) {
		hub := h.reg.Hub(hostID)
		if hub == nil {
			continue
		}
		h.pushMu.Lock()
		_, _ = execOnAgent(hub, "proxy-unbind-"+randomToken(4),
			fmt.Sprintf("rm -f %s %s && docker exec %s nginx -s reload 2>/dev/null || true",
				fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID),
				proxyCleanupCertGlob(app), nginxContainer), 30)
		h.pushMu.Unlock()
	}

	_ = h.store.UpdateApp(app.ID, func(a *Application) error {
		a.ProxyDomains = nil
		a.ProxyMode = ""
		a.ProxyUpstream = ""
		// The gateway host must go too, or a later rebind would leave the
		// stale gateway id behind (and the app detail would keep showing a
		// gateway binding that no longer exists).
		a.ProxyGatewayHostID = ""
		return nil
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// proxyCleanupCertGlob renders the per-domain cert/key paths for an app's
// bound domains as one shell argument list, used by unbind/purge cleanup.
func proxyCleanupCertGlob(app *Application) string {
	parts := make([]string, 0, len(app.ProxyDomains)*2)
	for _, b := range app.ProxyDomains {
		if b.Domain == "" {
			continue
		}
		parts = append(parts,
			fmt.Sprintf("%s/%s.crt", proxyCertDir, b.Domain),
			fmt.Sprintf("%s/%s.key", proxyCertDir, b.Domain))
	}
	if len(parts) == 0 {
		// Always keep at least one (harmless) path so the rm command shape
		// stays constant.
		return fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
	}
	return strings.Join(parts, " ")
}

// SyncProxyForHost updates the Nginx reverse-proxy configuration when an
// application moves to a new host (or rolls back).
// If oldHostID is non-empty and mode == "local", the old host's vhost is cleaned up.
func (h *ProxyHandler) SyncProxyForHost(app *Application, oldHostID string) error {
	if len(app.ProxyDomains) == 0 {
		return nil
	}
	mode := app.ProxyMode
	if mode == "" {
		mode = "local"
	}
	if mode == "local" {
		targetHub := h.reg.Hub(app.HostID)
		if targetHub == nil {
			return fmt.Errorf("目标主机 Agent 不在线")
		}
		if ag := h.reg.GetAgent(app.HostID); ag != nil && strings.EqualFold(ag.OS, "windows") {
			return fmt.Errorf("反代暂不支持 Windows 主机")
		}
		h.pushMu.Lock()
		ensureErr := h.ensureProxyNginx(targetHub)
		h.pushMu.Unlock()
		if ensureErr != nil {
			return ensureErr
		}
		// Resolve every binding up front: no partial cert set may land on the
		// target host when one of them fails.
		pairs, err := h.resolveBindingCerts(app)
		if err != nil {
			return err
		}
		upstream, err := h.defaultUpstream(app, "local", "")
		if err != nil {
			return err
		}
		h.pushMu.Lock()
		pushErr := func() error {
			for _, p := range pairs {
				if err := h.pushCertFiles(targetHub, p.binding.Domain, p.crt); err != nil {
					return err
				}
			}
			conf := renderVhosts(bindingList(pairs), upstream, true)
			confPath := fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
			if err := writeFileOnAgent(targetHub, confPath, []byte(conf)); err != nil {
				return fmt.Errorf("推送反代配置失败: %w", err)
			}
			return h.reloadNginx(targetHub)
		}()
		h.pushMu.Unlock()
		if pushErr != nil {
			return pushErr
		}
		if oldHostID != "" && oldHostID != app.HostID {
			if oldHub := h.reg.Hub(oldHostID); oldHub != nil {
				confPath := fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
				_, _ = execOnAgent(oldHub, "proxy-clean-"+randomToken(4),
					fmt.Sprintf("rm -f %s %s && docker exec %s nginx -s reload 2>/dev/null || true",
						confPath, proxyCleanupCertGlob(app), nginxContainer), 30)
			}
		}
		_ = h.store.UpdateApp(app.ID, func(a *Application) error {
			a.ProxyUpstream = upstream
			return nil
		})
		return nil
	}
	// Gateway mode:
	if app.ProxyGatewayHostID == "" {
		return fmt.Errorf("网关模式未记录网关主机 ID")
	}
	gatewayHub := h.reg.Hub(app.ProxyGatewayHostID)
	if gatewayHub == nil {
		return fmt.Errorf("网关主机 Agent 不在线")
	}
	upstream, err := h.defaultUpstream(app, "gateway", app.ProxyGatewayHostID)
	if err != nil {
		return err
	}
	conf := renderVhosts(app.ProxyDomains, upstream, true)
	confPath := fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
	h.pushMu.Lock()
	pushErr := func() error {
		if err := writeFileOnAgent(gatewayHub, confPath, []byte(conf)); err != nil {
			return fmt.Errorf("推送反代配置到网关失败: %w", err)
		}
		return h.reloadNginx(gatewayHub)
	}()
	h.pushMu.Unlock()
	if pushErr != nil {
		return pushErr
	}
	_ = h.store.UpdateApp(app.ID, func(a *Application) error {
		a.ProxyUpstream = upstream
		return nil
	})
	return nil
}

// bindingList extracts the domain bindings from resolved pairs.
func bindingList(pairs []domainCertPair) []DomainBinding {
	out := make([]DomainBinding, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.binding)
	}
	return out
}

// domainList extracts just the hostnames, for logs.
func domainList(bindings []DomainBinding) []string {
	out := make([]string, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b.Domain)
	}
	return out
}

// resolveBindingCerts resolves every domain binding of an app to its
// certificate. Explicit CertIDs are honored as-is; legacy entries (empty
// CertID, migrated from the single-domain era) fall back to the deterministic
// best match, which is then frozen back onto the record — the once-implicit
// choice becomes an explicit, durable fact the next caller can rely on.
func (h *ProxyHandler) resolveBindingCerts(app *Application) ([]domainCertPair, error) {
	pairs := make([]domainCertPair, 0, len(app.ProxyDomains))
	freeze := map[string]string{} // domain → cert id, for legacy entries
	for _, b := range app.ProxyDomains {
		domain := strings.ToLower(strings.TrimSpace(b.Domain))
		if domain == "" {
			continue
		}
		b.Domain = domain
		if b.CertID != "" {
			crt, ok := h.hub.Get(b.CertID)
			if !ok || crt == nil {
				return nil, fmt.Errorf("域名 %s 绑定的证书已不存在，请重新绑定证书", domain)
			}
			pairs = append(pairs, domainCertPair{binding: b, crt: crt})
			continue
		}
		crt, ok := h.hub.FindByDomain(domain)
		if !ok || crt == nil {
			return nil, fmt.Errorf("域名 %s 的存量绑定未记录证书，且证书中心没有覆盖它的证书，请先在应用详情绑定证书", domain)
		}
		freeze[domain] = crt.ID
		pairs = append(pairs, domainCertPair{binding: DomainBinding{Domain: domain, CertID: crt.ID}, crt: crt})
	}
	if len(freeze) > 0 {
		if err := h.store.UpdateApp(app.ID, func(a *Application) error {
			for i := range a.ProxyDomains {
				if id, ok := freeze[strings.ToLower(strings.TrimSpace(a.ProxyDomains[i].Domain))]; ok {
					a.ProxyDomains[i].CertID = id
				}
			}
			return nil
		}); err == nil {
			h.log.Info("存量域名绑定已按确定性匹配冻结证书", "app", app.Name, "frozen", freeze)
		}
	}
	return pairs, nil
}

// pushCertFiles writes the certificate chain and key for one domain onto the
// nginx host. Shared by the bind flow and the renewal re-push.
func (h *ProxyHandler) pushCertFiles(agentHub *rpc.Hub, domain string, crt *cert.Certificate) error {
	certBase := fmt.Sprintf("%s/%s", proxyCertDir, domain)
	if err := writeFileOnAgent(agentHub, certBase+".crt", []byte(crt.CertPEM)); err != nil {
		return fmt.Errorf("推送证书失败: %w", err)
	}
	if err := writeFileOnAgent(agentHub, certBase+".key", []byte(crt.KeyPEM)); err != nil {
		return fmt.Errorf("推送私钥失败: %w", err)
	}
	return nil
}

// errReloadTimeout marks an nginx reload that never answered within the
// deadline, so bind can answer 504 for it while config validation failures
// stay a plain 500.
var errReloadTimeout = errors.New("nginx reload 超时")

// Actionable ensure failures — host conditions the user can fix themselves.
// They answer 409 so the message reads as a host problem, not a server bug;
// everything else stays a generic 500 with a log excerpt.
var (
	errProxyDockerUnavailable = errors.New("目标主机 Docker 不可用：请先在主机详情 Docker 页一键安装或启动 Docker 后重试")
	errProxyPortConflict      = errors.New("目标主机 80/443 端口已被占用，反代 Nginx 无法监听：请检查主机上已有 Web 服务，或改用其他主机作为反代节点")
	errProxyImagePull         = errors.New("nginx:alpine 镜像拉取失败：请检查主机网络，或为主机配置 Docker 镜像加速后重试")
)

// Exit codes of the ensure scripts, checked by classifyEnsureError. The check
// script emits 0 (inspect line), 3 (container missing) or 4/5/6 (docker /
// daemon / mkdir broken); the run script emits 10 (docker run failed) or 11
// (container not running after start).
const (
	proxyEnsureExitNoContainer = 3
	proxyEnsureExitNoDocker    = 4
	proxyEnsureExitDaemonDown  = 5
	proxyEnsureExitMkdirFailed = 6
	proxyEnsureExitRunFailed   = 10
	proxyEnsureExitNotRunning  = 11
)

// proxyEnsureCheckScript probes whether the system proxy container is present
// and healthy. Exit 0 prints "status|mounts" for proxyContainerHealthy; the
// other exits map to classifyEnsureError. The mkdir is not incidental: the
// bind mounts need both host dirs to exist before docker run.
func proxyEnsureCheckScript() string {
	return fmt.Sprintf(`mkdir -p %s %s || exit %d
command -v docker >/dev/null 2>&1 || exit %d
docker info >/dev/null 2>&1 || exit %d
docker inspect %s >/dev/null 2>&1 || exit %d
echo "$(docker inspect --format '{{.State.Status}}' %s)|$(docker inspect --format '{{range .Mounts}}{{.Source}}:{{.Destination}};{{end}}' %s)"`,
		proxyConfigDir, proxyCertDir, proxyEnsureExitMkdirFailed,
		proxyEnsureExitNoDocker,
		proxyEnsureExitDaemonDown,
		nginxContainer, proxyEnsureExitNoContainer,
		nginxContainer, nginxContainer)
}

// proxyEnsureRunScript rebuilds the proxy container from scratch — same
// idempotent rm -f + run replacement pattern as the socat mesh forwarder.
// --network host is load-bearing: local-mode upstreams are 127.0.0.1:<port>,
// which under bridge networking would point at the container itself and
// blackhole traffic; host networking makes it the host's loopback and lets
// nginx bind 80/443 directly (no -p mappings to keep in sync). Both mounts
// are :ro — nginx only reads conf and certs, writes go through file pushes.
// The 2s settle + status check turns "started but immediately dying" (port
// conflict, bad config) into a classified error instead of a silent success.
func proxyEnsureRunScript() string {
	return fmt.Sprintf(`docker rm -f %s 2>/dev/null
docker run -d --name %s --restart unless-stopped --network host --label watchman.managed=proxy -v %s:/etc/nginx/conf.d:ro -v %s:/opt/apps/nginx/certs:ro nginx:alpine || exit %d
sleep 2
st=$(docker inspect --format '{{.State.Status}}' %s 2>/dev/null || echo missing)
if [ "$st" != "running" ]; then
  echo "STATUS=$st"
  docker logs --tail 30 %s 2>&1 || true
  exit %d
fi`,
		nginxContainer,
		nginxContainer, proxyConfigDir, proxyCertDir, proxyEnsureExitRunFailed,
		nginxContainer,
		nginxContainer, proxyEnsureExitNotRunning)
}

// proxyContainerHealthy parses the check script's "status|mounts" line: the
// container must be running with both host dirs mounted where the system
// expects them. Anything else (stopped, recreated by hand without the mounts,
// a user container squatting on the name) triggers a rebuild.
func proxyContainerHealthy(out string) bool {
	status, mounts, ok := strings.Cut(out, "|")
	if !ok || strings.TrimSpace(status) != "running" {
		return false
	}
	return strings.Contains(mounts, proxyConfigDir+":/etc/nginx/conf.d") &&
		strings.Contains(mounts, proxyCertDir+":/opt/apps/nginx/certs")
}

// ensureProxyNginx idempotently guarantees the system-managed reverse-proxy
// container on the nginx host — the 1Panel OpenResty / dokploy-traefik hosting
// model, minus their dependencies: no host-level nginx install, no docker.sock
// exposure, config still flows through audited file pushes + agent reloads.
// The check is a sub-second exec that leaves a healthy container untouched;
// missing/broken containers are rebuilt. Callers hold pushMu so a rebuild
// never races an in-flight cert/conf push or reload.
func (h *ProxyHandler) ensureProxyNginx(agentHub *rpc.Hub) error {
	res, err := execOnAgent(agentHub, "proxy-ensure-check-"+randomToken(4), proxyEnsureCheckScript(), 30)
	if err != nil {
		return fmt.Errorf("反代容器检查失败: %w", err)
	}
	switch res.GetExitCode() {
	case 0:
		if proxyContainerHealthy(string(res.GetStdout())) {
			return nil
		}
		h.log.Info("反代容器状态异常（停止或挂载缺失），正在重建")
	case proxyEnsureExitNoContainer:
		h.log.Info("反代容器不存在，正在初始化")
	default:
		// docker / daemon / mkdir broken — classified as-is, no rebuild
		// attempt that would fail the same way.
		return classifyEnsureError(res.GetExitCode(), string(res.GetStdout()), string(res.GetStderr()))
	}
	res, err = execOnAgent(agentHub, "proxy-ensure-run-"+randomToken(4), proxyEnsureRunScript(), 300)
	if err != nil {
		return fmt.Errorf("反代容器重建失败: %w", err)
	}
	if res.GetExitCode() != 0 {
		return classifyEnsureError(res.GetExitCode(), string(res.GetStdout()), string(res.GetStderr()))
	}
	return nil
}

// classifyEnsureError turns a failed ensure exec into the error the user
// sees. Pure string classification over the exec's exit code and captured
// output, so it is table-driven testable. Order matters: docker availability
// first (nothing else can succeed without it), then pull vs runtime vs
// port-conflict by output sniffing, generic + log excerpt last so no failure
// is ever silently swallowed.
func classifyEnsureError(exit int32, stdout, stderr string) error {
	switch {
	case exit == proxyEnsureExitNoDocker || exit == proxyEnsureExitDaemonDown:
		return errProxyDockerUnavailable
	case exit == proxyEnsureExitRunFailed && proxyPullFailure(stderr):
		return errProxyImagePull
	case strings.Contains(strings.ToLower(stdout+"\n"+stderr), "address already in use"):
		return errProxyPortConflict
	}
	excerpt := strings.TrimSpace(stderr)
	if excerpt == "" {
		excerpt = strings.TrimSpace(stdout)
	}
	if len(excerpt) > 200 {
		excerpt = excerpt[:200] + "…"
	}
	if excerpt != "" {
		return fmt.Errorf("反代 Nginx 容器保障失败: %s", excerpt)
	}
	return fmt.Errorf("反代 Nginx 容器保障失败（exit %d）", exit)
}

// proxyPullFailure reports whether a `docker run` failure was the image pull
// (registry unreachable / rate-limited / image gone) rather than a runtime
// error — the user-facing fix differs: 配置镜像加速 vs 查看日志.
func proxyPullFailure(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, kw := range []string{
		"unable to find image", "pull access denied", "manifest",
		"i/o timeout", "client.timeout exceeded", "tls handshake timeout",
		"dial tcp", "proxyconnect", "connection reset by peer", "unexpected eof",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// reloadNginx validates the pushed config (nginx -t) and hot-reloads nginx in
// the proxy container. Shared by the bind flow and the renewal re-push.
func (h *ProxyHandler) reloadNginx(agentHub *rpc.Hub) error {
	script := fmt.Sprintf(
		`docker exec %[1]s nginx -t 2>&1 && docker exec %[1]s nginx -s reload 2>&1`,
		nginxContainer)
	res, err := execOnAgent(agentHub, "proxy-reload-"+randomToken(4), script, 30)
	if err != nil {
		return fmt.Errorf("%w: %w", errReloadTimeout, err)
	}
	if res.GetExitCode() != 0 {
		out := string(res.GetStdout())
		if out == "" {
			out = string(res.GetStderr())
		}
		return fmt.Errorf("nginx 配置校验失败: %s", strings.TrimSpace(out))
	}
	return nil
}

// repushTarget names one app whose freshly stored certificate must be
// re-distributed, the domain bindings affected and the host its nginx runs on.
type repushTarget struct {
	app        *Application
	bindings   []DomainBinding
	targetHost string
}

// appsForCertPush selects what a freshly stored certificate must update.
// Identity first: a binding whose CertID equals the certificate is always
// re-pushed — a renewed certificate keeps serving exactly the domains it was
// bound to, and another certificate covering the same domain can never
// silently replace it. Legacy entries (empty CertID, pre-explicit-binding
// records) keep the domain-coverage rule, gated on the deterministic
// FindByDomain so two overlapping certificates don't take turns. nginx runs
// on the app host for local mode and on the gateway host for gateway mode.
func (h *ProxyHandler) appsForCertPush(crt *cert.Certificate, apps []*Application) []repushTarget {
	if crt == nil {
		return nil
	}
	var out []repushTarget
	for _, app := range apps {
		var matched []DomainBinding
		for _, b := range app.ProxyDomains {
			domain := strings.TrimSpace(strings.ToLower(b.Domain))
			if domain == "" {
				continue
			}
			if b.CertID == crt.ID {
				matched = append(matched, DomainBinding{Domain: domain, CertID: b.CertID})
				continue
			}
			if b.CertID == "" {
				if best, ok := h.hub.FindByDomain(domain); ok && best.ID == crt.ID {
					matched = append(matched, DomainBinding{Domain: domain, CertID: crt.ID})
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		targetHost := app.HostID
		if app.ProxyMode == "gateway" && app.ProxyGatewayHostID != "" {
			targetHost = app.ProxyGatewayHostID
		}
		out = append(out, repushTarget{app: app, bindings: matched, targetHost: targetHost})
	}
	return out
}

// RepushAppCerts re-distributes a freshly stored certificate to every nginx
// host serving a domain explicitly bound to it. Wired as the cert hub's
// OnRenewed callback (see Hub.SetOnRenewed) so renewals — and any new
// issuance or import bound to app domains — reach the serving hosts instead
// of silently going stale at expiry. System-triggered, not a user operation:
// per-app failures are logged and never block the remaining apps or the
// renewal flow itself (per §7 it stays out of the operation audit).
func (h *ProxyHandler) RepushAppCerts(crt *cert.Certificate) {
	if crt == nil || crt.CertPEM == "" || crt.KeyPEM == "" {
		return
	}
	// Wired through the router with both deps set; the nil guards keep the
	// hub callback safe in any other wiring (tests, future callers).
	if h.store == nil || h.reg == nil {
		return
	}
	for _, t := range h.appsForCertPush(crt, h.store.ListApps()) {
		agentHub := h.reg.Hub(t.targetHost)
		if agentHub == nil {
			h.log.Warn("证书回推跳过：nginx 主机 Agent 不在线", "app", t.app.Name, "host_id", t.targetHost)
			continue
		}
		h.pushMu.Lock()
		err := error(nil)
		for _, b := range t.bindings {
			if err = h.pushCertFiles(agentHub, b.Domain, crt); err != nil {
				break
			}
		}
		if err == nil {
			err = h.reloadNginx(agentHub)
		}
		h.pushMu.Unlock()
		if err != nil {
			h.log.Warn("证书回推失败", "app", t.app.Name, "domains", domainList(t.bindings), "error", err.Error())
			continue
		}
		h.log.Info("证书回推完成", "app", t.app.Name, "cert", crt.ID, "bindings", len(t.bindings))
	}
}

// defaultUpstream derives the proxy target for the app.
//
// Gateway mode has two paths to the app host and 异地组网 is NOT a
// prerequisite:
//   - 组网路径：app host and gateway host are BOTH in the overlay → use the app
//     host's 100.x address (works even behind NAT);
//   - 直连路径：otherwise proxy straight to the app host's public IP, falling
//     back to its internal IP (same LAN / VPC).
//
// 127.0.0.1 is never used as a gateway upstream: it would point at the gateway
// itself and blackhole traffic.
func (h *ProxyHandler) defaultUpstream(app *Application, mode, gatewayHostID string) (string, error) {
	var port int
	if len(app.Ports) > 0 {
		port = app.Ports[0].Host
	}
	if port == 0 {
		port = 8080
	}
	if mode == "local" {
		return fmt.Sprintf("127.0.0.1:%d", port), nil
	}

	appMesh, gwMesh := "", ""
	if h.networkStore != nil {
		if st, ok := h.networkStore.GetNodeStatus(app.HostID); ok {
			appMesh = st.IP
		}
		if gatewayHostID != "" {
			if st, ok := h.networkStore.GetNodeStatus(gatewayHostID); ok {
				gwMesh = st.IP
			}
		}
	}
	appPublicIP, appInternalIP := "", ""
	if ag := h.reg.GetAgent(app.HostID); ag != nil {
		appPublicIP, appInternalIP = ag.PublicIP, ag.InternalIP
	}
	return resolveGatewayUpstream(port, appMesh, gwMesh, appPublicIP, appInternalIP)
}

// resolveGatewayUpstream picks the gateway → app host upstream:
//   - 组网：both ends carry a mesh IP → the app host's 100.x;
//   - 直连：otherwise the app host's public IP, then its internal IP.
//
// 127.0.0.1 is deliberately never returned (it would point at the gateway
// itself and blackhole traffic); when nothing resolves the caller must ask the
// user for an explicit Upstream.
func resolveGatewayUpstream(port int, appMesh, gwMesh, appPublicIP, appInternalIP string) (string, error) {
	if appMesh != "" && gwMesh != "" {
		return fmt.Sprintf("%s:%d", appMesh, port), nil
	}
	if appPublicIP != "" {
		return fmt.Sprintf("%s:%d", appPublicIP, port), nil
	}
	if appInternalIP != "" {
		return fmt.Sprintf("%s:%d", appInternalIP, port), nil
	}
	return "", fmt.Errorf("无法确定网关到应用主机的上游地址：应用主机未加入异地组网，且没有可用的公网/内网 IP；请在应用详情「域名与反代」手动填写 Upstream")
}

// validProxyDomain enforces a strict RFC-1035-ish hostname shape.
func validProxyDomain(d string) bool {
	if len(d) == 0 || len(d) > 253 {
		return false
	}
	for _, label := range strings.Split(d, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// renderVhosts produces the nginx config for one application: per domain an
// 80→443 redirect block plus a 443 server block carrying that domain's own
// certificate (each domain may come from a different CA/vendor). All blocks
// proxy to the same upstream; nginx SNI picks the right cert per name.
func renderVhosts(bindings []DomainBinding, upstream string, websocket bool) string {
	var b strings.Builder
	for _, bd := range bindings {
		domain := bd.Domain
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
		fmt.Fprintf(&b, "    ssl_protocols TLSv1.2 TLSv1.3;\n")
		// nginx defaults to a 1m body limit; proxied uploads would be silently
		// truncated with a confusing 413 for the app behind the vhost.
		fmt.Fprintf(&b, "    client_max_body_size 64m;\n\n")
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
		fmt.Fprintf(&b, "}\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
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

	// The agent expects the initial op followed by data chunks; chunk sequence
	// starts at 0.
	seq := uint32(0)
	for offset := 0; offset < len(data); offset += writeChunkSize {
		end := offset + writeChunkSize
		if end > len(data) {
			end = len(data)
		}
		hub.Send(&agentpb.ServerMessage{
			Payload: &agentpb.ServerMessage_FileOp{
				FileOp: &agentpb.FileOp{
					OpId:     opID,
					Op:       "write",
					Path:     path,
					Offset:   int64(offset),
					ChunkSeq: seq,
					Data:     data[offset:end],
					// The agent's write session completes only when a frame
					// carries total_size and the byte count reaches it, so the
					// final chunk must repeat the header's TotalSize.
					TotalSize: int64(len(data)),
					Sha256:    expected,
				},
			},
		})
		seq++
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
