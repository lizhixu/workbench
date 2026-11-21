package settings

import (
	"encoding/json"
	"testing"
)

// SetOnAgentRedirectChange fires after a committed system-scope write
// touches agent.redirect_to (set, change or clear), outside the store
// lock — even a re-save of the same value, which the panel uses as a
// deliberate "push again". Unrelated keys must not fire, and the hook
// must be able to re-enter the store without deadlocking.
func TestSetOnAgentRedirectChange(t *testing.T) {
	s := newTestStore(t)
	calls := 0
	s.SetOnAgentRedirectChange(func() {
		// Re-entering the store proves the hook runs after SetMany has
		// released its lock.
		_ = s.AgentRedirectTo()
		calls++
	})

	must := func(vals map[string]json.RawMessage) {
		t.Helper()
		if err := s.SetMany(ScopeSystem, "", vals); err != nil {
			t.Fatalf("SetMany(%v): %v", vals, err)
		}
	}

	// Setting the target fires.
	must(map[string]json.RawMessage{"agent.redirect_to": raw(t, "35.212.206.45:9090")})
	if got := s.AgentRedirectTo(); got != "35.212.206.45:9090" {
		t.Fatalf("AgentRedirectTo = %q", got)
	}
	// Re-saving the same value fires again (deliberate re-push).
	must(map[string]json.RawMessage{"agent.redirect_to": raw(t, "35.212.206.45:9090")})
	// Unrelated keys must not fire.
	must(map[string]json.RawMessage{"agent.grpc_host": raw(t, "1.2.3.4")})
	// Clearing fires too (the consumer decides it is a no-op).
	must(map[string]json.RawMessage{"agent.redirect_to": raw(t, "")})
	if calls != 3 {
		t.Fatalf("hook fired %d times, want 3", calls)
	}
	if got := s.AgentRedirectTo(); got != "" {
		t.Fatalf("AgentRedirectTo after clear = %q", got)
	}
}

// The redirect target reuses the grpc_host validation: host[:port], no
// scheme or path, empty allowed.
func TestAgentRedirectToValidation(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"agent.redirect_to": raw(t, "https://example.com/x"),
	}); err == nil {
		t.Fatalf("scheme/path target accepted")
	}
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"agent.redirect_to": raw(t, "35.212.206.45:9090"),
	}); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
}
