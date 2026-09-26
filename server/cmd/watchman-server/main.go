// Command watchman-server is the control-plane entrypoint.
//
// It wires together every server/internal package that previously had no
// main: the gRPC agent endpoint (optional TLS), the HTTP REST API +
// WebSocket terminal gateway, the install-script/binary endpoints, the
// reverse TCP tunnel coordinator, background workers (metrics collection
// + retention janitor, alert monitor) and static frontend serving.
//
// Example:
//
//	watchman-server -grpc :9090 -http :18080 -data /opt/watchman/data \
//	    -bindir /opt/watchman/bin -web /opt/watchman/web/dist
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/ai"
	"watchman/server/internal/alert"
	"watchman/server/internal/api"
	"watchman/server/internal/auth"
	"watchman/server/internal/install"
	"watchman/server/internal/metrics"
	"watchman/server/internal/policy"
	"watchman/server/internal/rpc"
	"watchman/server/internal/scan"
	"watchman/server/internal/session"
	"watchman/server/internal/vault"

	"github.com/gin-gonic/gin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	grpcAddr := flag.String("grpc", ":9090", "gRPC listen address for agents")
	httpAddr := flag.String("http", ":18080", "HTTP listen address for REST API / WebSocket / install")
	dataDir := flag.String("data", "./data", "data directory (agents, enroll tokens, metrics, sessions)")
	binDir := flag.String("bindir", "./bin", "directory with agent binaries served by /install")
	webDir := flag.String("web", "", "frontend dist directory to serve (empty = no static serving)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate for gRPC (empty = insecure)")
	tlsKey := flag.String("tls-key", "", "TLS private key for gRPC")
	httpTLS := flag.Bool("http-tls", false, "serve HTTPS on -http using -tls-cert/-tls-key")
	jwtSecret := flag.String("jwt-secret", "", "JWT signing secret (auto-generated and persisted if empty)")
	upgradePriv := flag.String("upgrade-privkey", "", "hex Ed25519 seed to sign agent upgrades (agents pin the pubkey)")
	retentionDays := flag.Int("metrics-retention-days", 7, "metrics history retention in days")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Error("create data dir", "err", err)
		os.Exit(1)
	}

	// ---- Core registry + tunnel coordinator ----
	reg := rpc.NewRegistry(*dataDir, log)
	tc := rpc.NewTunnelCoordinator(reg, log)
	reg.SetTunnelCoordinator(tc)

	// ---- Persistent stores ----
	jwtKey := *jwtSecret
	if jwtKey == "" {
		var err error
		jwtKey, err = loadOrGenerateSecret(filepath.Join(*dataDir, "jwt.key"))
		if err != nil {
			log.Error("jwt secret", "err", err)
			os.Exit(1)
		}
	}
	authStore, err := auth.NewStore(*dataDir, jwtKey)
	must(log, "auth store", err)
	sessStore, err := session.NewStore(*dataDir)
	must(log, "session store", err)
	alertStore, err := alert.NewStore(*dataDir)
	must(log, "alert store", err)
	vaultStore, err := vault.NewStore(*dataDir, jwtKey)
	must(log, "vault store", err)
	metricsStore, err := metrics.NewStore(*dataDir, log)
	must(log, "metrics store", err)
	scanStore, err := scan.NewStore(*dataDir, log)
	must(log, "scan store", err)
	policyStore, err := policy.NewStore(*dataDir, log)
	must(log, "policy store", err)
	aiAssistant := ai.NewAssistant(*dataDir, ai.Config{Enabled: false}, reg, log)

	// ---- Upgrade signing (Ed25519) ----
	if *upgradePriv != "" {
		seed, err := hex.DecodeString(*upgradePriv)
		if err != nil || len(seed) != ed25519.SeedSize {
			log.Error("upgrade-privkey must be hex of 32 bytes")
			os.Exit(1)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		api.UpgradeSigner = func(version, sha256 string) []byte {
			return ed25519.Sign(priv, []byte(version+"\n"+sha256))
		}
		log.Info("upgrade signing enabled", "pubkey", hex.EncodeToString(pub))
	} else {
		log.Warn("upgrade signing disabled: set -upgrade-privkey and pin the pubkey on agents (-upgrade-pubkey)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ---- Background workers ----
	go metricsStore.StartAutoCollector(ctx, reg, 10*time.Second)
	go metricsStore.StartJanitor(ctx, *retentionDays, time.Hour)
	monitor := alert.NewMonitor(alertStore, alert.NewHostProvider(reg), metricsStore, aiAssistant, log)
	go monitor.Start(ctx, 30*time.Second)

	// ---- gRPC (agents) ----
	var grpcOpts []grpc.ServerOption
	switch {
	case *tlsCert != "" && *tlsKey != "":
		creds, err := credentials.NewServerTLSFromFile(*tlsCert, *tlsKey)
		must(log, "load TLS credentials", err)
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
		log.Info("gRPC TLS enabled")
	case *tlsCert != "" || *tlsKey != "":
		// Half-configured TLS must not silently fall back to plaintext.
		log.Error("-tls-cert and -tls-key must be set together")
		os.Exit(1)
	default:
		log.Warn("gRPC running without TLS: agent traffic is unencrypted; use -tls-cert/-tls-key")
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	agentpb.RegisterAgentServiceServer(grpcServer, rpc.NewServer(reg, log))
	grpcLis, err := net.Listen("tcp", *grpcAddr)
	must(log, "listen gRPC", err)
	go func() {
		log.Info("gRPC listening", "addr", *grpcAddr)
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Error("gRPC serve", "err", err)
		}
	}()

	// ---- HTTP (REST + WS + install + frontend) ----
	engine := api.Router(reg, log, authStore, sessStore, alertStore, vaultStore, aiAssistant, metricsStore, scanStore, policyStore)
	install.NewHandler(reg, *binDir).RegisterRoutes(engine)
	serveFrontend(engine, *webDir, log)

	httpServer := &http.Server{Addr: *httpAddr, Handler: engine}
	go func() {
		if *httpTLS {
			if *tlsCert == "" || *tlsKey == "" {
				log.Error("-http-tls requires -tls-cert and -tls-key")
				os.Exit(1)
			}
			log.Info("HTTPS listening", "addr", *httpAddr)
			if err := httpServer.ListenAndServeTLS(*tlsCert, *tlsKey); err != http.ErrServerClosed {
				log.Error("HTTPS serve", "err", err)
			}
			return
		}
		log.Info("HTTP listening", "addr", *httpAddr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Error("HTTP serve", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutCtx)
	grpcServer.GracefulStop()
	monitor.Stop()
	log.Info("stopped")
}

func must(log *slog.Logger, what string, err error) {
	if err != nil {
		log.Error("init failed", "what", what, "err", err)
		os.Exit(1)
	}
}

// loadOrGenerateSecret returns the persisted secret, generating one if needed.
func loadOrGenerateSecret(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil && len(data) >= 32 {
		return string(data), nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	hexed := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(hexed), 0o600); err != nil {
		return "", err
	}
	return hexed, nil
}

// serveFrontend serves a built SPA from webDir. API/WS/install routes take
// precedence; everything else falls back to index.html.
func serveFrontend(engine *gin.Engine, webDir string, log *slog.Logger) {
	if webDir == "" {
		return
	}
	index := filepath.Join(webDir, "index.html")
	if _, err := os.Stat(index); err != nil {
		log.Warn("web dir has no index.html, skipping static serving", "dir", webDir)
		return
	}
	engine.Static("/assets", filepath.Join(webDir, "assets"))
	engine.GET("/", func(c *gin.Context) { c.File(index) })
	engine.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if len(p) >= 4 && p[:4] == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.File(index)
	})
	log.Info("serving frontend", "dir", webDir)
}
