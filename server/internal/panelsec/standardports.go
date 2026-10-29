package panelsec

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"watchman/server/internal/settings"
)

// PortStatus reports one standard-port listener of the panel.
type PortStatus struct {
	Port      int    `json:"port"`
	Desired   bool   `json:"desired"`
	Listening bool   `json:"listening"`
	Error     string `json:"error,omitempty"`
}

// StandardPorts keeps the panel reachable without an explicit port: :80 while
// a panel domain is bound, plus :443 while panel SSL is enabled. The panel's
// primary listener stays on its configured high port; these are additional
// listeners sharing the same handler, reconciled on every settings change and
// retried periodically so a port freed later (e.g. a container releasing :80)
// is picked up without a restart.
type StandardPorts struct {
	settings  *settings.Store
	handler   http.Handler
	tlsConfig *tls.Config
	log       *slog.Logger
	skip      map[int]bool

	mu       sync.Mutex
	servers  map[int]*http.Server
	errs     map[int]string
	baseErrs map[int]string
}

// NewStandardPorts builds a reconciler. mainAddr is the panel's primary listen
// address; a standard port already served by it (e.g. -http :80) is skipped to
// avoid an address conflict.
func NewStandardPorts(st *settings.Store, handler http.Handler, tlsConfig *tls.Config, mainAddr string, log *slog.Logger) *StandardPorts {
	if log == nil {
		log = slog.Default()
	}
	skip := map[int]bool{}
	if _, portStr, err := net.SplitHostPort(mainAddr); err == nil {
		if n, err := strconv.Atoi(portStr); err == nil {
			skip[n] = true
		}
	}
	return &StandardPorts{
		settings:  st,
		handler:   handler,
		tlsConfig: tlsConfig,
		log:       log,
		skip:      skip,
		servers:   map[int]*http.Server{},
		errs:      map[int]string{},
		baseErrs:  map[int]string{},
	}
}

// desiredStandardPorts derives the listener set from the panel security
// settings. Without a bound domain the panel stays on its primary port only.
func desiredStandardPorts(sec settings.PanelSecurityConfig) []int {
	if strings.TrimSpace(sec.PanelDomain) == "" {
		return nil
	}
	ports := []int{80}
	if sec.SSLEnabled {
		ports = append(ports, 443)
	}
	return ports
}

// Reconcile starts and stops listeners so they match the current settings.
func (p *StandardPorts) Reconcile() {
	if p == nil || p.settings == nil {
		return
	}
	desired := desiredStandardPorts(p.settings.PanelSecurity())
	want := make(map[int]bool, len(desired))
	for _, port := range desired {
		want[port] = true
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for port, srv := range p.servers {
		if want[port] {
			continue
		}
		delete(p.servers, port)
		delete(p.errs, port)
		delete(p.baseErrs, port)
		// Drain rather than hard-close: the settings write that disabled SSL
		// may be in flight on this very listener (unbind / SSL off), and its
		// response must still reach the browser that triggered it.
		go func(srv *http.Server) {
			ctx, cancelDone := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelDone()
			_ = srv.Shutdown(ctx)
		}(srv)
		p.log.Info("panel standard port listener stopped", "port", port)
	}
	for _, port := range desired {
		if p.skip[port] || p.servers[port] != nil {
			continue
		}
		srv, err := p.startLocked(port)
		if err != nil {
			base := standardPortError(err)
			if p.baseErrs[port] != base {
				p.baseErrs[port] = base
				p.errs[port] = base
				p.log.Warn("panel standard port listener failed", "port", port, "err", err)
			}
			if isAddrInUse(err) {
				// Name the process holding the port; detection shells out, so
				// it must not run while the reconciler lock is held.
				go p.enrichPortError(port, base)
			}
			continue
		}
		p.servers[port] = srv
		delete(p.errs, port)
		delete(p.baseErrs, port)
		p.log.Info("panel standard port listener started", "port", port)
	}
}

func (p *StandardPorts) startLocked(port int) (*http.Server, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	var serveLn net.Listener = ln
	if port == 443 {
		if p.tlsConfig == nil {
			_ = ln.Close()
			return nil, errors.New("TLS 配置不可用")
		}
		serveLn = tls.NewListener(ln, p.tlsConfig)
	}
	srv := &http.Server{Handler: p.handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		err := srv.Serve(serveLn)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		base := standardPortError(err)
		p.mu.Lock()
		// Only retire the entry if this server is still the live one for the
		// port; a concurrent stop/replace must not be clobbered.
		if p.servers[port] == srv {
			delete(p.servers, port)
			p.baseErrs[port] = base
			p.errs[port] = base
		}
		p.mu.Unlock()
		p.log.Warn("panel standard port listener exited", "port", port, "err", err)
		if isAddrInUse(err) {
			p.enrichPortError(port, base)
		}
	}()
	return srv, nil
}

// standardPortError renders a bind/serve failure as a short hint for the
// settings page. The raw error stays in the log; the user needs to know the
// cause and who holds the port (filled in by enrichPortError).
func standardPortError(err error) string {
	lower := strings.ToLower(err.Error())
	switch {
	case isAddrInUse(err):
		return "端口被其他服务占用（释放后 30 秒内自动恢复）"
	case strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "access is denied"):
		return "无权限监听（需要 root 或 CAP_NET_BIND_SERVICE 权限）"
	default:
		return "监听失败：" + err.Error()
	}
}

