// Package main is the watchman control server entrypoint.
//
// It starts the gRPC server (agents dial in) and the REST/WS HTTP server
// (browsers connect) on a single port via h2c-aware muxing. For MVP the two
// are on separate listeners (gRPC on :9090, HTTP on :8080); a later step
// collapses them onto :443.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/ai"
	"watchman/server/internal/alert"
	"watchman/server/internal/api"
	"watchman/server/internal/apps"
	"watchman/server/internal/audit"
	"watchman/server/internal/auth"
	"watchman/server/internal/backup"
	"watchman/server/internal/cert"
	"watchman/server/internal/commands"
	"watchman/server/internal/gitprovider"
	"watchman/server/internal/groups"
	"watchman/server/internal/install"
	"watchman/server/internal/metrics"
	"watchman/server/internal/network"
	"watchman/server/internal/policy"
	"watchman/server/internal/prefs"
	"watchman/server/internal/rpc"
	"watchman/server/internal/scan"
	"watchman/server/internal/session"
	"watchman/server/internal/snapshots"
	"watchman/server/internal/vault"
	"watchman/server/internal/ws"

	"google.golang.org/grpc"
)

func main() {
	grpcAddr := flag.String("grpc", ":9090", "gRPC listen address (agents)")
	httpAddr := flag.String("http", ":18080", "HTTP listen address (browsers)")
	dataDir := flag.String("data", "./data", "data directory (users.json, sessions, etc.)")
	jwtKey := flag.String("jwt-key", "", "JWT signing key (random if empty)")
	vaultPass := flag.String("vault-pass", "", "Passphrase for credential vault encryption (random if empty)")
	aiURL := flag.String("ai-url", "", "LLM endpoint URL (Ollama: http://localhost:11434, vLLM: http://localhost:8000/v1)")
	aiModel := flag.String("ai-model", "qwen2.5:14b", "LLM model name")
	aiKey := flag.String("ai-key", "", "LLM API key (for vLLM/OpenAI-compatible; empty for Ollama)")
	wsOrigins := flag.String("ws-origins", "", "comma-separated extra browser origins allowed to open WebSocket sessions (same-origin and loopback are always allowed; \"*\" disables the check)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(log)

	// Browser origins allowed to open terminal/file WebSocket sessions.
	if *wsOrigins != "" {
		list := strings.Split(*wsOrigins, ",")
		ws.SetAllowedOrigins(list)
		log.Info("ws extra allowed origins", "origins", list)
	}

	// User store + JWT auth.
	authStore, err := auth.NewStore(*dataDir, *jwtKey)
	if err != nil {
		log.Error("auth store init", "err", err)
		os.Exit(1)
	}
	log.Info("auth store ready", "users", len(authStore.List()), "data_dir", *dataDir)
	log.Info("default admin account", "username", "admin", "password", "admin", "note", "change after first login")

	// Session audit store (terminal recordings).
	sessStore, err := session.NewStore(*dataDir)
	if err != nil {
		log.Error("session store init", "err", err)
		os.Exit(1)
	}
	log.Info("session audit store ready", "record_dir", sessStore.RecordDir())

	// Alert store (rules + events + webhook).
	alertStore, err := alert.NewStore(*dataDir)
	if err != nil {
		log.Error("alert store init", "err", err)
		os.Exit(1)
	}
	log.Info("alert store ready", "rules", len(alertStore.ListRules()))

	// Credential vault (AES-256-GCM encrypted).
	vaultStore, err := vault.NewStore(*dataDir, *vaultPass)
	if err != nil {
		log.Error("vault init", "err", err)
		os.Exit(1)
	}
	log.Info("credential vault ready", "credentials", len(vaultStore.List()))

	// Metrics historical store (7-day retention).
	metricsStore, err := metrics.NewStore(*dataDir, log)
	if err != nil {
		log.Error("metrics store init", "err", err)
		os.Exit(1)
	}
	log.Info("metrics historical store ready")

	// Scan job store (security scanning).
	scanStore, err := scan.NewStore(*dataDir, log)
	if err != nil {
		log.Error("scan store init", "err", err)
		os.Exit(1)
	}
	log.Info("scan store ready", "jobs", len(scanStore.ListJobs("", "", "")))

	// High-risk command policy store.
	policyStore, err := policy.NewStore(*dataDir, log)
	if err != nil {
		log.Error("policy store init", "err", err)
		os.Exit(1)
	}
	log.Info("command policy store ready", "enabled", policyStore.GetPolicy().Enabled)

	// Unified operational audit trail.
	auditStore, err := audit.NewStore(*dataDir, log)
	if err != nil {
		log.Error("audit store init", "err", err)
		os.Exit(1)
	}
	log.Info("audit store ready")

	// Saved-command library (推送命令 quick picks).
	commandStore, err := commands.NewStore(*dataDir, log)
	if err != nil {
		log.Error("command library init", "err", err)
		os.Exit(1)
	}
	log.Info("command library ready", "entries", len(commandStore.List("", "", "")))

	// Host groups + per-group user authorization.
	groupStore, err := groups.NewStore(*dataDir, log)
	if err != nil {
		log.Error("group store init", "err", err)
		os.Exit(1)
	}
	log.Info("group store ready", "groups", len(groupStore.List()))

	// Per-user terminal preferences (theme / shell / font), server-side so the
	// terminal looks the same from any machine the user logs in from.
	prefsStore, err := prefs.NewStore(*dataDir, log)
	if err != nil {
		log.Error("term prefs store init", "err", err)
		os.Exit(1)
	}
	log.Info("term prefs store ready")

	// Control-plane backup/restore, so the whole dataset can be archived and
	// moved to another machine (AGENTS.md 5.1 数据自主可控).
	backupStore, err := backup.NewStore(*dataDir, log)
	if err != nil {
		log.Error("backup store init", "err", err)
		os.Exit(1)
	}
	log.Info("backup store ready", "backup_dir", backupStore.BackupDir(), "archives", len(backupStore.List()))

	// Overlay networking store (Tailscale / Headscale).
	networkStore, err := network.NewStore(*dataDir, log)
	if err != nil {
		log.Error("network store init", "err", err)
		os.Exit(1)
	}
	log.Info("overlay network store ready")

	// Git-linked application store + deployment engine (Dokploy-style
	// application lifecycle management).
	appStore, err := apps.NewStore(*dataDir, log)
	if err != nil {
		log.Error("app store init", "err", err)
		os.Exit(1)
	}
	log.Info("application store ready", "apps", len(appStore.ListApps()))

	// SSL certificate hub (ACME DNS-01 delegated to the dns-mng service).
	certHub, err := cert.NewHub(*dataDir, log)
	if err != nil {
		log.Error("cert hub init", "err", err)
		os.Exit(1)
	}
	log.Info("certificate hub ready", "certs", len(certHub.List()))

	reg := rpc.NewRegistry(*dataDir, log)
	appEngine := apps.NewEngine(reg, appStore, vaultStore, log)
	appEngine.SetNetworkStore(networkStore)

	// Git provider integration (GitHub authorization & repo selector).
	gitProviderStore, err := gitprovider.NewStore(*dataDir, log)
	if err != nil {
		log.Error("git provider store init", "err", err)
		os.Exit(1)
	}
	appEngine.SetGitProvider(gitProviderStore)
	log.Info("git provider store ready")

	// Host & container volume snapshots / database hot-backup.
	snapshotStore, err := snapshots.NewStore(*dataDir, log)
	if err != nil {
		log.Error("snapshot store init", "err", err)
		os.Exit(1)
	}
	snapshotEngine := snapshots.NewEngine(reg, snapshotStore, log)
	log.Info("snapshot store ready", "jobs", len(snapshotStore.ListJobs()))

	// AI diagnostics assistant (runtime configurable via settings).
	aiCfg := ai.Config{
		BaseURL: *aiURL,
		Model:   *aiModel,
		APIKey:  *aiKey,
		Enabled: *aiURL != "",
	}
	aiAssistant := ai.NewAssistant(*dataDir, aiCfg, reg, log)
	// Inject metrics + alert stores for ops reports / natural language Q&A.
	aiAssistant.SetMetricsStore(ai.NewMetricsStoreAdapter(metricsStore))
	aiAssistant.SetAlertStore(&alertStoreAdapter{store: alertStore})
	// Inject the command policy so multi-step plans flag high-risk steps.
	if policyStore != nil {
		aiAssistant.SetPolicyChecker(policyStore)
	}
	currentAICfg := aiAssistant.Config()
	if currentAICfg.Enabled && currentAICfg.BaseURL != "" {
		log.Info("AI diagnostics enabled", "url", currentAICfg.BaseURL, "model", currentAICfg.Model)
	} else {
		log.Info("AI diagnostics ready (configure in Settings UI)")
	}

	grpcSrv := rpc.NewServer(reg, log)

	// gRPC server (agent inbound).
	gs := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(gs, grpcSrv)
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Error("grpc listen", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go grpcSrv.StartReaper(ctx)

	// Background metrics auto-collector (15s) and 7-day retention janitor.
	go metricsStore.StartAutoCollector(ctx, reg, 15*time.Second)
	go metricsStore.StartJanitor(ctx, 7, 1*time.Hour)

	// Alert monitor: evaluates rules every 30s against the live registry.
	// metricsStore enables anomaly detection; aiAssistant enables alert interpretation.
	alertMonitor := alert.NewMonitor(alertStore, alert.NewHostProvider(reg), metricsStore, aiAssistant, log)
	go alertMonitor.Start(ctx, 30*time.Second)

	// Certificate renewal loop: auto-renews certificates expiring within 30
	// days through the dns-mng DNS-01 delegation.
	certHub.RunRenewalLoop(ctx.Done())

	// Snapshot scheduler: evaluates cron expressions on backup jobs every minute.
	snapshotEngine.StartScheduler(ctx.Done(), log)

	// Application auto-healing: continuously checks application endpoints
	// and automatically restarts containers failing 3 consecutive probes.
	autoHealer := apps.NewAutoHealer(reg, appStore, log)
	autoHealer.Start(ctx.Done())

	go func() {
		log.Info("grpc server listening", "addr", *grpcAddr)
		if err := gs.Serve(lis); err != nil {
			log.Error("grpc serve", "err", err)
			cancel()
		}
	}()

	// HTTP server (REST + WS).
	hr := api.Router(reg, log, authStore, sessStore, alertStore, vaultStore, aiAssistant, metricsStore, scanStore, policyStore, auditStore, commandStore, groupStore, prefsStore, backupStore, networkStore, appStore, appEngine, certHub, snapshotStore, snapshotEngine, gitProviderStore, alertMonitor)
	// Install endpoints (one-line agent install + binary download).
	installHandler := install.NewHandler(reg, "bin")
	installHandler.RegisterRoutes(hr)
	hs := &http.Server{
		Addr:              *httpAddr,
		Handler:           hr,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("http server listening", "addr", *httpAddr)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http serve", "err", err)
			cancel()
		}
	}()

	// Wait for shutdown signal.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutdown signal", "sig", s)
	case <-ctx.Done():
	}

	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()

	// Broadcast maintenance notice to all connected agents before stopping gRPC,
	// allowing agents to enter scheduled maintenance state and suppress reconnect alerts.
	reg.BroadcastMaintenance("server_restart", 120)
	time.Sleep(150 * time.Millisecond) // brief pause to flush outbound frame

	_ = hs.Shutdown(shutdownCtx)
	gs.GracefulStop()
	log.Info("bye")
}

// alertStoreAdapter wraps *alert.Store to satisfy ai.alertStoreRef without
// creating a circular import (alert imports ai, not vice versa).
type alertStoreAdapter struct {
	store *alert.Store
}

func (a *alertStoreAdapter) ListEvents(limit int) []ai.AlertEventProxy {
	events := a.store.ListEvents(limit)
	out := make([]ai.AlertEventProxy, len(events))
	for i, e := range events {
		out[i] = ai.AlertEventProxy{
			ID:       e.ID,
			RuleName: e.RuleName,
			Severity: string(e.Severity),
			HostID:   e.HostID,
			Hostname: e.Hostname,
			Message:  e.Message,
			FiredAt:  e.FiredAt,
			Resolved: e.Resolved,
		}
	}
	return out
}
