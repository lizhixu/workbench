package apps

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// newUpdateTestHandler builds a handler over a store seeded with one app.
func newUpdateTestHandler(t *testing.T, app *Application) (*AppHandlers, *Store) {
	t.Helper()
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	return NewAppHandlers(rpc.NewRegistry(t.TempDir(), nil), store, nil), store
}

func putApp(h *AppHandlers, id string, body map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/apps/"+id, strings.NewReader(string(raw)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.updateApp(c)
	return w
}

// TestUpdateAppPartialDoesNotClobber: the update body must behave as a patch.
// Saving only env vars used to wipe auto_deploy, the Git credential, the
// health check URL and the mesh relay coordinates, because createAppReq's
// non-pointer fields were assigned unconditionally.
func TestUpdateAppPartialDoesNotClobber(t *testing.T) {
	app := &Application{
		ID: "app_patch", Name: "keep-me", HostID: "h1", SourceType: "git",
		RepoURL: "https://github.com/o/r", Branch: "main", AuthVaultID: "vault1",
		AutoDeploy: true, HealthcheckURL: "http://127.0.0.1:8080/healthz",
		MeshForwardEnabled: true, MeshForwardHostID: "h2",
		MeshForwardEntryPort: 18080, MeshForwardTargetPort: 8080,
		EnvVars: map[string]string{"OLD": "1"},
		Ports:   []PortMapping{{Host: 8080, Container: 80}},
		Volumes: []string{"/data:/data"},
	}
	h, store := newUpdateTestHandler(t, app)

	// Only the env vars arrive — the relay fields are untouched, so the
	// offline entry host must not be contacted and cannot fail the save.
	w := putApp(h, "app_patch", map[string]any{"env_vars": map[string]string{"NEW": "2"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got, _ := store.GetApp("app_patch")
	if got.AutoDeploy != true {
		t.Error("auto_deploy must survive a partial update")
	}
	if got.AuthVaultID != "vault1" {
		t.Errorf("auth_vault_id must survive, got %q", got.AuthVaultID)
	}
	if got.HealthcheckURL != "http://127.0.0.1:8080/healthz" {
		t.Errorf("healthcheck_url must survive, got %q", got.HealthcheckURL)
	}
	if !got.MeshForwardEnabled || got.MeshForwardHostID != "h2" ||
		got.MeshForwardEntryPort != 18080 || got.MeshForwardTargetPort != 8080 {
		t.Errorf("mesh forward must survive, got %+v", got)
	}
	if got.EnvVars["NEW"] != "2" || len(got.EnvVars) != 1 {
		t.Errorf("env vars must be fully replaced, got %v", got.EnvVars)
	}
	if got.Name != "keep-me" || got.Branch != "main" {
		t.Errorf("unrelated fields must not change: %+v", got)
	}
}

// TestUpdateAppExplicitClear: an explicit false / "" / {} still clears,
// so the patch semantics do not make fields un-clearable.
func TestUpdateAppExplicitClear(t *testing.T) {
	app := &Application{
		ID: "app_clear", Name: "n", HostID: "h1", SourceType: "git",
		AutoDeploy: true, AuthVaultID: "vault1",
		HealthcheckURL: "http://127.0.0.1:8080/healthz",
		EnvVars:        map[string]string{"A": "1"},
	}
	h, store := newUpdateTestHandler(t, app)

	w := putApp(h, "app_clear", map[string]any{
		"auto_deploy":     false,
		"auth_vault_id":   "",
		"healthcheck_url": "",
		"env_vars":        map[string]string{},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got, _ := store.GetApp("app_clear")
	if got.AutoDeploy {
		t.Error("explicit false must disable auto_deploy")
	}
	if got.AuthVaultID != "" {
		t.Errorf("explicit empty string must clear the credential, got %q", got.AuthVaultID)
	}
	if got.HealthcheckURL != "" {
		t.Errorf("explicit empty string must clear the health check, got %q", got.HealthcheckURL)
	}
	if len(got.EnvVars) != 0 {
		t.Errorf("explicit empty map must clear env vars, got %v", got.EnvVars)
	}
}

// TestUpdateAppExplicitClearBuildFields: the same explicit-clear contract
// for the five build fields whose non-empty guards silently ignored "".
// Empty values are stored as-is; the deploy pipeline re-applies its
// defaults (Dockerfile, "." context, 900s timeout) for them.
func TestUpdateAppExplicitClearBuildFields(t *testing.T) {
	app := &Application{
		ID: "app_clear_build", Name: "n", HostID: "h1", SourceType: "git",
		Dockerfile: "Dockerfile", BuildContext: ".", BuildTimeout: 600,
		ComposeContent: "services: {}", BuildType: "dockerfile",
	}
	h, store := newUpdateTestHandler(t, app)

	w := putApp(h, "app_clear_build", map[string]any{
		"dockerfile":        "",
		"build_context":     "",
		"build_timeout_sec": 0,
		"compose_content":   "",
		"build_type":        "",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got, _ := store.GetApp("app_clear_build")
	if got.Dockerfile != "" {
		t.Errorf("explicit empty dockerfile must clear, got %q", got.Dockerfile)
	}
	if got.BuildContext != "" {
		t.Errorf("explicit empty build_context must clear, got %q", got.BuildContext)
	}
	if got.BuildTimeout != 0 {
		t.Errorf("explicit 0 build_timeout_sec must clear, got %d", got.BuildTimeout)
	}
	if got.ComposeContent != "" {
		t.Errorf("explicit empty compose_content must clear, got %q", got.ComposeContent)
	}
	if got.BuildType != "" {
		t.Errorf("explicit empty build_type must clear, got %q", got.BuildType)
	}
}

// TestUpdateAppMeshTouchedRequiresEntryHost: touching a relay field validates
// the EFFECTIVE config — disabling needs no entry host, enabling does.
func TestUpdateAppMeshTouchedValidates(t *testing.T) {
	app := &Application{
		ID: "app_mesh", Name: "n", HostID: "h1", SourceType: "image", Image: "nginx",
		MeshForwardEnabled: true, MeshForwardHostID: "h2",
		MeshForwardEntryPort: 18080, MeshForwardTargetPort: 8080,
	}
	h, store := newUpdateTestHandler(t, app)

	// Disabling the relay must not require an online entry host.
	w := putApp(h, "app_mesh", map[string]any{"mesh_forward_enabled": false})
	if w.Code != http.StatusOK {
		t.Fatalf("disable: status = %d, body %s", w.Code, w.Body.String())
	}
	got, _ := store.GetApp("app_mesh")
	if got.MeshForwardEnabled || got.MeshForwardHostID != "" || got.MeshForwardEntryPort != 0 {
		t.Errorf("disabling must clear coordinates, got %+v", got)
	}

	// Re-enabling without an entry host is rejected.
	w = putApp(h, "app_mesh", map[string]any{"mesh_forward_enabled": true})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("enable without host: status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "必须选择入口主机") {
		t.Errorf("unexpected error body: %s", w.Body.String())
	}
}

// TestUpdateAppImageWhitelist: only a pre-built image app may retag; template
// and Git/compose images come from their own pipeline.
func TestUpdateAppImageWhitelist(t *testing.T) {
	cases := []struct {
		sourceType string
		wantCode   int
	}{
		{"image", http.StatusOK},
		{"template", http.StatusBadRequest},
		{"git", http.StatusBadRequest},
		{"raw_compose", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.sourceType, func(t *testing.T) {
			app := &Application{
				ID: "app_img", Name: "n", HostID: "h1",
				SourceType: tc.sourceType, Image: "old:1",
			}
			h, store := newUpdateTestHandler(t, app)
			w := putApp(h, "app_img", map[string]any{"image": "new:2"})
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d, body %s", w.Code, tc.wantCode, w.Body.String())
			}
			got, _ := store.GetApp("app_img")
			if tc.wantCode == http.StatusOK && got.Image != "new:2" {
				t.Errorf("image must be updated, got %q", got.Image)
			}
			if tc.wantCode != http.StatusOK && got.Image != "old:1" {
				t.Errorf("rejected update must not change the image, got %q", got.Image)
			}
		})
	}
}

// TestStoreUpdatedAtAdvances: every persisted mutation touches UpdatedAt (the
// list's 最近更新 column depends on it), and a backwards clock must not rewind
// the stamp.
func TestStoreUpdatedAtAdvances(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	created := time.Now().Add(-time.Hour)
	app := &Application{ID: "a1", Name: "n", HostID: "h1", CreatedAt: created}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	got, _ := store.GetApp("a1")
	if !got.UpdatedAt.Equal(created) {
		t.Errorf("PutApp must seed UpdatedAt from CreatedAt, got %v want %v", got.UpdatedAt, created)
	}

	time.Sleep(2 * time.Millisecond)
	if err := store.UpdateApp("a1", func(a *Application) error { a.Name = "n2"; return nil }); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	first, _ := store.GetApp("a1")
	if !first.UpdatedAt.After(created) {
		t.Errorf("UpdateApp must advance UpdatedAt, got %v", first.UpdatedAt)
	}

	// A failing mutation rolls nothing forward.
	if err := store.UpdateApp("a1", func(a *Application) error { return os.ErrPermission }); err == nil {
		t.Fatal("expected error from failing mutation")
	}
	afterFail, _ := store.GetApp("a1")
	if !afterFail.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("failed mutation must not advance UpdatedAt: %v -> %v", first.UpdatedAt, afterFail.UpdatedAt)
	}

	// Clock stepping backwards keeps the previous stamp (monotonic).
	if got := nextUpdatedAt(first.UpdatedAt, first.UpdatedAt.Add(-time.Hour)); !got.Equal(first.UpdatedAt) {
		t.Errorf("backwards clock must not rewind the stamp, got %v", got)
	}
}

// TestLoadAppsBackfillsUpdatedAt: records persisted before the field existed
// must show the newest known activity instead of a blank column.
func TestLoadAppsBackfillsUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	created := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	deployed := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	legacy := []*Application{
		{ID: "a1", Name: "with-deploy", HostID: "h1", CreatedAt: created, LastDeployAt: deployed},
		{ID: "a2", Name: "never-deployed", HostID: "h1", CreatedAt: created},
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, appsFile), b, 0600); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}
	store, err := NewStore(dir, slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	a1, _ := store.GetApp("a1")
	if !a1.UpdatedAt.Equal(deployed) {
		t.Errorf("backfill must use the last deploy time: got %v want %v", a1.UpdatedAt, deployed)
	}
	a2, _ := store.GetApp("a2")
	if !a2.UpdatedAt.Equal(created) {
		t.Errorf("backfill must fall back to CreatedAt: got %v want %v", a2.UpdatedAt, created)
	}
}

// TestCreateAppStartsDeployment: creating an app queues its first deployment
// (trigger=create) and returns the record. With no agent attached the
// pipeline fails asynchronously — the create response must still be a success
// carrying the deployment, because the app record is valid on its own.
//
// The test must not return while the pipeline goroutine can still write to
// the store dir: its status writes race t.TempDir's RemoveAll cleanup
// ("directory not empty"). Waiting for the pipeline's own terminal state is
// NOT the fix here — on a streamless hub the first agent exec blocks for
// its full timeout (docker-run 120s + 30s grace = 150s) before failing, so
// a terminal-state poll would stall every run for minutes.
//
// The race is subtler than "guarded writes are no-ops": cancelling
// immediately after create is not enough either. The pipeline goroutine may
// not have run yet when the test cancels, and under load it can wake up
// AFTER the test returns: its first setDeploymentStatus(Building) is
// terminal-guarded in memory, but Store.UpdateDeployment still rewrites
// app_deployments.jsonl (tmp file + rename) even when the mutate was a
// no-op — recreating the file mid-RemoveAll. The reliable gate is to wait
// until the goroutine has demonstrably passed its first status write and
// is blocked inside the first agent exec: from there no further store
// write can land until the exec timeout, long after cleanup has finished.
// The first exec is the Docker preflight (30s timeout + 30s grace = 60s),
// which runs while the record is still Building — so the gate polls for
// Building, not Deploying (Deploying is only written after the preflight
// passes, which never happens on this streamless hub). So: poll for
// Building, then cancel, then poll for terminal.
func TestCreateAppStartsDeployment(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	engine := NewEngine(reg, store, nil, slog.Default())
	h := NewAppHandlers(reg, store, engine)

	// An online-looking hub is required by createApp's precondition; registering
	// an agent gives us one without a live stream (execs will simply time out,
	// which is exactly the "deploy fails asynchronously" path under test).
	token := reg.IssueEnrollToken()
	hub, resp, err := reg.Register(t.Context(), &agentpb.RegisterRequest{
		EnrollToken: token,
		Os:          "linux",
		Hostname:    "box",
	}, "")
	if err != nil || hub == nil {
		t.Fatalf("Register: %v", err)
	}

	raw, _ := json.Marshal(map[string]any{
		"name": "created", "host_id": resp.GetAgentId(),
		"source_type": "image", "image": "nginx:alpine",
		"ports": []map[string]any{{"host": 18081, "container": 80}},
	})
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/apps", strings.NewReader(string(raw)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.createApp(c)

	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	var body struct {
		Data       Application `json:"data"`
		Deployment *Deployment `json:"deployment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Deployment == nil {
		t.Fatalf("create must return the queued deployment, body %s", w.Body.String())
	}
	if body.Deployment.Trigger != "create" {
		t.Errorf("trigger = %q, want create", body.Deployment.Trigger)
	}
	if body.Deployment.AppID != body.Data.ID {
		t.Errorf("deployment app id = %q, want %q", body.Deployment.AppID, body.Data.ID)
	}
	if got, ok := store.GetDeployment(body.Data.ID, body.Deployment.ID); !ok || got == nil {
		t.Fatal("deployment record must be persisted")
	}
	// Gate on Building before cancelling (see the doc comment above):
	// only once the pipeline goroutine has passed its first status write
	// and is blocked in the first agent exec (the Docker preflight) is it
	// safe to cancel and return — a goroutine that has not run yet would
	// otherwise do its (guarded but still file-rewriting) first status
	// write during TempDir cleanup.
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, ok := store.GetDeployment(body.Data.ID, body.Deployment.ID)
		if !ok || got == nil {
			t.Fatal("deployment record disappeared while waiting for building state")
		}
		if got.Status == DeployBuilding {
			break
		}
		if isTerminalDeploymentStatus(got.Status) {
			t.Fatalf("deployment reached terminal state %q before building, want %q", got.Status, DeployBuilding)
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not reach building state within 10s, last status %q", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Cancel the run before returning: the cancel finalizes the record
	// synchronously, and the goroutine is now blocked in the agent exec
	// (the Docker preflight), so no further store write can land until
	// its 60s timeout — long after TempDir cleanup has finished.
	if !engine.CancelDeployment(body.Data.ID, "test") {
		t.Fatal("expected the deploying deployment to still be running so the test can cancel it")
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		got, ok := store.GetDeployment(body.Data.ID, body.Deployment.ID)
		if !ok || got == nil {
			t.Fatal("deployment record disappeared while waiting for terminal state")
		}
		if isTerminalDeploymentStatus(got.Status) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled deployment did not reach a terminal state within 10s, last status %q", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
