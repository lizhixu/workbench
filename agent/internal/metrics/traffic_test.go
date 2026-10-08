package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestTracker(t *testing.T) *trafficTracker {
	t.Helper()
	dir := t.TempDir()
	return newTrafficTracker(filepath.Join(dir, "state.json"), 1, nil)
}

const (
	testBootA = int64(1700000000)
	testBootB = int64(1700003600) // host rebooted ~1h later
)

func TestFirstSampleBaseslineWithoutCountingBootTraffic(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	rx, tx := tr.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, now)
	if rx != 0 || tx != 0 {
		t.Fatalf("first sample must baseline at zero, got rx=%d tx=%d", rx, tx)
	}
}

func TestAccumulatesDeltas(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	tr.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, now)
	rx, tx := tr.account(map[string]ifaceCounters{"eth0": {1_500_000, 700_000}}, testBootA, now.Add(15*time.Second))
	if rx != 500_000 || tx != 200_000 {
		t.Fatalf("expected deltas 500000/200000, got %d/%d", rx, tx)
	}
}

func TestRebootViaBootTimeChangeCountsFromZero(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	tr.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, now)
	// Host reboots: boot time changes and counters restart from small values.
	rx, tx := tr.account(map[string]ifaceCounters{"eth0": {30_000, 10_000}}, testBootB, now.Add(time.Minute))
	if rx != 30_000 || tx != 10_000 {
		t.Fatalf("reboot should count traffic since boot, expected 30000/10000, got %d/%d", rx, tx)
	}
	// Continue accumulating normally after the reboot.
	rx, tx = tr.account(map[string]ifaceCounters{"eth0": {40_000, 15_000}}, testBootB, now.Add(2*time.Minute))
	if rx != 40_000 || tx != 15_000 {
		t.Fatalf("post-reboot accumulation wrong, got %d/%d", rx, tx)
	}
}

func TestIfaceRemovalDoesNotPhantomAdd(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	tr.account(map[string]ifaceCounters{
		"eth0":    {1_000_000, 500_000},
		"vethABC": {200_000, 100_000},
	}, testBootA, now)
	rx, tx := tr.account(map[string]ifaceCounters{
		"eth0":    {1_500_000, 700_000},
		"vethABC": {300_000, 150_000},
	}, testBootA, now.Add(time.Minute))
	if rx != 600_000 || tx != 250_000 {
		t.Fatalf("expected 600000/250000, got %d/%d", rx, tx)
	}
	// A docker network is removed: vethABC vanishes WITHOUT a reboot.
	// Only eth0's own delta may be counted; the vanished interface must
	// not cause the surviving total to be mistaken for a delta.
	rx, tx = tr.account(map[string]ifaceCounters{
		"eth0": {1_600_000, 750_000},
	}, testBootA, now.Add(2*time.Minute))
	if rx != 700_000 || tx != 300_000 {
		t.Fatalf("iface removal must not phantom-add, expected 700000/300000, got %d/%d", rx, tx)
	}
}

func TestSingleIfaceCounterReset(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	tr.account(map[string]ifaceCounters{
		"eth0": {1_000_000, 500_000},
		"eth1": {2_000_000, 800_000},
	}, testBootA, now)
	// eth0's driver reloads (its counters reset) while eth1 is unaffected
	// and the host did NOT reboot: only eth0's current value counts.
	rx, tx := tr.account(map[string]ifaceCounters{
		"eth0": {30_000, 10_000},
		"eth1": {2_100_000, 820_000},
	}, testBootA, now.Add(time.Minute))
	if rx != 130_000 || tx != 30_000 {
		t.Fatalf("expected 130000/30000, got %d/%d", rx, tx)
	}
}

