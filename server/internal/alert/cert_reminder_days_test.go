package alert

import "testing"

// certReminderDaysOrDefault drives checkCertExpiry's threshold; the provider
// is injected by the server entrypoint from the settings store.
func TestCertReminderDaysProvider(t *testing.T) {
	mon := NewMonitor(newTestStore(t), &mockProvider{}, nil, nil, nil)
	if got := mon.certReminderDaysOrDefault(); got != 30 {
		t.Fatalf("default = %d, want 30", got)
	}

	mon.SetCertExpiryReminderDays(func() int { return 60 })
	if got := mon.certReminderDaysOrDefault(); got != 60 {
		t.Fatalf("custom = %d, want 60", got)
	}

	mon.SetCertExpiryReminderDays(func() int { return 0 })
	if got := mon.certReminderDaysOrDefault(); got != 30 {
		t.Fatalf("zero provider = %d, want 30", got)
	}
	mon.SetCertExpiryReminderDays(func() int { return 365 })
	if got := mon.certReminderDaysOrDefault(); got != 30 {
		t.Fatalf("oversized provider = %d, want 30", got)
	}

	mon.SetCertExpiryReminderDays(nil)
	if got := mon.certReminderDaysOrDefault(); got != 30 {
		t.Fatalf("nil provider = %d, want 30", got)
	}
}

// 配置的提醒提前天数生效：40 天后到期的手动证书在默认 30 天下不提醒，
// 把提醒天数调到 60 后应提醒。
func TestCheckCertExpiryRespectsConfiguredDays(t *testing.T) {
	mon, store, _ := newCertTestMonitor(t, []CertInfo{
		certExpiringIn(t, "crt_manual", 40, false),
	})
	mon.evaluate()
	if n := len(store.ListEvents(100)); n != 0 {
		t.Fatalf("default 30d: expected no reminder for cert expiring in 40d, got %d events", n)
	}

	mon.SetCertExpiryReminderDays(func() int { return 60 })
	mon.evaluate()
	events := store.ListEvents(100)
	if len(events) != 1 {
		t.Fatalf("configured 60d: expected 1 reminder for cert expiring in 40d, got %d events", len(events))
	}
	if events[0].CertID != "crt_manual" || events[0].RuleID != "builtin-cert-expiry" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}
