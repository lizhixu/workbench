package rpc

import (
	"log/slog"
	"testing"
)

// effectiveProbeURL falls back to the built-in default and honours overrides.
func TestEffectiveProbeURL(t *testing.T) {
	if got := effectiveProbeURL(""); got != DefaultProbeURL {
		t.Fatalf("empty stored: got %q, want default %q", got, DefaultProbeURL)
	}
	if got := effectiveProbeURL("  https://example.com/ping  "); got != "https://example.com/ping" {
		t.Fatalf("override: got %q", got)
	}
}

// normalizeProbeURL accepts empty + absolute http(s), rejects the rest.
func TestNormalizeProbeURL(t *testing.T) {
	for _, ok := range []string{"", "https://www.zstaticcdn.com/", "http://192.0.2.1:8080/health"} {
		if _, err := normalizeProbeURL(ok); err != nil {
			t.Fatalf("normalizeProbeURL(%q): unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"zstaticcdn.com", "ftp://example.com", "https://", "javascript:alert(1)"} {
		if _, err := normalizeProbeURL(bad); err == nil {
			t.Fatalf("normalizeProbeURL(%q): want error", bad)
		}
	}
}

// SetAgentBilling persists a valid probe URL and rejects invalid ones
// without touching the stored value.
func TestSetAgentBillingProbeURL(t *testing.T) {
	r := NewRegistry("", slog.Default())
	r.agents["h1"] = &Agent{ID: "h1", Hostname: "a"}

	cfg := HostBillingConfig{ProbeEnabled: true, ProbeURL: "https://example.com/ping"}
	if err := r.SetAgentBilling("h1", cfg); err != nil {
		t.Fatalf("set valid probe url: %v", err)
	}
	if got := r.agents["h1"].ProbeURL; got != "https://example.com/ping" {
		t.Fatalf("stored probe url = %q", got)
	}
	if !r.agents["h1"].ProbeEnabled {
		t.Fatal("probe_enabled should be stored")
	}

	if err := r.SetAgentBilling("h1", HostBillingConfig{ProbeURL: "not-a-url"}); err == nil {
		t.Fatal("invalid probe url should be rejected")
	}
	if got := r.agents["h1"].ProbeURL; got != "https://example.com/ping" {
		t.Fatalf("rejected save clobbered probe url: %q", got)
	}
}

// probePushURL gates on the per-host switch: off (or no agent) pushes an
// empty URL, which the agent reads as "probing disabled".
func TestProbePushURLGating(t *testing.T) {
	if got := probePushURL(nil); got != "" {
		t.Fatalf("nil agent: got %q, want empty", got)
	}
	off := &Agent{ProbeEnabled: false, ProbeURL: "https://example.com/ping"}
	if got := probePushURL(off); got != "" {
		t.Fatalf("disabled: got %q, want empty", got)
	}
	onDefault := &Agent{ProbeEnabled: true}
	if got := probePushURL(onDefault); got != DefaultProbeURL {
		t.Fatalf("enabled without override: got %q, want default", got)
	}
	onCustom := &Agent{ProbeEnabled: true, ProbeURL: "https://example.com/ping"}
	if got := probePushURL(onCustom); got != "https://example.com/ping" {
		t.Fatalf("enabled with override: got %q", got)
	}
}
