package network

import (
	"log/slog"
	"os"
	"testing"
)

func TestNetworkStore(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	store, err := NewStore(dir, log)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	cfg := store.GetConfig()
	if cfg.ControlPlane != "headscale" {
		t.Errorf("expected default headscale, got %s", cfg.ControlPlane)
	}

	cfg.ServerURL = "https://hs.example.com"
	cfg.AuthKey = "tskey-auth-123456"
	if err := store.UpdateConfig(cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}

	got := store.GetConfig()
	if got.ServerURL != "https://hs.example.com" || got.AuthKey != "tskey-auth-123456" {
		t.Errorf("unexpected updated config: %+v", got)
	}

	// Test node status
	st := NodeStatus{
		HostID:    "h1",
		Installed: true,
		Online:    true,
		IP:        "100.64.0.10",
		NodeName:  "node-1.ts.net",
	}
	if err := store.SetNodeStatus(st); err != nil {
		t.Fatalf("SetNodeStatus failed: %v", err)
	}

	node, ok := store.GetNodeStatus("h1")
	if !ok || node.IP != "100.64.0.10" {
		t.Errorf("unexpected node: %+v", node)
	}

	nodes := store.GetAllNodes()
	if len(nodes) != 1 {
		t.Errorf("expected 1 node, got %d", len(nodes))
	}
}
