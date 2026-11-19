package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestTracker(t *testing.T) *trafficTracker {
	return newTestTrackerWithRoutes(t, nil)
}

// newTestTrackerWithRoutes builds a tracker with a stubbed routing table
// so interface selection is deterministic and independent of the machine
// the tests run on. routes == nil means no default route is known, so
// accounting falls back to all non-loopback interfaces.
func newTestTrackerWithRoutes(t *testing.T, routes map[string]bool) *trafficTracker {
	t.Helper()
	orig := defaultRouteIfacesFunc
	defaultRouteIfacesFunc = func() map[string]bool { return routes }
	t.Cleanup(func() { defaultRouteIfacesFunc = orig })
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

func TestRebootRebaselinesWithoutCounting(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 8, 22, 10, 0, 0, 0, time.Local)
	tr.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, now)
	// Host reboots: boot time changes and counters restart from small values.
	// The reboot sample must rebaseline and count 0 — the current counter
	// values are traffic since boot, but how much of that flowed before
	// the agent started sampling is unknowable, and counting the full
	// values is the phantom-add bug (a false reboot detection added the
	// host's entire uptime traffic to the cycle at once).
	rx, tx := tr.account(map[string]ifaceCounters{"eth0": {30_000, 10_000}}, testBootB, now.Add(time.Minute))
	if rx != 0 || tx != 0 {
		t.Fatalf("reboot must rebaseline without counting, expected 0/0, got %d/%d", rx, tx)
	}
	// Continue accumulating normally after the reboot: only deltas from
	// the new baseline count.
	rx, tx = tr.account(map[string]ifaceCounters{"eth0": {40_000, 15_000}}, testBootB, now.Add(2*time.Minute))
	if rx != 10_000 || tx != 5_000 {
		t.Fatalf("post-reboot accumulation wrong, got %d/%d", rx, tx)
	}
}

func TestFalseRebootDetectionDoesNotPhantomAdd(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)
	// A host up for weeks: large cumulative counters, small monthly total.
	tr.account(map[string]ifaceCounters{"eth0": {600_000_000_000, 400_000_000_000}}, testBootA, now)
	tr.account(map[string]ifaceCounters{"eth0": {600_010_000_000, 400_005_000_000}}, testBootA, now.Add(time.Minute))
	// A false reboot detection (persisted boot time stale, host did NOT
	// reboot): the month total must not jump by the full cumulative
	// counters — it must stay at the deltas actually observed.
	rx, tx := tr.account(map[string]ifaceCounters{"eth0": {600_020_000_000, 400_010_000_000}}, testBootB, now.Add(2*time.Minute))
	if rx != 10_000_000 || tx != 5_000_000 {
		t.Fatalf("false reboot detection must not phantom-add, expected 10000000/5000000, got %d/%d", rx, tx)
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
	// and the host did NOT reboot: eth0 rebaselines and counts 0 for this
	// sample; only eth1's delta counts. The full current value of a
	// reset interface is never added.
	rx, tx := tr.account(map[string]ifaceCounters{
		"eth0": {30_000, 10_000},
		"eth1": {2_100_000, 820_000},
	}, testBootA, now.Add(time.Minute))
	if rx != 100_000 || tx != 20_000 {
		t.Fatalf("expected 100000/20000, got %d/%d", rx, tx)
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

func TestSetCorrectionOverridesTotalsAndKeepsAccumulating(t *testing.T) {
	tr := newTestTracker(t)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)
	tr.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, now)
	rx, tx := tr.account(map[string]ifaceCounters{"eth0": {2_000_000, 900_000}}, testBootA, now.Add(time.Minute))
	if rx != 1_000_000 || tx != 400_000 {
		t.Fatalf("setup deltas wrong, got %d/%d", rx, tx)
	}

	// Panel-driven correction (e.g. repairing a phantom-add): the totals
	// become exactly the supplied values.
	tr.setCorrection(100_000, 50_000)
	if tr.st.MonthRx != 100_000 || tr.st.MonthTx != 50_000 {
		t.Fatalf("setCorrection must override totals, got %d/%d", tr.st.MonthRx, tr.st.MonthTx)
	}

	// The next sample only adds the delta observed after the correction —
	// it must not re-add the interface's cumulative counters.
	rx, tx = tr.account(map[string]ifaceCounters{"eth0": {2_100_000, 950_000}}, testBootA, now.Add(2*time.Minute))
	if rx != 200_000 || tx != 100_000 {
		t.Fatalf("after correction only new deltas count, got %d/%d", rx, tx)
	}
}

func TestSetCorrectionPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.Local)

	tr1 := newTrafficTracker(statePath, 1, nil)
	tr1.account(map[string]ifaceCounters{"eth0": {1_000_000, 500_000}}, testBootA, now)
	tr1.setCorrection(100_000, 50_000)

	tr2 := newTrafficTracker(statePath, 1, nil)
	rx, tx := tr2.account(map[string]ifaceCounters{"eth0": {1_100_000, 550_000}}, testBootA, now.Add(time.Minute))
	if rx != 200_000 || tx != 100_000 {
		t.Fatalf("correction must survive restart and keep accumulating, got %d/%d", rx, tx)
	}
}

func TestLoopbackExcludedFromAccounting(t *testing.T) {
	// Default route via eth0: only eth0 participates; loopback never does,
	// and virtual interfaces are excluded by default (no double-count).
	tr := newTestTrackerWithRoutes(t, map[string]bool{"eth0": true})
	if tr.ifaceAllowed("lo") {
		t.Fatal("lo must be excluded from traffic accounting")
	}
	if tr.ifaceAllowed("Loopback Pseudo-Interface 1") {
		t.Fatal("Windows loopback must be excluded from traffic accounting")
	}
	if !tr.ifaceAllowed("eth0") {
		t.Fatal("eth0 (default route) must be allowed")
	}
	if tr.ifaceAllowed("docker0") {
		t.Fatal("docker0 must be excluded by default (virtual interface, double-counts)")
	}
	if tr.ifaceAllowed("tailscale0") {
		t.Fatal("tailscale0 must be excluded by default (virtual interface, double-counts)")
	}
	if tr.ifaceAllowed("vethABC") {
		t.Fatal("veth must be excluded by default (virtual interface, double-counts)")
	}
}

func TestNoDefaultRouteFallsBackToAllNonLoopback(t *testing.T) {
	// Routing table unreadable / no default route: fall back to all
	// non-loopback interfaces rather than counting nothing.
	tr := newTestTracker(t) // routes stubbed to nil
	if tr.ifaceAllowed("lo") {
		t.Fatal("lo must be excluded from traffic accounting")
	}
	if !tr.ifaceAllowed("eth0") || !tr.ifaceAllowed("docker0") {
		t.Fatal("fallback must allow all non-loopback interfaces")
	}
}

func TestParseDefaultRouteIfaces(t *testing.T) {
	routes := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n" +
		"tailscale0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"docker0\t00000000\t00000000\t0000\t0\t0\t0\t00000000\t0\t0\t0\n"
	got := parseDefaultRouteIfaces(strings.NewReader(routes))
	// eth0 (up, gateway) and tailscale0 (up, no gateway) carry a default
	// route; docker0's default route is down (flags 0) and is ignored.
	if len(got) != 2 || !got["eth0"] || !got["tailscale0"] {
		t.Fatalf("expected eth0+tailscale0, got %v", got)
	}
	if got := parseDefaultRouteIfaces(strings.NewReader("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n")); len(got) != 0 {
		t.Fatalf("header-only table must yield no default route, got %v", got)
	}
	if got := parseDefaultRouteIfaces(strings.NewReader("")); got != nil {
		t.Fatalf("empty table must yield nil, got %v", got)
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
