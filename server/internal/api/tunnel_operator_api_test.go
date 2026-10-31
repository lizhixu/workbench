package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"watchman/server/internal/auth"
	"watchman/server/internal/settings"

	"github.com/gin-gonic/gin"
)

func getJSON(t *testing.T, r *gin.Engine, token, path, host string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if host != "" {
		req.Host = host
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestTunnelClientInfoDisabled(t *testing.T) {
	r, authStore, _ := newTestRouter(t)
	tok := tokenFor(t, authStore, "ci_viewer", auth.RoleViewer)

	code, body := getJSON(t, r, tok, "/api/v1/tunnels/client-info", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	if body["enabled"] != false {
		t.Fatalf("disabled data plane must report enabled=false: %v", body)
	}
	if _, ok := body["endpoint"]; ok {
		t.Fatalf("disabled data plane must not advertise an endpoint: %v", body)
	}
	cli, _ := body["cli"].(map[string]any)
	if !strings.Contains(cli["windows_amd64"].(string), "watchmanctl-windows-amd64.exe") {
		t.Fatalf("missing windows CLI download link: %v", cli)
	}
	if !strings.HasPrefix(cli["linux_amd64"].(string), "https://github.com/") {
		t.Fatalf("CLI links must point at GitHub releases: %v", cli)
	}
}

func TestTunnelClientInfoEnabledDerivation(t *testing.T) {
	r, authStore, settingsStore := newTestRouterOp(t, ":19443")
	tok := tokenFor(t, authStore, "ci_op", auth.RoleOperator)

	// Host header fallback.
	code, body := getJSON(t, r, tok, "/api/v1/tunnels/client-info", "panel.example.com:18789")
	if code != http.StatusOK || body["enabled"] != true {
		t.Fatalf("enabled data plane: %d %v", code, body)
	}
	if body["endpoint"] != "panel.example.com:19443" {
		t.Fatalf("endpoint must derive host from request Host: %v", body["endpoint"])
	}

	// The bound panel domain wins over the request Host.
	if err := settingsStore.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": json.RawMessage(`"watchman.example.com"`),
	}); err != nil {
		t.Fatalf("set panel domain: %v", err)
	}
	// The bound panel domain wins over the request Host. Access via loopback
	// so the strict-domain guard does not reject the probe first.
	_, body = getJSON(t, r, tok, "/api/v1/tunnels/client-info", "127.0.0.1:18789")
	if body["endpoint"] != "watchman.example.com:19443" {
		t.Fatalf("bound domain hostname must win: %v", body["endpoint"])
	}
}

// TestTunnelClientInfoIPv6Host: bracketed IPv6 Host headers must come out
// bracketed exactly once ([[::1]]:9443 is the double-bracket bug).
func TestTunnelClientInfoIPv6Host(t *testing.T) {
	r, authStore, _ := newTestRouterOp(t, ":19443")
	tok := tokenFor(t, authStore, "ci_v6", auth.RoleOperator)

	for _, host := range []string{"[::1]:18789", "[::1]"} {
		_, body := getJSON(t, r, tok, "/api/v1/tunnels/client-info", host)
		if body["endpoint"] != "[::1]:19443" {
			t.Fatalf("Host %q must yield [::1]:19443, got %v", host, body["endpoint"])
		}
	}
}

func TestMyOperatorTunnelsEmptyAndCloseUnknown(t *testing.T) {
	r, authStore, _ := newTestRouter(t)
	tok := tokenFor(t, authStore, "mine_op", auth.RoleOperator)

	code, body := getJSON(t, r, tok, "/api/v1/tunnels/mine", "")
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	tunnels, _ := body["tunnels"].([]any)
	if len(tunnels) != 0 {
		t.Fatalf("fresh user must have no tunnels: %v", tunnels)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/tunnels/mine/nope", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("closing an unknown leg must 404, got %d", w.Code)
	}
}
