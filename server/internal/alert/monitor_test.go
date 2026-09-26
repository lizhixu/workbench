package alert

import (
	"log/slog"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

type mockProvider struct {
	hosts []HostInfo
}

func (m *mockProvider) ListHosts() []HostInfo {
	return m.hosts
}

func (m *mockProvider) ClearReconnectReason(hostID string) {
	for i := range m.hosts {
		if m.hosts[i].ID == hostID {
			m.hosts[i].ReconnectReason = ""
		}
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	return s
}

// TestMonitorColdStartNoFalseAlarms verifies that when the control server
// starts up, existing online hosts do NOT trigger false "host online" alarms.
func TestMonitorColdStartNoFalseAlarms(t *testing.T) {
	store := newTestStore(t)
	prov := &mockProvider{
		hosts: []HostInfo{
			{ID: "h1", Hostname: "host-alpha", Status: "online"},
			{ID: "h2", Hostname: "host-beta", Status: "online"},
		},
	}

	mon := NewMonitor(store, prov, nil, nil, slog.Default())
	// During startup grace (default 60s), evaluate should seed state without firing.
	mon.evaluate()

	events := store.ListEvents(100)
	if len(events) != 0 {
		t.Fatalf("expected 0 events on cold start, got %d: %+v", len(events), events)
	}

	// Transition to post-grace state.
	mon.SetBootGraceForTest(0)
	mon.evaluate()

	events = store.ListEvents(100)
	if len(events) != 0 {
		t.Fatalf("expected 0 events in steady state after grace, got %d: %+v", len(events), events)
	}
}

// TestMonitorRealTransitionOfflineOnline tests that a real state transition
// from offline -> online DOES fire a single online notification, and steady
// state does not repeat it.
func TestMonitorRealTransitionOfflineOnline(t *testing.T) {
	store := newTestStore(t)
	prov := &mockProvider{
		hosts: []HostInfo{
			{ID: "h1", Hostname: "host-alpha", Status: "online"},
		},
	}

	mon := NewMonitor(store, prov, nil, nil, slog.Default())
	mon.SetBootGraceForTest(0) // skip grace for this transition test
	// First tick establishes baseline.
	mon.evaluate()

	if len(store.ListEvents(100)) != 0 {
		t.Fatal("unexpected event on initial baseline")
	}

	// 1. Host goes offline.
	prov.hosts[0].Status = "offline"
	mon.evaluate()

	events := store.ListEvents(100)
	if len(events) != 1 {
		t.Fatalf("expected 1 offline event, got %d", len(events))
	}
	if events[0].RuleType() != RuleOffline && events[0].RuleID != "builtin-offline" {
		t.Fatalf("expected offline alert, got %s", events[0].RuleName)
	}

	// Steady-state offline should NOT fire another offline event.
	mon.evaluate()
	if len(store.ListEvents(100)) != 1 {
		t.Fatalf("expected still 1 event in steady-state offline, got %d", len(store.ListEvents(100)))
	}

	// 2. Host reconnects (offline -> online transition).
	prov.hosts[0].Status = "online"
	mon.evaluate()

	events = store.ListEvents(100)
	// Should now have 2 events: the offline alert (marked resolved) and the new online notification.
	if len(events) != 2 {
		t.Fatalf("expected 2 events after reconnect, got %d", len(events))
	}

	// Check that previous offline alert is resolved.
	for _, e := range events {
		if e.RuleID == "builtin-offline" && !e.Resolved {
			t.Errorf("expected offline alert to be resolved after reconnect")
		}
		if e.RuleID == "builtin-online" {
			if e.Hostname != "host-alpha" {
				t.Errorf("online event hostname = %s, want host-alpha", e.Hostname)
			}
		}
	}

	// 3. Steady-state online: next evaluation must NOT fire duplicate online event.
	mon.evaluate()
	events = store.ListEvents(100)
	if len(events) != 2 {
		t.Fatalf("expected still 2 events in steady-state online, got %d", len(events))
	}
}

func (e *Event) RuleType() RuleType {
	return RuleType(e.RuleID)
}

// TestMonitorNewHostDiscovered verifies that when a brand new host is enrolled
// while the server is running, it fires an online notification once.
func TestMonitorNewHostDiscovered(t *testing.T) {
	store := newTestStore(t)
	prov := &mockProvider{
		hosts: []HostInfo{
			{ID: "h1", Hostname: "host-alpha", Status: "online"},
		},
	}

	mon := NewMonitor(store, prov, nil, nil, slog.Default())
	mon.SetBootGraceForTest(0)
	mon.evaluate()

	// New host enrolled!
	prov.hosts = append(prov.hosts, HostInfo{
		ID:       "h2",
		Hostname: "host-new",
		Status:   "online",
	})
	mon.evaluate()

	events := store.ListEvents(100)
	if len(events) != 1 {
		t.Fatalf("expected 1 online notification for new host, got %d", len(events))
	}
	if events[0].Hostname != "host-new" {
		t.Errorf("event hostname = %s, want host-new", events[0].Hostname)
	}

	// Next tick does not re-fire.
	mon.evaluate()
	if len(store.ListEvents(100)) != 1 {
		t.Fatalf("expected 1 event after steady state, got %d", len(store.ListEvents(100)))
	}
}

// TestStoreDedupHistoricalOnlineEvents verifies that duplicate historical online
// events from server restarts are cleaned up to a single latest event per host.
func TestStoreDedupHistoricalOnlineEvents(t *testing.T) {
	store := newTestStore(t)
	t1 := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 5, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	store.mu.Lock()
	store.events = []*Event{
		{ID: "e1", RuleID: "builtin-online", HostID: "h1", Hostname: "serv", FiredAt: t1},
		{ID: "e2", RuleID: "builtin-offline", HostID: "h2", Hostname: "remote", FiredAt: t1},
		{ID: "e3", RuleID: "builtin-online", HostID: "h1", Hostname: "serv", FiredAt: t2},
		{ID: "e4", RuleID: "builtin-online", HostID: "h1", Hostname: "serv", FiredAt: t3},
	}
	store.dedupHistoricalOnlineEventsLocked()
	remaining := store.events
	store.mu.Unlock()

	// Should keep: e2 (offline alert, kept), and e4 (latest online event for h1).
	if len(remaining) != 2 {
		t.Fatalf("expected 2 events after dedup, got %d: %+v", len(remaining), remaining)
	}
	if remaining[0].ID != "e2" {
		t.Errorf("event[0] = %s, want e2", remaining[0].ID)
	}
	if remaining[1].ID != "e4" {
		t.Errorf("event[1] = %s, want e4 (latest)", remaining[1].ID)
	}
}

// TestMonitorMaintenanceSuppression verifies that when an agent is in maintenance
// or reconnects after upgrade/maintenance, offline/online notifications are suppressed.
func TestMonitorMaintenanceSuppression(t *testing.T) {
	store := newTestStore(t)
	prov := &mockProvider{
		hosts: []HostInfo{
			{ID: "h1", Hostname: "host-alpha", Status: "online"},
		},
	}

	mon := NewMonitor(store, prov, nil, nil, slog.Default())
	mon.SetBootGraceForTest(0)
	mon.evaluate() // steady state

	// 1. Host enters scheduled maintenance (e.g. upgrade)
	mon.SetHostMaintenance("h1", 120*time.Second, "upgrade")

	// Host goes offline during maintenance
	prov.hosts[0].Status = "offline"
	mon.evaluate()

	events := store.ListEvents(100)
	if len(events) != 0 {
		t.Fatalf("expected 0 events while offline in maintenance, got %d", len(events))
	}

	// 2. Host reconnects carrying ReconnectReason="upgrade"
	prov.hosts[0].Status = "online"
	prov.hosts[0].ReconnectReason = "upgrade"
	mon.evaluate()

	events = store.ListEvents(100)
	if len(events) != 0 {
		t.Fatalf("expected 0 events when reconnecting after upgrade, got %d", len(events))
	}

	// 3. The reason is one-shot: the same host going offline again must alert
	// normally once maintenance has ended.
	mon.ClearHostMaintenance("h1")
	prov.hosts[0].ReconnectReason = ""
	prov.hosts[0].Status = "offline"
	mon.evaluate()

	events = store.ListEvents(100)
	if len(events) != 1 {
		t.Fatalf("expected offline alert after maintenance ended, got %d events", len(events))
	}
}

// TestMonitorMaintenanceExpiry verifies a maintenance window stops suppressing
// once it elapses, so a real outage is never permanently silenced by a stale window.
func TestMonitorMaintenanceExpiry(t *testing.T) {
	store := newTestStore(t)
	prov := &mockProvider{
		hosts: []HostInfo{{ID: "h1", Hostname: "host-beta", Status: "online"}},
	}

	mon := NewMonitor(store, prov, nil, nil, slog.Default())
	mon.SetBootGraceForTest(0)
	mon.evaluate() // baseline

	mon.SetHostMaintenance("h1", 50*time.Millisecond, "agent_upgrade")
	prov.hosts[0].Status = "offline"
	mon.evaluate()
	if got := len(store.ListEvents(100)); got != 0 {
		t.Fatalf("expected 0 events inside the maintenance window, got %d", got)
	}

	time.Sleep(80 * time.Millisecond) // let the window lapse
	mon.evaluate()
	if got := len(store.ListEvents(100)); got != 1 {
		t.Fatalf("expected offline alert once maintenance expired, got %d events", got)
	}
		if mon.InMaintenance("h1") {
			t.Error("InMaintenance should report false after the window expired")
		}
	}

func TestMonitorHostSpecificTrafficAlert(t *testing.T) {
	tmp := t.TempDir()
	store, _ := NewStore(tmp)

	prov := &mockProvider{
		hosts: []HostInfo{
			{
				ID:       "h1",
				Hostname: "no-quota-host",
				Status:   "online",
				Metrics: &agentpb.MetricsSample{
					MonthRx: 90 * (1 << 30),
					MonthTx: 90 * (1 << 30),
				},
			},
			{
				ID:             "h2",
				Hostname:       "has-quota-host",
				Status:         "online",
				TrafficLimitGB: 100,
				Metrics: &agentpb.MetricsSample{
					MonthRx: 45 * (1 << 30),
					MonthTx: 45 * (1 << 30),
				},
			},
			{
				ID:              "h3",
				Hostname:        "out-only-host",
				Status:          "online",
				TrafficLimitGB:  100,
				TrafficCalcType: "out",
				Metrics: &agentpb.MetricsSample{
					MonthRx: 60 * (1 << 30),
					MonthTx: 50 * (1 << 30),
				},
			},
		},
	}

	mon := NewMonitor(store, prov, nil, nil, slog.Default())
	mon.SetBootGraceForTest(0)
	mon.evaluate()

	events := store.ListEvents(100)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 traffic event (for h2), got %d", len(events))
	}
	if events[0].HostID != "h2" {
		t.Errorf("expected event for h2, got %s", events[0].HostID)
	}
}

func TestMonitorHostExpiryReminder(t *testing.T) {
	tmp := t.TempDir()
	store, _ := NewStore(tmp)

	in15 := time.Now().AddDate(0, 0, 15).Format("2006/01/02")
	in60 := time.Now().AddDate(0, 0, 60).Format("2006/01/02")

	prov := &mockProvider{
		hosts: []HostInfo{
			{ID: "h1", Hostname: "expiring-soon", Status: "online", ExpiresAt: in15},
			{ID: "h2", Hostname: "expiring-later", Status: "online", ExpiresAt: in60},
			{ID: "h3", Hostname: "no-expiry", Status: "online"},
		},
	}

	mon := NewMonitor(store, prov, nil, nil, slog.Default())
	mon.SetBootGraceForTest(0)
	mon.evaluate()

	// 只有 30 天内到期的 h1 触发提醒，未到期与未配置的主机不触发
	events := store.ListEvents(100)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 expiry reminder (for h1), got %d", len(events))
	}
	if events[0].HostID != "h1" {
		t.Errorf("expected reminder for h1, got %s", events[0].HostID)
	}

	// 到期前 30 天内的重复评估不重复提醒
	mon.evaluate()
	if events := store.ListEvents(100); len(events) != 1 {
		t.Fatalf("expected no duplicate reminder, got %d events", len(events))
	}

	// 续费（到期时间推后到 30 天以外）后提醒自动解除，且不产生新事件
	prov.hosts[0].ExpiresAt = in60
	mon.evaluate()
	if events := store.ListEvents(100); len(events) != 1 {
		t.Fatalf("expected no new events after renewal, got %d", len(events))
	}
}
