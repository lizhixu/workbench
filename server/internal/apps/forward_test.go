package apps

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/rpc"
)

func TestValidateMeshForward(t *testing.T) {
	cases := []struct {
		name      string
		appHost   string
		enabled   bool
		entryHost string
		entry     int
		target    int
		wantErr   string
	}{
		{"disabled passes with empty fields", "h1", false, "", 0, 0, ""},
		{"ok", "h1", true, "h2", 8080, 6379, ""},
		{"entry host required", "h1", true, "", 8080, 6379, "必须选择入口主机"},
		{"entry == app host rejected", "h1", true, "h1", 8080, 6379, "不能与应用部署主机相同"},
		{"entry port 0", "h1", true, "h2", 0, 6379, "入口端口非法"},
		{"entry port overflow", "h1", true, "h2", 65536, 6379, "入口端口非法"},
		{"target port 0", "h1", true, "h2", 8080, 0, "目标端口非法"},
		{"target port overflow", "h1", true, "h2", 8080, 65536, "目标端口非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMeshForward(tc.appHost, tc.enabled, tc.entryHost, tc.entry, tc.target)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestMeshForwardConflict(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	h := NewAppHandlers(nil, store, nil)

	existing := &Application{
		ID: "app_a", Name: "A",
		MeshForwardEnabled: true, MeshForwardHostID: "entry1", MeshForwardEntryPort: 8080,
	}
	if err := store.PutApp(existing); err != nil {
		t.Fatalf("PutApp: %v", err)
	}

	if err := h.meshForwardConflict("app_b", "entry1", 8080); err == nil {
		t.Fatal("same (entry host, entry port) on another app must conflict")
	}
	if err := h.meshForwardConflict("app_a", "entry1", 8080); err != nil {
		t.Fatalf("self update must not conflict with itself: %v", err)
	}
	if err := h.meshForwardConflict("app_b", "entry1", 8081); err != nil {
		t.Fatalf("different port must not conflict: %v", err)
	}
	if err := h.meshForwardConflict("app_b", "entry2", 8080); err != nil {
		t.Fatalf("different entry host must not conflict: %v", err)
	}

	// A disabled app never blocks a pair even if legacy fields linger.
	disabled := &Application{ID: "app_c", Name: "C", MeshForwardHostID: "entry1", MeshForwardEntryPort: 9090}
	if err := store.PutApp(disabled); err != nil {
		t.Fatalf("PutApp disabled: %v", err)
	}
	if err := h.meshForwardConflict("app_b", "entry1", 9090); err != nil {
		t.Fatalf("disabled forwarder must not conflict: %v", err)
	}
}

// TestUpdateAppReValidates guards B2: updates must enforce the same mesh
// rules as creates even though the request type allows partial fields.
func TestUpdateAppReValidates(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	h := NewAppHandlers(rpc.NewRegistry("", slog.Default()), store, nil)

	app := &Application{ID: "app_u", Name: "U", HostID: "host_app", SourceType: "image", Image: "nginx"}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}

	cases := []struct {
		name     string
		body     map[string]any
		wantCode int
		wantErr  string
	}{
		{"entry == app host", map[string]any{"mesh_forward_enabled": true, "mesh_forward_host_id": "host_app", "mesh_forward_entry_port": 8080, "mesh_forward_target_port": 6379}, http.StatusBadRequest, "不能与应用部署主机相同"},
		{"entry port out of range", map[string]any{"mesh_forward_enabled": true, "mesh_forward_host_id": "entry1", "mesh_forward_entry_port": 99999, "mesh_forward_target_port": 6379}, http.StatusBadRequest, "入口端口非法"},
		{"enabled without entry host", map[string]any{"mesh_forward_enabled": true, "mesh_forward_entry_port": 8080, "mesh_forward_target_port": 6379}, http.StatusBadRequest, "必须选择入口主机"},
		{"invalid port mapping", map[string]any{"ports": []map[string]any{{"host": 0, "container": 80}}}, http.StatusBadRequest, "端口映射非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c := newReqCtx("app_u", tc.body)
			h.updateApp(c)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantCode, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantErr) {
				t.Fatalf("body = %s, want error containing %q", w.Body.String(), tc.wantErr)
			}
			// Rejected updates must not mutate the stored app.
			got, _ := store.GetApp("app_u")
			if got.MeshForwardEnabled {
				t.Fatal("rejected update enabled mesh forwarding on stored app")
			}
		})
	}
}

func newReqCtx(id string, body map[string]any) (*httptest.ResponseRecorder, *gin.Context) {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/apps/"+id, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: id}}
	return w, c
}

func TestResolveForwardUpstream(t *testing.T) {
	// 1. Both app and entry have mesh IPs -> prefer overlay
	target, err := resolveForwardUpstream(6379, "100.64.0.5", "100.64.0.1", "1.2.3.4", "192.168.1.10")
	if err != nil || target != "100.64.0.5:6379" {
		t.Fatalf("expected 100.64.0.5:6379, got %q (err: %v)", target, err)
	}

	// 2. Only one has mesh IP -> fallback to public IP
	target, err = resolveForwardUpstream(6379, "100.64.0.5", "", "1.2.3.4", "192.168.1.10")
	if err != nil || target != "1.2.3.4:6379" {
		t.Fatalf("expected 1.2.3.4:6379, got %q (err: %v)", target, err)
	}

	// 3. No mesh, no public IP -> fallback to internal IP
	target, err = resolveForwardUpstream(6379, "", "", "", "192.168.1.10")
	if err != nil || target != "192.168.1.10:6379" {
		t.Fatalf("expected 192.168.1.10:6379, got %q (err: %v)", target, err)
	}

	// 4. No mesh, no public, no internal IP -> error
	_, err = resolveForwardUpstream(6379, "", "", "", "")
	if err == nil {
		t.Fatalf("expected error when no reachable IP is found, got nil")
	}
}
