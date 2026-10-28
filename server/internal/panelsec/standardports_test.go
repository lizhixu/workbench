package panelsec

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"watchman/server/internal/settings"
)

func TestDesiredStandardPorts(t *testing.T) {
	cases := []struct {
		name string
		sec  settings.PanelSecurityConfig
		want []int
	}{
		{"no domain", settings.PanelSecurityConfig{}, nil},
		{"domain only", settings.PanelSecurityConfig{PanelDomain: "panel.example.com"}, []int{80}},
		{"domain with ssl", settings.PanelSecurityConfig{PanelDomain: "panel.example.com", SSLEnabled: true}, []int{80, 443}},
		{"ssl without domain", settings.PanelSecurityConfig{SSLEnabled: true}, nil},
		{"blank domain", settings.PanelSecurityConfig{PanelDomain: "   ", SSLEnabled: true}, nil},
	}
	for _, tc := range cases {
		if got := desiredStandardPorts(tc.sec); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: desiredStandardPorts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStandardPortStatus(t *testing.T) {
	st, err := settings.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("settings store: %v", err)
	}
	p := NewStandardPorts(st, http.NotFoundHandler(), nil, ":18789", nil)
	if got := p.Status(); len(got) != 0 {
		t.Fatalf("status without a bound domain = %v, want empty", got)
	}

	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": json.RawMessage(`"panel.example.com"`),
	}); err != nil {
		t.Fatalf("bind domain: %v", err)
	}
	got := p.Status()
	if len(got) != 1 || got[0].Port != 80 || !got[0].Desired || got[0].Listening {
		t.Fatalf("status after binding = %+v, want :80 desired and not listening", got)
	}

	// A primary listener already on :80 covers the port; it must be reported
	// as listening (and must not be double-bound).
	p80 := NewStandardPorts(st, http.NotFoundHandler(), nil, ":80", nil)
	if got := p80.Status(); len(got) != 1 || !got[0].Listening {
		t.Fatalf("status with primary listener on :80 = %+v, want listening", got)
	}
}
