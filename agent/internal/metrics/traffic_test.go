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
	return newTrafficTracker(filepath.Join(dir, "state.json"), 1)
}

func TestFirstSampleBaseslineWithoutCountingBootTraffic(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	rx, tx := tr.account(1_000_000, 500_000, now)
	if rx != 0 || tx != 0 {
		t.Fatalf("first sample must baseline at zero, got rx=%d tx=%d", rx, tx)
	}
}

func TestAccumulatesDeltas(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	tr.account(1_000_000, 500_000, now)
	rx, tx := tr.account(1_500_000, 700_000, now.Add(15*time.Second))
	if rx != 500_000 || tx != 200_000 {
		t.Fatalf("expected deltas 500000/200000, got %d/%d", rx, tx)
	}
}

func TestRebootCorrectionCountsFromZero(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	tr.account(1_000_000, 500_000, now)
	// Host reboots: counters drop below the persisted baseline.
	rx, tx := tr.account(30_000, 10_000, now.Add(time.Minute))
	if rx != 30_000 || tx != 10_000 {
		t.Fatalf("reboot should count traffic since boot, expected 30000/10000, got %d/%d", rx, tx)
	}
	// Continue accumulating normally after the reboot.
	rx, tx = tr.account(40_000, 15_000, now.Add(2*time.Minute))
	if rx != 40_000 || tx != 15_000 {
		t.Fatalf("post-reboot accumulation wrong, got %d/%d", rx, tx)
	}
}

func TestCycleRolloverResetsCounters(t *testing.T) {
	tr := newTestTracker(t)
	aug := time.Date(2026, 8, 31, 23, 59, 0, 0, time.Local)
	tr.account(1_000_000, 500_000, aug)
	tr.account(2_000_000, 800_000, aug.Add(30*time.Second))
	rx, tx := tr.account(2_100_000, 830_000, time.Date(2026, 9, 1, 0, 0, 30, 0, time.Local))
	if rx != 100_000 || tx != 30_000 {
		t.Fatalf("cycle rollover should count only the new-cycle delta, got %d/%d", rx, tx)
	}
}

func TestResetDayShiftsCycleBoundary(t *testing.T) {
	dir := t.TempDir()
	tr := newTrafficTracker(filepath.Join(dir, "state.json"), 15)
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

	tr1 := newTrafficTracker(statePath, 1)
	tr1.account(1_000_000, 500_000, now)
	tr1.account(2_000_000, 900_000, now.Add(time.Minute))
	if _, err := os.Stat(filepath.Join(dir, "traffic-state.json")); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	// Simulate agent restart: new tracker loads the same state.
	tr2 := newTrafficTracker(statePath, 1)
	rx, tx := tr2.account(2_200_000, 1_000_000, now.Add(2*time.Minute))
	if rx != 1_200_000 || tx != 500_000 {
		t.Fatalf("restart lost accounting: expected 1200000/500000, got %d/%d", rx, tx)
	}
}
