package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeServerAddr(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"wx1.us.ci":             "wx1.us.ci:9090",
		"wx1.us.ci:9090":        "wx1.us.ci:9090",
		"https://wx1.us.ci/":    "wx1.us.ci:9090",
		"http://1.2.3.4:9090/x": "1.2.3.4:9090",
		"1.2.3.4":               "1.2.3.4:9090",
		"[::1]:9090":            "[::1]:9090",
		"  1.2.3.4  ":           "1.2.3.4:9090",
	}
	for in, want := range cases {
		if got := NormalizeServerAddr(in); got != want {
			t.Errorf("NormalizeServerAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedirectStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	cfg, err := Load(stateFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.ServerAddr = "192.255.178.173:9090"
	now := time.Now()

	// No redirect: the flag address is the effective address.
	if got := cfg.EffectiveAddr(); got != "192.255.178.173:9090" {
		t.Fatalf("EffectiveAddr without redirect = %q", got)
	}

	// Applying a redirect persists it and outranks the flag.
	if err := cfg.SetRedirect("35.212.206.45:9090", "192.255.178.173:9090", now); err != nil {
		t.Fatalf("SetRedirect: %v", err)
	}
	if got := cfg.EffectiveAddr(); got != "35.212.206.45:9090" {
		t.Fatalf("EffectiveAddr with redirect = %q", got)
	}
	reloaded, err := Load(stateFile)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.EffectiveAddr(); got != "35.212.206.45:9090" {
		t.Fatalf("reloaded EffectiveAddr = %q, redirect did not survive restart", got)
	}
	if target, prev := reloaded.RedirectState(); target != "35.212.206.45:9090" || prev != "192.255.178.173:9090" {
		t.Fatalf("reloaded redirect state = (%q, %q)", target, prev)
	}

	// Falling back records the failure and returns to the flag address.
	if err := reloaded.NoteRedirectFailed(now); err != nil {
		t.Fatalf("NoteRedirectFailed: %v", err)
	}
	if got := reloaded.EffectiveAddr(); got != "192.255.178.173:9090" {
		t.Fatalf("EffectiveAddr after fallback = %q", got)
	}
	if got := reloaded.FailedTargetForReport(now); got != "35.212.206.45:9090" {
		t.Fatalf("FailedTargetForReport = %q", got)
	}
	// The same target is in cooldown; a different one is not.
	if !reloaded.RedirectCoolingDown("35.212.206.45:9090", now) {
		t.Fatalf("failed target should be in cooldown")
	}
	if reloaded.RedirectCoolingDown("35.212.206.46:9090", now) {
		t.Fatalf("different target should not be in cooldown")
	}
	// After the cooldown the failure clears itself from reports.
	if got := reloaded.FailedTargetForReport(now.Add(RedirectCooldown + time.Minute)); got != "" {
		t.Fatalf("FailedTargetForReport after cooldown = %q, want empty", got)
	}
	if reloaded.RedirectCoolingDown("35.212.206.45:9090", now.Add(RedirectCooldown+time.Minute)) {
		t.Fatalf("cooldown should have expired")
	}

	// Clearing drops everything.
	if err := reloaded.ClearRedirect(); err != nil {
		t.Fatalf("ClearRedirect: %v", err)
	}
	if target, prev := reloaded.RedirectState(); target != "" || prev != "" {
		t.Fatalf("redirect state after clear = (%q, %q)", target, prev)
	}
	if got := reloaded.FailedTargetForReport(now); got != "" {
		t.Fatalf("FailedTargetForReport after clear = %q", got)
	}
}

func TestRedirectReversal(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.ServerAddr = "10.0.0.1:9090"
	now := time.Now()

	if cfg.RedirectReversal("10.0.0.1:9090", now) {
		t.Fatalf("reversal flagged with no redirect active")
	}
	if err := cfg.SetRedirect("10.0.0.2:9090", "10.0.0.1:9090", now); err != nil {
		t.Fatalf("SetRedirect: %v", err)
	}
	// Going straight back within the window is a likely loop.
	if !cfg.RedirectReversal("10.0.0.1:9090", now) {
		t.Fatalf("rapid reversal not detected")
	}
	// A different target is not a reversal.
	if cfg.RedirectReversal("10.0.0.3:9090", now) {
		t.Fatalf("different target flagged as reversal")
	}
	// After the window a deliberate rollback is accepted.
	if cfg.RedirectReversal("10.0.0.1:9090", now.Add(RedirectLoopGuard+time.Minute)) {
		t.Fatalf("reversal after the window still flagged")
	}
}

func TestSyncAgentConf(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	cfg, err := Load(stateFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// No agent.conf: a no-op, not an error.
	if err := cfg.SyncAgentConf("35.212.206.45:9090"); err != nil {
		t.Fatalf("SyncAgentConf without file: %v", err)
	}

	confPath := filepath.Join(dir, "agent.conf")
	if err := os.WriteFile(confPath, []byte("server=192.255.178.173:9090\ntoken=secret-token\n"), 0o644); err != nil {
		t.Fatalf("write agent.conf: %v", err)
	}
	if err := cfg.SyncAgentConf("35.212.206.45"); err != nil {
		t.Fatalf("SyncAgentConf: %v", err)
	}
	b, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read agent.conf: %v", err)
	}
	if want := "server=35.212.206.45:9090\ntoken=secret-token\n"; string(b) != want {
		t.Fatalf("agent.conf = %q, want %q", string(b), want)
	}
	// The rewrite preserves the original file's mode (the test file was
	// created under the sandbox umask, so assert preservation, not a
	// fixed value).
	infoBefore, err := os.Stat(confPath)
	if err != nil {
		t.Fatalf("stat agent.conf: %v", err)
	}
	if err := cfg.SyncAgentConf("35.212.206.46:9090"); err != nil {
		t.Fatalf("SyncAgentConf: %v", err)
	}
	if infoAfter, err := os.Stat(confPath); err != nil || infoAfter.Mode().Perm() != infoBefore.Mode().Perm() {
		t.Fatalf("agent.conf mode changed: %v -> %v", infoBefore.Mode(), infoAfter.Mode())
	}

	// A file without a server= line gets one appended.
	if err := os.WriteFile(confPath, []byte("token=secret-token\n"), 0o644); err != nil {
		t.Fatalf("write agent.conf: %v", err)
	}
	if err := cfg.SyncAgentConf("35.212.206.45:9090"); err != nil {
		t.Fatalf("SyncAgentConf: %v", err)
	}
	b, err = os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read agent.conf: %v", err)
	}
	if want := "token=secret-token\nserver=35.212.206.45:9090\n"; string(b) != want {
		t.Fatalf("agent.conf = %q, want %q", string(b), want)
	}
}
