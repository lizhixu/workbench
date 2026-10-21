package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestCancelDeploymentEngine(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, store, nil, slog.Default())

	app := &Application{
		ID:         "app_cancel_test",
		Name:       "TestCancel",
		HostID:     "host_1",
		SourceType: "image",
		Image:      "nginx:alpine",
	}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}

	// 1. When nothing is running, CancelDeployment should return false
	if engine.CancelDeployment(app.ID, "admin") {
		t.Errorf("expected CancelDeployment to return false when nothing is running")
	}

	// 2. Register an active deployment manually to simulate in-flight execution
	dep := &Deployment{
		ID:        "dep_123",
		AppID:     app.ID,
		Status:    DeployBuilding,
		StartedAt: time.Now(),
		StartedBy: "user1",
	}
	if err := store.AddDeployment(dep); err != nil {
		t.Fatalf("AddDeployment: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := engine.registerRunning(app.ID, &activeDeployment{
		depID:  dep.ID,
		cancel: cancel,
	}); err != nil {
		t.Fatalf("registerRunning: %v", err)
	}

	if !engine.IsRunning(app.ID) {
		t.Errorf("expected engine.IsRunning to be true")
	}

	// 3. Cancel the in-flight deployment
	cancelled := engine.CancelDeployment(app.ID, "operator_bob")
	if !cancelled {
		t.Errorf("expected CancelDeployment to return true")
	}

	// Context should be cancelled
	if ctx.Err() == nil {
		t.Errorf("expected deployment context to be cancelled")
	}

	// Engine slot should be freed
	if engine.IsRunning(app.ID) {
		t.Errorf("expected engine.IsRunning to be false after cancel")
	}

	// Deployment record should be updated to DeployCancelled with CancelledBy stamped
	updated, ok := store.GetDeployment(app.ID, dep.ID)
	if !ok {
		t.Fatalf("deployment record missing")
	}
	if updated.Status != DeployCancelled {
		t.Errorf("expected status %q, got %q", DeployCancelled, updated.Status)
	}
	if updated.CancelledBy != "operator_bob" {
		t.Errorf("expected CancelledBy %q, got %q", "operator_bob", updated.CancelledBy)
	}
}

func TestCancelDeploymentHTTPHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, store, nil, slog.Default())
	handlers := NewAppHandlers(nil, store, engine)

	r := gin.New()
	rg := r.Group("")
	handlers.RegisterAppRoutes(rg, rg, rg)

	// App not found -> 404
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/apps/nonexistent/deployments/cancel", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown app, got %d", w.Code)
	}

	app := &Application{
		ID:         "app_http_test",
		Name:       "HTTPTest",
		HostID:     "host_1",
		SourceType: "image",
	}
	_ = store.PutApp(app)

	// App exists, but no active deployment -> 200 with cancelled: false
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/apps/"+app.ID+"/deployments/cancel", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp struct {
		OK        bool `json:"ok"`
		Cancelled bool `json:"cancelled"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json parse error: %v", err)
	}
	if !resp.OK || resp.Cancelled {
		t.Errorf("expected ok: true, cancelled: false when idle; got %+v", resp)
	}

	// Register an active deployment
	dep := &Deployment{
		ID:        "dep_http_1",
		AppID:     app.ID,
		Status:    DeployDeploying,
		StartedAt: time.Now(),
	}
	_ = store.AddDeployment(dep)
	_, cancel := context.WithCancel(context.Background())
	_ = engine.registerRunning(app.ID, &activeDeployment{depID: dep.ID, cancel: cancel})

	// Cancel active deployment -> 200 with cancelled: true
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/apps/"+app.ID+"/deployments/cancel", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	resp = struct {
		OK        bool `json:"ok"`
		Cancelled bool `json:"cancelled"`
	}{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || !resp.Cancelled {
		t.Errorf("expected ok: true, cancelled: true; got %+v", resp)
	}

	// Verify deployment state in store
	savedDep, _ := store.GetDeployment(app.ID, dep.ID)
	if savedDep.Status != DeployCancelled {
		t.Errorf("expected deployment status cancelled, got %s", savedDep.Status)
	}
}

func TestForceRedeploy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, store, nil, slog.Default())
	handlers := NewAppHandlers(nil, store, engine)

	r := gin.New()
	rg := r.Group("")
	handlers.RegisterAppRoutes(rg, rg, rg)

	app := &Application{
		ID:         "app_redeploy",
		Name:       "RedeployApp",
		HostID:     "host_1",
		SourceType: "image",
	}
	_ = store.PutApp(app)

	// Simulate in-flight deployment
	dep1 := &Deployment{
		ID:        "dep_old",
		AppID:     app.ID,
		Status:    DeployBuilding,
		StartedAt: time.Now(),
	}
	_ = store.AddDeployment(dep1)
	ctx1, cancel1 := context.WithCancel(context.Background())
	_ = engine.registerRunning(app.ID, &activeDeployment{depID: dep1.ID, cancel: cancel1})

	// Deploy without force while running -> 409 Conflict
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/apps/"+app.ID+"/deploy", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict without force, got %d", w.Code)
	}

	// Deploy with force=true -> cancels old deployment and starts new one
	w = httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"force": true})
	req, _ = http.NewRequest(http.MethodPost, "/apps/"+app.ID+"/deploy", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with force=true, got %d: %s", w.Code, w.Body.String())
	}

		// Old deployment context must be cancelled
		if ctx1.Err() == nil {
			t.Errorf("expected old deployment context to be cancelled")
		}
		oldDep, _ := store.GetDeployment(app.ID, dep1.ID)
		if oldDep.Status != DeployCancelled {
			t.Errorf("expected old deployment to be cancelled, got %s", oldDep.Status)
		}

		// Wait for the async second deployment to complete (fails fast due to offline agent)
		// before TempDir is unlinked, preventing Windows file lock cleanup errors.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if !engine.IsRunning(app.ID) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
