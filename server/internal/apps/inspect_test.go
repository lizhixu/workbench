package apps

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/gitprovider"
	"watchman/server/internal/registry"
)

func init() { gin.SetMode(gin.TestMode) }

// callInspect builds a gin context carrying the given role/username and
// invokes one probe handler with a JSON body.
func callInspect(t *testing.T, h *AppHandlers, handler func(*gin.Context), role, username, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("role", role)
	c.Set("username", username)
	c.Request = httptest.NewRequest(http.MethodPost, "/apps/inspect", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return w
}

func TestInspectHandlersRejectViewer(t *testing.T) {
	called := false
	h := &AppHandlers{
		imageInspector: func(context.Context, string) (*registry.ImageMetadata, error) {
			called = true
			return &registry.ImageMetadata{}, nil
		},
		repoInspector: func(context.Context, string) (*gitprovider.RepoMetadata, error) {
			called = true
			return &gitprovider.RepoMetadata{}, nil
		},
	}
	probeLimiter = newProbeRateLimiter(30, time.Minute)

	// Viewer is denied on both endpoints before the prober ever runs: the
	// repo probe runs with the admin's stored GitHub token, so a viewer
	// must not reach it with a guessable github.com/<owner>/<repo> URL.
	for name, tc := range map[string]struct {
		fn   func(*gin.Context)
		body string
	}{
		"image": {h.inspectImage, `{"image":"nginx:alpine"}`},
		"repo":  {h.inspectRepo, `{"repo_url":"https://github.com/o/r"}`},
	} {
		w := callInspect(t, h, tc.fn, "viewer", "mallory", tc.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s probe as viewer: status %d, want 403", name, w.Code)
		}
		if called {
			t.Errorf("%s probe as viewer reached the prober", name)
		}
	}

	// Operator (allowed to create apps) passes the gate.
	w := callInspect(t, h, h.inspectRepo, "operator", "op", `{"repo_url":"https://github.com/o/r"}`)
	if w.Code != http.StatusOK {
		t.Errorf("repo probe as operator: status %d, want 200", w.Code)
	}
	if !called {
		t.Error("repo probe as operator did not reach the prober")
	}
}

func TestInspectHandlersRateLimit(t *testing.T) {
	probeLimiter = newProbeRateLimiter(3, time.Minute)
	h := &AppHandlers{
		imageInspector: func(context.Context, string) (*registry.ImageMetadata, error) {
			return &registry.ImageMetadata{}, nil
		},
	}
	for i := 0; i < 3; i++ {
		w := callInspect(t, h, h.inspectImage, "operator", "op", `{"image":"nginx:alpine"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("probe %d: status %d, want 200", i+1, w.Code)
		}
	}
	w := callInspect(t, h, h.inspectImage, "operator", "op", `{"image":"nginx:alpine"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("4th probe in window: status %d, want 429", w.Code)
	}
	// A different user has an independent quota.
	w = callInspect(t, h, h.inspectImage, "operator", "other", `{"image":"nginx:alpine"}`)
	if w.Code != http.StatusOK {
		t.Errorf("other user's probe: status %d, want 200", w.Code)
	}
	probeLimiter = newProbeRateLimiter(30, time.Minute)
}

// TestProbeLimiterProductionValue pins the production probe quota: 30 probes
// per user per minute (inspect.go's package-level probeLimiter). The
// mechanism tests above inject smaller limiters, so every test that
// reassigns probeLimiter must restore the production value before it
// returns — this assertion reads the shared variable and fails if a test
// leaks a different limiter or if the production value itself drifts.
func TestProbeLimiterProductionValue(t *testing.T) {
	if probeLimiter.limit != 30 {
		t.Errorf("production probe limit = %d, want 30", probeLimiter.limit)
	}
	if probeLimiter.window != time.Minute {
		t.Errorf("production probe window = %v, want 1m", probeLimiter.window)
	}
}
