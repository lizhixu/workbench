// traffic.go implements monthly (billing-cycle) traffic accounting.
//
// Kernel NIC counters are cumulative since boot (or since the interface
// appeared). This tracker folds consecutive per-interface counter deltas
// into per-cycle totals that survive agent restarts. A host reboot is
// detected via a change in boot time; in that case the current counter
// values are exactly the traffic since boot. An interface that disappears
// (docker network removed, VPN down, NIC unplugged, …) only drops its
// baseline — its past traffic was already counted, and the surviving
// interfaces' totals are never mistaken for a delta (that confusion was
// the old phantom-add bug: any single counter decrease made the whole
// cumulative total count as one sample's delta).
//
// Loopback interfaces are excluded from accounting: they carry only
// host-internal traffic (e.g. a local reverse proxy to 127.0.0.1) that
// no ISP bills. An optional interface allowlist further restricts
// accounting, e.g. to the external NIC on docker-heavy hosts where one
// packet is counted on veth + bridge + physical NIC.
//
// The billing cycle resets monthly on a configurable day-of-month (1..28).
// State is persisted as JSON next to the agent state file.
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ifaceCounters is one interface's cumulative kernel counters.
type ifaceCounters struct {
	Rx uint64 `json:"rx"`
	Tx uint64 `json:"tx"`
}

// trafficState is the persisted monthly traffic accounting state.
type trafficState struct {
	CycleKey string `json:"cycle_key"` // billing cycle identifier, e.g. "2026-08"
	MonthRx  uint64 `json:"month_rx"`  // bytes received this cycle
	MonthTx  uint64 `json:"month_tx"`  // bytes transmitted this cycle
	// ResetDay is the server-pushed billing reset day (1..28); 0 means
	// unset, in which case the constructor's flag value applies.
	ResetDay int `json:"reset_day"`
	// BootTime is the host boot time (unix seconds) at the last sample; a
	// change means the host rebooted and all counters restarted.
	BootTime int64 `json:"boot_time"`
	// Ifaces holds the per-interface baselines from the last sample.
	Ifaces map[string]ifaceCounters `json:"ifaces"`
}

// trafficTracker accumulates monthly traffic with restart-safe correction.
type trafficTracker struct {
	path     string // state file location ("" disables persistence)
	resetDay int    // billing cycle reset day-of-month (1..28)
	allow    map[string]bool // interface allowlist; nil/empty = all non-loopback
	st       trafficState
}

func newTrafficTracker(stateFile string, resetDay int, allowIfaces []string) *trafficTracker {
	t := &trafficTracker{resetDay: clampResetDay(resetDay)}
	if len(allowIfaces) > 0 {
		t.allow = make(map[string]bool, len(allowIfaces))
		for _, n := range allowIfaces {
			if n = strings.TrimSpace(n); n != "" {
				t.allow[n] = true
			}
		}
	}
	if stateFile != "" {
		t.path = filepath.Join(filepath.Dir(stateFile), "traffic-state.json")
		t.load()
		// A reset day pushed by the server (persisted) wins over the flag:
		// it is fresher than whatever the agent was started with.
		if t.st.ResetDay >= 1 && t.st.ResetDay <= 28 {
			t.resetDay = t.st.ResetDay
		}
	}
	return t
}

func clampResetDay(d int) int {
	if d < 1 || d > 28 {
		return 1
	}
	return d
}

// setResetDay applies a server-pushed reset day (e.g. from RegisterResponse)
// and persists it so agent restarts keep the panel-configured value.
func (t *trafficTracker) setResetDay(day int) {
	if day < 1 || day > 28 || day == t.resetDay {
		return
	}
	t.resetDay = day
	t.st.ResetDay = day
	t.save()
}

// ifaceAllowed reports whether an interface participates in accounting:
// loopback never does; an allowlist, when configured, restricts to its members.
func (t *trafficTracker) ifaceAllowed(name string) bool {
	if isLoopbackIface(name) {
		return false
	}
	if len(t.allow) == 0 {
		return true
	}
	return t.allow[name]
}

