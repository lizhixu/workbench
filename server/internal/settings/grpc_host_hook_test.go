package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// SetOnAgentGRPCHostChange fires after a committed system-scope write
// touches agent.grpc_host (set, change or clear), outside the store
// lock — even a re-save of the same value, which the panel uses as a
// deliberate "push again". Unrelated keys must not fire, and the hook
// must be able to re-enter the store without deadlocking.
func TestSetOnAgentGRPCHostChange(t *testing.T) {
	s := newTestStore(t)
	calls := 0
	s.SetOnAgentGRPCHostChange(func() {
		// Re-entering the store proves the hook runs after SetMany has
		// released its lock.
		_ = s.AgentGRPCHost()
		calls++
	})

	must := func(vals map[string]json.RawMessage) {
		t.Helper()
		if err := s.SetMany(ScopeSystem, "", vals); err != nil {
			t.Fatalf("SetMany(%v): %v", vals, err)
		}
	}

	// Setting the address fires.
	must(map[string]json.RawMessage{"agent.grpc_host": raw(t, "35.212.206.45:9090")})
	if got := s.AgentGRPCHost(); got != "35.212.206.45:9090" {
		t.Fatalf("AgentGRPCHost = %q", got)
	}
	// Re-saving the same value fires again (deliberate re-push).
	must(map[string]json.RawMessage{"agent.grpc_host": raw(t, "35.212.206.45:9090")})
	// Unrelated keys must not fire.
	must(map[string]json.RawMessage{"server.timezone": raw(t, "Asia/Shanghai")})
	// Clearing fires too (the consumer decides it is a no-op).
	must(map[string]json.RawMessage{"agent.grpc_host": raw(t, "")})
	if calls != 3 {
		t.Fatalf("hook fired %d times, want 3", calls)
	}
	if got := s.AgentGRPCHost(); got != "" {
		t.Fatalf("AgentGRPCHost after clear = %q", got)
	}
}

// The retired agent.redirect_to key is no longer in the registry: an old
// client still writing it gets a clear error instead of silently storing
// a setting nothing reads.
func TestRetiredAgentRedirectToRejected(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"agent.redirect_to": raw(t, "35.212.206.45:9090"),
	}); err == nil {
		t.Fatal("retired agent.redirect_to key accepted by the registry")
	}
}

// A leftover agent.redirect_to is folded into agent.grpc_host once at
// load; grpc_host wins when both are set, and an empty redirect is just
// dropped.
func TestMigrateRetiredRedirectTo(t *testing.T) {
	writeSettings := func(t *testing.T, dir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("write settings.json: %v", err)
		}
	}

	// Redirect only: carried over to grpc_host.
	dir := t.TempDir()
	writeSettings(t, dir, `{"system":{"agent.redirect_to":"35.212.206.45:9090"}}`)
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.AgentGRPCHost(); got != "35.212.206.45:9090" {
		t.Fatalf("AgentGRPCHost after migration = %q", got)
	}
	// Reloading must not re-apply or resurrect the retired key.
	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	if got := s2.AgentGRPCHost(); got != "35.212.206.45:9090" {
		t.Fatalf("AgentGRPCHost after reload = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	if _, ok := doc.System["agent.redirect_to"]; ok {
		t.Fatal("agent.redirect_to survived the migration")
	}

	// Both set: grpc_host wins, redirect dropped.
	dir = t.TempDir()
	writeSettings(t, dir, `{"system":{"agent.grpc_host":"1.2.3.4:9090","agent.redirect_to":"35.212.206.45:9090"}}`)
	s, err = NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.AgentGRPCHost(); got != "1.2.3.4:9090" {
		t.Fatalf("AgentGRPCHost with both set = %q", got)
	}

	// Empty redirect: dropped, grpc_host stays empty.
	dir = t.TempDir()
	writeSettings(t, dir, `{"system":{"agent.redirect_to":""}}`)
	s, err = NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.AgentGRPCHost(); got != "" {
		t.Fatalf("AgentGRPCHost with empty redirect = %q", got)
	}
}
