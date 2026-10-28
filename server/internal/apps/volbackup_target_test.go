package apps

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/snapshots"
)

// The package already has fakeS3TargetStore (volbackup_test.go); these
// helpers build the two-target shape destination-selection tests need.
func newTargetSelectionStore() *fakeS3TargetStore {
	r2 := &snapshots.S3Target{ID: "t-r2", Name: "R2", Bucket: "bkt-r2"}
	minio := &snapshots.S3Target{ID: "t-minio", Name: "MinIO", Bucket: "bkt-minio"}
	return &fakeS3TargetStore{def: r2, targets: map[string]*snapshots.S3Target{"t-r2": r2, "t-minio": minio}}
}

func newTargetSelectionStoreNoDefault() *fakeS3TargetStore {
	s := newTargetSelectionStore()
	s.def = nil
	return s
}

// ---- resolveVolumeBackupTarget ----

func TestResolveVolumeBackupTarget(t *testing.T) {
	engine := &Engine{s3: newTargetSelectionStore()}

	// An explicit target wins over the default.
	if got, err := engine.resolveVolumeBackupTarget("t-minio"); err != nil || got.ID != "t-minio" {
		t.Errorf("explicit target: got (%v, %v), want (t-minio, nil)", got, err)
	}
	// Whitespace-only input counts as omitted, not as an unknown id.
	if got, err := engine.resolveVolumeBackupTarget("  "); err != nil || got.ID != "t-r2" {
		t.Errorf("blank target: got (%v, %v), want (t-r2, nil)", got, err)
	}
	// Omitted input falls back to the default target.
	if got, err := engine.resolveVolumeBackupTarget(""); err != nil || got.ID != "t-r2" {
		t.Errorf("omitted target: got (%v, %v), want (t-r2, nil)", got, err)
	}
	// An unknown id must not silently fall back to the default.
	if _, err := engine.resolveVolumeBackupTarget("t-gone"); !errors.Is(err, errVolumeTargetUnknown) {
		t.Errorf("unknown target: err = %v, want errVolumeTargetUnknown", err)
	}

	// Without a default, an explicit valid target still resolves: choosing
	// a destination must not require a default to exist.
	noDefault := &Engine{s3: newTargetSelectionStoreNoDefault()}
	if got, err := noDefault.resolveVolumeBackupTarget("t-minio"); err != nil || got.ID != "t-minio" {
		t.Errorf("explicit without default: got (%v, %v), want (t-minio, nil)", got, err)
	}
	// Without a default and without a choice, there is nowhere to upload to.
	if _, err := noDefault.resolveVolumeBackupTarget(""); !errors.Is(err, errVolumeTargetNone) {
		t.Errorf("no target at all: err = %v, want errVolumeTargetNone", err)
	}
}

// ---- createVolumeBackup target mapping ----

func newTargetTestHandler(t *testing.T, store *fakeS3TargetStore) *AppHandlers {
	t.Helper()
	s, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, s, nil, slog.Default())
	engine.s3 = store
	if err := s.PutApp(&Application{ID: "app_vol", Name: "Vol", HostID: "h1", SourceType: "image", Image: "nginx:alpine", Volumes: []string{"/opt/data:/data"}}); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	return NewAppHandlers(nil, s, engine)
}

func postVolumeBackup(h *AppHandlers, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/apps/app_vol/volume-backups", bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: "app_vol"}}
	h.createVolumeBackup(c)
	return w
}

func TestCreateVolumeBackupTargetMapping(t *testing.T) {
	// Unknown target -> 400 telling the user to pick again. This fires
	// before the slot/hub checks, so no agent is needed.
	h := newTargetTestHandler(t, newTargetSelectionStore())
	w := postVolumeBackup(h, `{"target_id":"t-gone"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "所选备份位置不存在") {
		t.Errorf("unknown target: code = %d, body = %s, want 400 with re-pick message", w.Code, w.Body.String())
	}

	// No target and no default -> 400 guiding to configure one.
	h = newTargetTestHandler(t, newTargetSelectionStoreNoDefault())
	w = postVolumeBackup(h, `{}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "请先选择备份位置") {
		t.Errorf("no target no default: code = %d, body = %s, want 400 with configure message", w.Code, w.Body.String())
	}

	// Explicit valid target with no default configured must pass
	// resolution: it reaches the offline-host check (503), not a 400.
	h = newTargetTestHandler(t, newTargetSelectionStoreNoDefault())
	w = postVolumeBackup(h, `{"target_id":"t-minio"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("explicit without default: code = %d, body = %s, want 503 (resolution passed, host offline)", w.Code, w.Body.String())
	}

	// Omitted target with a default falls back the same way.
	h = newTargetTestHandler(t, newTargetSelectionStore())
	w = postVolumeBackup(h, `{}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("omitted with default: code = %d, body = %s, want 503 (resolution passed, host offline)", w.Code, w.Body.String())
	}
}
