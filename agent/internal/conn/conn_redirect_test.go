package conn

import (
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"watchman/agent/internal/config"
)

// closedPort returns a loopback host:port nothing listens on (allocate a
// port, then close it). A small race if something else grabs it mid-test,
// but the listener below holds its own port for the whole test.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func newTestDialer(t *testing.T, serverAddr string) *Dialer {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.ServerAddr = serverAddr
	return NewDialer(cfg, slog.Default())
}

func TestProbeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	if !probeTCP(ln.Addr().String(), time.Second) {
		t.Fatalf("probeTCP(live listener) = false")
	}
	if probeTCP(closedPort(t), time.Second) {
		t.Fatalf("probeTCP(closed port) = true")
	}
}

func TestApplyRedirectFromServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	target := ln.Addr().String()

	d := newTestDialer(t, "192.255.178.173:9090")

	// Empty means "no opinion": nothing changes.
	if d.applyRedirectFromServer("") {
		t.Fatalf("empty target applied")
	}
	if got := d.cfg.EffectiveAddr(); got != "192.255.178.173:9090" {
		t.Fatalf("EffectiveAddr after empty = %q", got)
	}
	// Same address as current: no-op.
	if d.applyRedirectFromServer("192.255.178.173:9090") {
		t.Fatalf("same-address target applied")
	}
	// Unreachable target: ignored, agent stays put.
	if d.applyRedirectFromServer(closedPort(t)) {
		t.Fatalf("unreachable target applied")
	}
	if got := d.cfg.EffectiveAddr(); got != "192.255.178.173:9090" {
		t.Fatalf("EffectiveAddr after unreachable = %q", got)
	}
	// Reachable target: applied and persisted.
	if !d.applyRedirectFromServer(target) {
		t.Fatalf("reachable target not applied")
	}
	if got := d.cfg.EffectiveAddr(); got != target {
		t.Fatalf("EffectiveAddr after apply = %q, want %q", got, target)
	}
	if tgt, prev := d.cfg.RedirectState(); tgt != target || prev != "192.255.178.173:9090" {
		t.Fatalf("redirect state = (%q, %q)", tgt, prev)
	}
	// Re-applying the same target is a no-op (no second probe/reconnect).
	if d.applyRedirectFromServer(target) {
		t.Fatalf("same target re-applied")
	}
}

func TestApplyRedirectCooldown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	target := ln.Addr().String()

	d := newTestDialer(t, "192.255.178.173:9090")
	if !d.applyRedirectFromServer(target) {
		t.Fatalf("reachable target not applied")
	}
	// Simulate the fallback: the target failed after being applied.
	if err := d.cfg.NoteRedirectFailed(time.Now()); err != nil {
		t.Fatalf("NoteRedirectFailed: %v", err)
	}
	// Same target inside the cooldown is ignored even though it answers.
	if d.applyRedirectFromServer(target) {
		t.Fatalf("cooldown target applied")
	}
	if got := d.cfg.EffectiveAddr(); got != "192.255.178.173:9090" {
		t.Fatalf("EffectiveAddr during cooldown = %q", got)
	}
}

func TestApplyRedirectLoopGuard(t *testing.T) {
	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lnA.Close()
	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lnB.Close()
	aAddr, bAddr := lnA.Addr().String(), lnB.Addr().String()

	d := newTestDialer(t, aAddr)
	// Move A -> B.
	if !d.applyRedirectFromServer(bAddr) {
		t.Fatalf("A->B redirect not applied")
	}
	// An immediate reversal B -> A is refused: the two servers are
	// likely redirecting at each other, and flipping back would flap
	// the agent between them every second.
	if d.applyRedirectFromServer(aAddr) {
		t.Fatalf("rapid reversal applied")
	}
	if got := d.cfg.EffectiveAddr(); got != bAddr {
		t.Fatalf("EffectiveAddr after refused reversal = %q, want %q", got, bAddr)
	}
	// Rewind the redirect past the guard window: the same reversal is
	// now accepted (deliberate rollback).
	past := time.Now().Add(-(config.RedirectLoopGuard + time.Minute))
	if err := d.cfg.SetRedirect(bAddr, aAddr, past); err != nil {
		t.Fatalf("SetRedirect: %v", err)
	}
	if !d.applyRedirectFromServer(aAddr) {
		t.Fatalf("reversal after guard window not applied")
	}
}

func TestMaybeFallback(t *testing.T) {
	// Previous server: a live listener. New server: a closed port.
	prevLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer prevLn.Close()
	prev := prevLn.Addr().String()

	d := newTestDialer(t, prev)
	target := closedPort(t)
	if err := d.cfg.SetRedirect(target, prev, time.Now()); err != nil {
		t.Fatalf("SetRedirect: %v", err)
	}

	// First failure only arms the timer.
	if d.maybeFallback() {
		t.Fatalf("fallback fired on first failure")
	}
	if got := d.cfg.EffectiveAddr(); got != target {
		t.Fatalf("EffectiveAddr after first failure = %q", got)
	}
	// Still inside the window: no fallback.
	d.redirectFailedSince = time.Now().Add(-5 * time.Minute)
	if d.maybeFallback() {
		t.Fatalf("fallback fired inside the window")
	}
	// Past the window with the previous server reachable: fall back.
	d.redirectFailedSince = time.Now().Add(-(config.RedirectFallbackAfter + time.Minute))
	if !d.maybeFallback() {
		t.Fatalf("fallback did not fire past the window")
	}
	if got := d.cfg.EffectiveAddr(); got != prev {
		t.Fatalf("EffectiveAddr after fallback = %q, want %q", got, prev)
	}
	if got := d.cfg.FailedTargetForReport(time.Now()); got != target {
		t.Fatalf("FailedTargetForReport = %q, want %q", got, target)
	}

	// Past the window but the previous server is gone too: keep trying
	// the new address, never flap to a dead server.
	d2 := newTestDialer(t, closedPort(t))
	target2 := closedPort(t)
	if err := d2.cfg.SetRedirect(target2, closedPort(t), time.Now()); err != nil {
		t.Fatalf("SetRedirect: %v", err)
	}
	d2.redirectFailedSince = time.Now().Add(-(config.RedirectFallbackAfter + time.Minute))
	if d2.maybeFallback() {
		t.Fatalf("fallback fired with previous server unreachable")
	}
	if got := d2.cfg.EffectiveAddr(); got != target2 {
		t.Fatalf("EffectiveAddr = %q, want %q (should keep trying new)", got, target2)
	}
}