func TestCycleRolloverResetsCounters(t *testing.T) {
	tr := newTestTracker(t)
	aug := time.Date(2026, 8, 31, 23, 59, 0, 0, time.Local)
	tr.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, aug)
	tr.account(map[string]ifaceCounters{"eth0": {2_000_000, 800_000}}, testBootA, aug.Add(30*time.Second))
	rx, tx := tr.account(map[string]ifaceCounters{"eth0": {2_100_000, 830_000}}, testBootA, time.Date(2026, 9, 1, 0, 0, 30, 0, time.Local))
	if rx != 100_000 || tx != 30_000 {
		t.Fatalf("cycle rollover should count only the new-cycle delta, got %d/%d", rx, tx)
	}
}

func TestResetDayShiftsCycleBoundary(t *testing.T) {
	dir := t.TempDir()
	tr := newTrafficTracker(filepath.Join(dir, "state.json"), 15, nil)
	// Aug 10 with resetDay=15 still belongs to the July cycle.
	if got := tr.cycleKey(time.Date(2026, 8, 10, 0, 0, 0, 0, time.Local)); got != "2026-07" {
		t.Fatalf("expected 2026-07, got %s", got)
	}
	// Aug 20 rolls into the August cycle.
	if got := tr.cycleKey(time.Date(2026, 8, 20, 0, 0, 0, 0, time.Local)); got != "2026-08" {
		t.Fatalf("expected 2026-08, got %s", got)
	}
}

func TestStatePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)

	tr1 := newTrafficTracker(statePath, 1, nil)
	tr1.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, now)
	tr1.account(map[string]ifaceCounters{"eth0": {2_000_000, 900_000}}, testBootA, now.Add(time.Minute))
	if _, err := os.Stat(filepath.Join(dir, "traffic-state.json")); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// Simulate agent restart: new tracker loads the same state.
	tr2 := newTrafficTracker(statePath, 1, nil)
	rx, tx := tr2.account(map[string]ifaceCounters{"eth0": {2_200_000, 1_000_000}}, testBootA, now.Add(2*time.Minute))
	if rx != 1_200_000 || tx != 500_000 {
		t.Fatalf("restart lost accounting: expected 1200000/500000, got %d/%d", rx, tx)
	}
}

func TestSetResetDayPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	tr1 := newTrafficTracker(statePath, 1, nil)
	tr1.setResetDay(15)
	if tr1.resetDay != 15 {
		t.Fatalf("setResetDay did not apply, got %d", tr1.resetDay)
	}
	// Out-of-range values are ignored.
	tr1.setResetDay(0)
	tr1.setResetDay(29)
	if tr1.resetDay != 15 {
		t.Fatalf("setResetDay accepted an invalid value, got %d", tr1.resetDay)
	}

	// The server-pushed value survives an agent restart even though the
	// startup flag still says 1.
	tr2 := newTrafficTracker(statePath, 1, nil)
	if tr2.resetDay != 15 {
		t.Fatalf("server-pushed reset day must survive restart, got %d", tr2.resetDay)
	}
}

func TestLoopbackExcludedFromAccounting(t *testing.T) {
	tr := newTestTracker(t)
	if tr.ifaceAllowed("lo") {
		t.Fatal("lo must be excluded from traffic accounting")
	}
	if tr.ifaceAllowed("Loopback Pseudo-Interface 1") {
		t.Fatal("Windows loopback must be excluded from traffic accounting")
	}
	if !tr.ifaceAllowed("eth0") {
		t.Fatal("eth0 must be allowed by default")
	}
	if !tr.ifaceAllowed("docker0") {
		t.Fatal("docker0 must be allowed by default (documented multi-count caveat)")
	}
}

func TestAllowlistRestrictsInterfaces(t *testing.T) {
	dir := t.TempDir()
	tr := newTrafficTracker(filepath.Join(dir, "state.json"), 1, []string{"eth0", " eth1 "})
	if !tr.ifaceAllowed("eth0") || !tr.ifaceAllowed("eth1") {
		t.Fatal("allowlisted interfaces must be allowed")
	}
	if tr.ifaceAllowed("docker0") {
		t.Fatal("non-allowlisted interface must be excluded")
	}
	if tr.ifaceAllowed("lo") {
		t.Fatal("loopback must stay excluded even with an allowlist")
	}
}