func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "address already in use") ||
		strings.Contains(lower, "only one usage of each socket address")
}

// enrichPortError upgrades the generic "port in use" message with the owning
// process name and PID. The listener may have started (or the cause changed)
// by the time detection finishes, so the update is guarded by the same base
// message that triggered it.
func (p *StandardPorts) enrichPortError(port int, base string) {
	msg := portErrorWithOwners(base, detectPortOwner(port))
	if msg == base {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.baseErrs[port] != base || p.servers[port] != nil {
		return
	}
	if p.errs[port] != msg {
		p.errs[port] = msg
		p.log.Warn("panel standard port occupied", "port", port, "detail", msg)
	}
}

// Run retries failed listeners on a fixed interval until ctx is cancelled.
func (p *StandardPorts) Run(ctx context.Context) {
	if p == nil {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Reconcile()
		}
	}
}

// Stop closes every standard-port listener (process shutdown).
func (p *StandardPorts) Stop() {
	if p == nil {
		return
	}
	p.mu.Lock()
	servers := p.servers
	p.servers = map[int]*http.Server{}
	p.mu.Unlock()
	for _, srv := range servers {
		_ = srv.Close()
	}
}

// Status reports the listener state of the currently desired standard ports.
func (p *StandardPorts) Status() []PortStatus {
	if p == nil || p.settings == nil {
		return nil
	}
	desired := desiredStandardPorts(p.settings.PanelSecurity())
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PortStatus, 0, len(desired))
	for _, port := range desired {
		st := PortStatus{Port: port, Desired: true, Error: p.errs[port]}
		st.Listening = p.skip[port] || p.servers[port] != nil
		out = append(out, st)
	}
	return out
}

var (
	stdPortsMu    sync.Mutex
	stdPorts      *StandardPorts
	panelHTTPPort string
)

// SetPanelHTTPPort records the primary panel listen port for the status
// endpoint and UI fallback links.
func SetPanelHTTPPort(port string) {
	stdPortsMu.Lock()
	panelHTTPPort = port
	stdPortsMu.Unlock()
}

// PanelHTTPPort returns the primary panel listen port ("" when unknown).
func PanelHTTPPort() string {
	stdPortsMu.Lock()
	defer stdPortsMu.Unlock()
	return panelHTTPPort
}

// SetStandardPorts publishes the process-wide reconciler so the panel status
// endpoint can report listener health.
func SetStandardPorts(p *StandardPorts) {
	stdPortsMu.Lock()
	stdPorts = p
	stdPortsMu.Unlock()
}

// StandardPortListening reports whether the published reconciler is currently
// serving the given standard port. It answers false when no reconciler is
// wired (tests, embedded usage) — callers use it to decide whether a redirect
// to the portless HTTPS endpoint can actually be followed.
func StandardPortListening(port int) bool {
	stdPortsMu.Lock()
	p := stdPorts
	stdPortsMu.Unlock()
	if p == nil {
		return false
	}
	return p.listening(port)
}

// listening reports whether port is served either by the primary panel
// listener (skip set) or by a live standard-port listener.
func (p *StandardPorts) listening(port int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skip[port] || p.servers[port] != nil
}

// StandardPortStatus reports the published reconciler's state; nil when none
// is wired (tests, embedded usage).
func StandardPortStatus() []PortStatus {
	stdPortsMu.Lock()
	p := stdPorts
	stdPortsMu.Unlock()
	if p == nil {
		return nil
	}
	return p.Status()
}
