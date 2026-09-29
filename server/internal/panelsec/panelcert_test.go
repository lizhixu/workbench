package panelsec

import (
	"encoding/json"
	"strings"
	"testing"

	"watchman/server/internal/settings"
)

func newTestIssuer(t *testing.T) *settings.Store {
	t.Helper()
	st, err := settings.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func rawStr(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// Validation happens before any network call or hub use: bad modes and
// non-public IPs must be rejected synchronously.
func TestStartIssueValidation(t *testing.T) {
	st := newTestIssuer(t)
	issuer := NewPanelCertIssuer(st, nil, nil) // hub nil on purpose

	if _, err := issuer.StartIssue("bogus", ""); err == nil || !strings.Contains(err.Error(), "未知") {
		t.Errorf("unknown mode: got %v, want 未知签发模式", err)
	}
	if _, err := issuer.StartIssue("domain", ""); err == nil || !strings.Contains(err.Error(), "绑定") {
		t.Errorf("domain mode without bound domain: got %v, want 尚未绑定面板域名", err)
	}

	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": rawStr("panel.example.com"),
	}); err != nil {
		t.Fatal(err)
	}
	// Bound domain passes validation; nil hub is the next failure.
	if _, err := issuer.StartIssue("domain", ""); err == nil || !strings.Contains(err.Error(), "证书中心不可用") {
		t.Errorf("domain mode with bound domain: got %v, want 证书中心不可用", err)
	}

	for _, ip := range []string{"not-an-ip", "192.168.1.1", "10.0.0.5", "127.0.0.1", "::1", "224.0.0.1"} {
		if _, err := issuer.StartIssue("ip", ip); err == nil {
			t.Errorf("expected error for IP %q", ip)
		}
	}
	// Public IP passes validation; nil hub is the next failure.
	if _, err := issuer.StartIssue("ip", "8.8.8.8"); err == nil || !strings.Contains(err.Error(), "证书中心不可用") {
		t.Errorf("public IP: got %v, want 证书中心不可用", err)
	}
}
