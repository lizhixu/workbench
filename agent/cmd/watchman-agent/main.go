// Package main is the watchman-agent entrypoint.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"watchman/agent/internal/config"
	"watchman/agent/internal/conn"
	"watchman/agent/internal/docker"
	"watchman/agent/internal/exec"
	"watchman/agent/internal/files"
	"watchman/agent/internal/metrics"
	"watchman/agent/internal/scan"
	"watchman/agent/internal/shell"
	"watchman/agent/internal/sysinfo"
	"watchman/agent/internal/upgrade"
)

func main() {
	server := flag.String("server", "localhost:9090", "control server address (host:port)")
	enroll := flag.String("enroll", "", "one-time enroll token (first registration only)")
	state := flag.String("state", config.DefaultStatePath(), "path to agent state file")
	trafficResetDay := flag.Int("traffic-reset-day", 1, "billing cycle reset day-of-month (1-28) for monthly traffic stats")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(log)

	cfg, err := config.Load(*state)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	cfg.ServerAddr = *server
	cfg.EnrollToken = *enroll
	cfg.StateFile = *state

	log.Info("watchman-agent starting",
		"server", cfg.ServerAddr,
		"state", cfg.StateFile,
		"has_token", cfg.State.AuthToken != "",
		"has_enroll", cfg.EnrollToken != "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d := conn.NewDialer(cfg, log)

	// Wire all subsystems.
	d.SetShellManager(shell.NewManager(log))
	d.SetFileManager(files.NewManager(log))
	d.SetExecManager(exec.NewManager(log))
	mm := metrics.NewManager(log)
	mm.SetStateFile(*state, *trafficResetDay)
	d.SetMetricsManager(mm)
	d.SetSysInfoManager(sysinfo.NewManager(log))
	d.SetDockerManager(docker.NewManager(log))
	d.SetUpgradeManager(upgrade.NewManager(log))
	d.SetScanManager(scan.NewManager(log))

	go d.Run(ctx)

	// Wait for shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")
	cancel()
}