// isLoopbackIface matches "lo" (Linux/macOS) and Windows'
// "Loopback Pseudo-Interface 1".
func isLoopbackIface(name string) bool {
	if name == "lo" {
		return true
	}
	return strings.Contains(strings.ToLower(name), "loopback")
}

func (t *trafficTracker) load() {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return // first run or unreadable — start fresh
	}
	_ = json.Unmarshal(data, &t.st)
}

func (t *trafficTracker) save() {
	if t.path == "" {
		return
	}
	data, err := json.MarshalIndent(t.st, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(t.path), 0o700)
	_ = os.WriteFile(t.path, data, 0o600)
}

// cycleKey returns the billing cycle identifier for the given time, honoring
// the reset day: e.g. with resetDay=15, Aug 20 2026 belongs to cycle
// "2026-08" (started Jul 15) and Aug 10 belongs to "2026-07".
func (t *trafficTracker) cycleKey(now time.Time) string {
	y, m := now.Year(), now.Month()
	if t.resetDay > 1 && now.Day() < t.resetDay {
		m--
		if m < 1 {
			m = 12
			y--
		}
	}
	return fmt.Sprintf("%04d-%02d", y, int(m))
}

// account folds one sampling of per-interface cumulative counters into the
// monthly totals and returns (monthRx, monthTx) for the current cycle.
// bootTime is the host boot time (unix seconds); 0 means unknown, in which
// case reboot detection is skipped and per-interface logic still guards
// against phantom deltas. Callers must serialize access (the metrics
// Manager already holds a lock while sampling).
func (t *trafficTracker) account(samples map[string]ifaceCounters, bootTime int64, now time.Time) (uint64, uint64) {
	key := t.cycleKey(now)
	var dRx, dTx uint64

	if t.st.Ifaces == nil {
		t.st.Ifaces = make(map[string]ifaceCounters, len(samples))
	}

	// First sample ever (no persisted state): t.st.Ifaces is empty and
	// BootTime is 0, so every interface below is treated as new and
	// baselined without counting boot-to-install traffic into the cycle.
	rebooted := bootTime > 0 && t.st.BootTime > 0 && bootTime != t.st.BootTime
	if rebooted {
		// Host rebooted: every counter restarted, so the current values
		// are exactly the traffic since boot.
		for _, s := range samples {
			dRx += s.Rx
			dTx += s.Tx
		}
	} else {
		for name, s := range samples {
			prev, ok := t.st.Ifaces[name]
			if !ok {
				continue // new interface: baseline it, don't count pre-existing traffic
			}
			if s.Rx >= prev.Rx {
				dRx += s.Rx - prev.Rx
			} else {
				dRx += s.Rx // this interface's counter reset (driver reload, …)
			}
			if s.Tx >= prev.Tx {
				dTx += s.Tx - prev.Tx
			} else {
				dTx += s.Tx
			}
		}
	}

	// Refresh baselines for the sampled interfaces; drop the ones that
	// vanished (docker network removed, VPN down, …). Their past traffic
	// was already counted in earlier deltas — the survivors' totals must
	// never be mistaken for a delta.
	for name, s := range samples {
		t.st.Ifaces[name] = s
	}
	for name := range t.st.Ifaces {
		if _, ok := samples[name]; !ok {
			delete(t.st.Ifaces, name)
		}
	}

	if t.st.CycleKey == "" {
		t.st.CycleKey = key
		t.st.MonthRx, t.st.MonthTx = 0, 0
	} else if key != t.st.CycleKey {
		// New billing cycle: start counting from this sample's delta.
		t.st.CycleKey = key
		t.st.MonthRx = dRx
		t.st.MonthTx = dTx
	} else {
		t.st.MonthRx += dRx
		t.st.MonthTx += dTx
	}
	if bootTime > 0 {
		t.st.BootTime = bootTime
	}
	t.save()
	return t.st.MonthRx, t.st.MonthTx
}
