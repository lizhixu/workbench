// traffic.go implements monthly (billing-cycle) traffic accounting.
//
// Kernel NIC counters are cumulative since boot (or since the interface
// appeared). This tracker folds consecutive per-interface counter deltas
// into per-cycle totals that survive agent restarts.
//
// The cardinal rule is that only traffic observed as a delta between two
// consecutive samples is ever counted. A cumulative counter value is
// never, by itself, added to the monthly total:
//
//   - A host reboot is detected via a change in boot time. The counters
//     restarted, so the current values are traffic since boot — but how
//     much of that flowed before the agent started sampling is unknowable,
//     so the reboot sample is rebaselined and counts 0. Counting the full
//     counters here was the phantom-add bug: a false reboot detection
//     (e.g. a stale boot time in the persisted state) added the host's
//     entire uptime traffic to the cycle in one sample.
//   - A single interface's counter decrease (driver reload, interface
//     recreated, …) likewise rebaselines that interface and counts 0 for
//     the sample; the full current value is never added.
//   - An interface that disappears (docker network removed, VPN down, NIC
//     unplugged, …) only drops its baseline — its past traffic was already
//     counted, and the surviving interfaces' totals are never mistaken
//     for a delta (that confusion was the old phantom-add bug: any single
//     counter decrease made the whole cumulative total count as one
//     sample's delta).
//
// Loopback interfaces are excluded from accounting: they carry only
// host-internal traffic (e.g. a local reverse proxy to 127.0.0.1) that
// no ISP bills. By default only the interface(s) carrying the default
// route (the external NIC) are counted, so one packet is not counted
// several times on veth + bridge + physical NIC; an explicit interface
// allowlist (-traffic-ifaces) overrides that selection, and when the
// routing table cannot be read accounting falls back to all non-loopback
// interfaces.
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
	path     string          // state file location ("" disables persistence)
	resetDay int             // billing cycle reset day-of-month (1..28)
	allow    map[string]bool // interface allowlist; nil/empty = default-route interfaces
	// defaultIfaces is the set of interfaces carrying the default route
	// (external NICs), resolved once at startup when no allowlist is set;
	// nil/empty means the routing table could not be read and accounting
	// falls back to all non-loopback interfaces.
	defaultIfaces map[string]bool
	st            trafficState
}

// defaultRouteIfacesFunc resolves the interfaces carrying the default
// route. It is a variable so tests can stub the routing table.
var defaultRouteIfacesFunc = defaultRouteIfaces

func newTrafficTracker(stateFile string, resetDay int, allowIfaces []string) *trafficTracker {
	t := &trafficTracker{resetDay: clampResetDay(resetDay)}
	if len(allowIfaces) > 0 {
		t.allow = make(map[string]bool, len(allowIfaces))
		for _, n := range allowIfaces {
			if n = strings.TrimSpace(n); n != "" {
				t.allow[n] = true
			}
		}
	} else {
		// No explicit allowlist: restrict to the external NIC(s) (the
		// interfaces carrying the default route) so virtual interfaces
		// (tailscale, docker bridge/veth, …) do not double-count the
		// same packet. Nil/empty falls back to all non-loopback.
		t.defaultIfaces = defaultRouteIfacesFunc()
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

// setCorrection overrides the current cycle's monthly totals with the
// panel-supplied values (e.g. to repair a corrupted total) and persists
// them. Later samples only add deltas observed after the correction, so
// the corrected total keeps accumulating correctly. The per-interface
// baselines are left untouched: the next sample adds only the small delta
// since the last sample, which is real traffic that flowed after the
// previous sample.
func (t *trafficTracker) setCorrection(rx, tx uint64) {
	t.st.MonthRx = rx
	t.st.MonthTx = tx
	t.save()
}

// ifaceAllowed reports whether an interface participates in accounting:
// loopback never does; an allowlist, when configured, restricts to its
// members; otherwise only the interfaces carrying the default route are
// counted (falling back to all non-loopback when the routing table could
// not be read).
func (t *trafficTracker) ifaceAllowed(name string) bool {
	if isLoopbackIface(name) {
		return false
	}
	if len(t.allow) > 0 {
		return t.allow[name]
	}
	if len(t.defaultIfaces) > 0 {
		return t.defaultIfaces[name]
	}
	return true
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
	//
	// Only deltas between two consecutive observed samples are ever
	// counted. A reboot or a counter decrease rebaselines and counts 0
	// for the sample: the full current counter value is never added, so
	// no code path can inject the host's cumulative uptime traffic into
	// the cycle (the phantom-add bug).
	rebooted := bootTime > 0 && t.st.BootTime > 0 && bootTime != t.st.BootTime
	if !rebooted {
		for name, s := range samples {
			prev, ok := t.st.Ifaces[name]
			if !ok {
				continue // new interface: baseline it, don't count pre-existing traffic
			}
			if s.Rx >= prev.Rx {
				dRx += s.Rx - prev.Rx
			}
			// else: this interface's counter decreased (driver reload,
			// interface recreated, …): rebaseline it and count 0 for
			// this sample.
			if s.Tx >= prev.Tx {
				dTx += s.Tx - prev.Tx
			}
			// else: same as Rx, rebaseline, count 0.
		}
	}
	// else: host rebooted. The counters restarted, so the current values
	// are traffic since boot — but the agent cannot know how much of that
	// flowed before it started sampling, and a false reboot detection
	// (stale persisted boot time) would add the host's entire uptime
	// traffic to the cycle in one sample. So the reboot sample is
	// rebaselined and counts 0; only deltas observed from here on are
	// counted. The baselines are refreshed below.

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
