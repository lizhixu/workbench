// traffic.go implements monthly (billing-cycle) traffic accounting.
//
// The NIC counters reported by the kernel are cumulative since boot; this
// tracker converts consecutive counter deltas into per-cycle totals that
// survive agent restarts and host reboots (a counter reset is detected when
// the new value is smaller than the previous one — in that case the current
// value is treated as the traffic since the reboot).
//
// The billing cycle resets monthly on a configurable day-of-month (default 1).
// State is persisted as JSON next to the agent state file.
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// trafficState is the persisted monthly traffic accounting state.
type trafficState struct {
	CycleKey   string `json:"cycle_key"`    // billing cycle identifier, e.g. "2026-08"
	MonthRx    uint64 `json:"month_rx"`     // bytes received this cycle
	MonthTx    uint64 `json:"month_tx"`     // bytes transmitted this cycle
	LastTotalRx uint64 `json:"last_total_rx"` // cumulative NIC counter at last sample
	LastTotalTx uint64 `json:"last_total_tx"`
}

// trafficTracker accumulates monthly traffic with restart-safe correction.
type trafficTracker struct {
	path     string // state file location ("" disables persistence)
	resetDay int    // billing cycle reset day-of-month (1..28)
	st       trafficState
}

func newTrafficTracker(stateFile string, resetDay int) *trafficTracker {
	if resetDay < 1 || resetDay > 28 {
		resetDay = 1
	}
	t := &trafficTracker{resetDay: resetDay}
	if stateFile != "" {
		t.path = filepath.Join(filepath.Dir(stateFile), "traffic-state.json")
		t.load()
	}
	return t
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

// account folds a new cumulative counter reading into the monthly totals and
// returns (monthRx, monthTx) for the current cycle. Callers must serialize
// access (the metrics Manager already holds a lock while sampling).
func (t *trafficTracker) account(totalRx, totalTx uint64, now time.Time) (uint64, uint64) {
	key := t.cycleKey(now)

	// First sample ever (no persisted state): baseline the counters without
	// counting boot-to-install traffic into the cycle.
	if t.st.CycleKey == "" {
		t.st.CycleKey = key
		t.st.MonthRx, t.st.MonthTx = 0, 0
		t.st.LastTotalRx, t.st.LastTotalTx = totalRx, totalTx
		t.save()
		return 0, 0
	}

	// Delta since last sample; counter reset (reboot) => use current value.
	dRx, dTx := totalRx, totalTx
	if totalRx >= t.st.LastTotalRx && totalTx >= t.st.LastTotalTx && t.st.LastTotalRx > 0 {
		dRx = totalRx - t.st.LastTotalRx
		dTx = totalTx - t.st.LastTotalTx
	}
	if key != t.st.CycleKey {
		// New billing cycle: start counting from this sample's delta.
		t.st.CycleKey = key
		t.st.MonthRx = dRx
		t.st.MonthTx = dTx
	} else {
		t.st.MonthRx += dRx
		t.st.MonthTx += dTx
	}
	t.st.LastTotalRx = totalRx
	t.st.LastTotalTx = totalTx
	t.save()
	return t.st.MonthRx, t.st.MonthTx
}
