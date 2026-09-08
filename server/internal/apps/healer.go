package apps

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"watchman/server/internal/rpc"
)

// AutoHealer polls applications with a configured HealthcheckURL every 30s.
// When an application fails 3 consecutive health checks, it automatically
// triggers a restart and logs the event (Dokploy-style auto-healing).
type AutoHealer struct {
	reg   *rpc.Registry
	store *Store
	log   *slog.Logger

	mu       sync.Mutex
	failures map[string]int // app_id -> consecutive failure count
}

// NewAutoHealer creates the application auto-healer.
func NewAutoHealer(reg *rpc.Registry, store *Store, log *slog.Logger) *AutoHealer {
	if log == nil {
		log = slog.Default()
	}
	return &AutoHealer{
		reg:      reg,
		store:    store,
		log:      log,
		failures: make(map[string]int),
	}
}

// Start launches the background health monitoring loop.
func (h *AutoHealer) Start(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				h.sweep()
			}
		}
	}()
}

func (h *AutoHealer) sweep() {
	apps := h.store.ListApps()
	for _, app := range apps {
		if app.HealthcheckURL == "" || app.ContainerName == "" {
			continue
		}
		hub := h.reg.Hub(app.HostID)
		if hub == nil {
			continue
		}
		go h.checkOne(hub, app)
	}
}

func (h *AutoHealer) checkOne(hub *rpc.Hub, app *Application) {
	res, err := execOnAgent(hub, "heal-chk-"+randomToken(4),
		fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --max-time 5 %s", shellQuote(app.HealthcheckURL)), 10)

	healthy := false
	if err == nil && res.GetExitCode() == 0 {
		code := strings.TrimSpace(string(res.GetStdout()))
		if strings.HasPrefix(code, "2") {
			healthy = true
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if healthy {
		if h.failures[app.ID] > 0 {
			h.log.Info("app recovered health", "app", app.ID, "url", app.HealthcheckURL)
			delete(h.failures, app.ID)
		}
		return
	}

	h.failures[app.ID]++
	count := h.failures[app.ID]
	h.log.Warn("app health check failed", "app", app.ID, "count", count, "url", app.HealthcheckURL)

	// Threshold: 3 consecutive failures trigger auto-restart.
	if count >= 3 {
		h.failures[app.ID] = 0 // reset counter before restarting
		h.log.Warn("triggering auto-healing restart for app", "app", app.ID, "container", app.ContainerName)
		go func(targetHub *rpc.Hub, targetContainer string, targetAppID string) {
			_, restartErr := execOnAgent(targetHub, "heal-rst-"+randomToken(4),
				fmt.Sprintf("docker restart %s", shellQuote(targetContainer)), 60)
			if restartErr != nil {
				h.log.Error("auto-healing restart failed", "app", targetAppID, "err", restartErr)
			} else {
				h.log.Info("auto-healing restart completed", "app", targetAppID)
			}
		}(hub, app.ContainerName, app.ID)
	}
}