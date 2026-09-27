package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"watchman/server/internal/auth"
)

func TestTunnelRoutesRegisteredAndRoleGated(t *testing.T) {
	r, authStore := newTestRouter(t)

	viewer := tokenFor(t, authStore, "tunnel_viewer", auth.RoleViewer)
	operator := tokenFor(t, authStore, "tunnel_operator", auth.RoleOperator)

	// Viewer cannot list tunnels (forbidden 403).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hosts/h-test/tunnels", nil)
	req.Header.Set("Authorization", "Bearer "+viewer)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for viewer, got %d", w.Code)
	}

	// Operator can access tunnel routes (404 host not found, not 404 route not found).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/hosts/h-test/tunnels", nil)
	req.Header.Set("Authorization", "Bearer "+operator)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 host not found for operator, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp["error"] != "host not found" {
		t.Fatalf("expected 'host not found', got %v", resp["error"])
	}

	// Operator cannot open tunnel on non-existent host.
	body := bytes.NewBufferString(`{"remote_port": 12345, "local_host": "127.0.0.1", "local_port": 80}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/hosts/h-test/tunnels", body)
	req.Header.Set("Authorization", "Bearer "+operator)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 host not found on POST tunnel, got %d", w.Code)
	}
}
