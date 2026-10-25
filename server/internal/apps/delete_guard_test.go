package apps

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func deleteAppRequest(h *AppHandlers, id string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/apps/"+id, nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.deleteApp(c)
	return w
}

func newDeleteGuardHandler(t *testing.T) (*AppHandlers, *Store, *Engine) {
	t.Helper()
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, store, nil, slog.Default())
	if err := store.PutApp(&Application{ID: "app_del", Name: "Del", HostID: "h1", SourceType: "image", Image: "nginx:alpine"}); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	return NewAppHandlers(nil, store, engine), store, engine
}

// Deleting under a running deployment used to be the one write path with
// no in-flight guard: purge could remove the old container and volumes
// while the pipeline still swapped a -next container into place.
func TestDeleteAppRejectsRunningDeployment(t *testing.T) {
	h, store, engine := newDeleteGuardHandler(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.registerRunning("app_del", &activeDeployment{depID: "dep_run", cancel: cancel})
	w := deleteAppRequest(h, "app_del")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "部署或迁移任务在执行") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
	if _, ok := store.GetApp("app_del"); !ok {
		t.Errorf("app must survive a rejected delete")
	}
}

func TestDeleteAppRejectsVolumeBusy(t *testing.T) {
	h, store, engine := newDeleteGuardHandler(t)
	if !engine.acquireVolumeSlot("app_del") {
		t.Fatalf("acquireVolumeSlot failed")
	}
	defer engine.releaseVolumeSlot("app_del")
	w := deleteAppRequest(h, "app_del")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if _, ok := store.GetApp("app_del"); !ok {
		t.Errorf("app must survive a rejected delete")
	}
}

func TestDeleteAppIdleSucceeds(t *testing.T) {
	h, store, _ := newDeleteGuardHandler(t)
	w := deleteAppRequest(h, "app_del")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if _, ok := store.GetApp("app_del"); ok {
		t.Errorf("idle app must be deleted")
	}
}
