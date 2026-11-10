package settings

import (
	"encoding/json"
	"testing"
)

// SetOnPanelDomainChange fires after a committed system-scope write actually
// changes security.panel_domain (bind, change or unbind), outside the store
// lock, with the previous and next values. Same-value writes — including
// case-only differences — and unrelated keys must not fire, and the hook
// must be able to re-enter the store without deadlocking.
func TestSetOnPanelDomainChange(t *testing.T) {
	s := newTestStore(t)
	var calls [][2]string
	s.SetOnPanelDomainChange(func(oldDomain, newDomain string) {
		// Re-entering the store proves the hook runs after SetMany has
		// released its lock.
		_ = s.PanelSecurity()
		calls = append(calls, [2]string{oldDomain, newDomain})
	})

	must := func(vals map[string]json.RawMessage) {
		t.Helper()
		if err := s.SetMany(ScopeSystem, "", vals); err != nil {
			t.Fatalf("SetMany(%v): %v", vals, err)
		}
	}

	// Binding fires with an empty previous value.
	must(map[string]json.RawMessage{"security.panel_domain": raw(t, "panel.example.com")})
	// Same value (case-only difference) must not fire — but the write still
	// persists the new case, so the next change reports it as the previous
	// value exactly as stored.
	must(map[string]json.RawMessage{"security.panel_domain": raw(t, "PANEL.Example.COM")})
	// Unrelated keys must not fire.
	must(map[string]json.RawMessage{"security.panel_ssl_enabled": raw(t, false)})
	// Changing fires with both values.
	must(map[string]json.RawMessage{"security.panel_domain": raw(t, "next.example.com")})
	// Unbinding fires too (the consumer decides it is a no-op).
	must(map[string]json.RawMessage{"security.panel_domain": raw(t, "")})

	want := [][2]string{
		{"", "panel.example.com"},
		{"PANEL.Example.COM", "next.example.com"},
		{"next.example.com", ""},
	}
	if len(calls) != len(want) {
		t.Fatalf("hook fired %d times (%v), want %d", len(calls), calls, len(want))
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call %d = %v, want %v", i, calls[i], w)
		}
	}
}
