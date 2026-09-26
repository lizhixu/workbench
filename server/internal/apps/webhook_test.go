package apps

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newWebhookRouter(t *testing.T) (*gin.Engine, *Store, *Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := newTestStore(t)
	app := &Application{
		ID: "app_hook", Name: "hook", HostID: "h1",
		RepoURL: "https://github.com/example/repo.git",
		Branch:  "main", AutoDeploy: true,
		WebhookToken: "tok123", CreatedAt: time.Now(),
	}
	if err := s.PutApp(app); err != nil {
		t.Fatal(err)
	}
	// Engine with a nil registry: StartDeployment marks running and spawns a
	// goroutine that fails fast (agent offline), which is fine for these
	// tests; the assertions below only check filtering behavior.
	e := NewEngine(nil, s, nil, nil)
	h := NewAppHandlers(nil, s, e)
	r := gin.New()
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group("/api/v1"))
	return r, s, e
}

func TestWebhookBranchFiltering(t *testing.T) {
	r, s, e := newWebhookRouter(t)

	// Unknown token.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/badtoken", strings.NewReader(`{}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("bad token: code=%d", w.Code)
	}

	// GitHub-shaped payload, non-matching branch -> skipped, no deployment.
	githubPush := `{"ref":"refs/heads/feature/x","repository":{"default_branch":"main"}}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/tok123", strings.NewReader(githubPush)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "skipped") {
		t.Fatalf("branch mismatch should be skipped: code=%d body=%s", w.Code, w.Body.String())
	}

	// Matching branch -> queued deployment (engine fails async; record exists).
	githubMain := `{"ref":"refs/heads/main","head_commit":{"id":"abc123"}}`
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/tok123", strings.NewReader(githubMain)))
	if w.Code != http.StatusOK {
		t.Fatalf("matching branch failed: code=%d body=%s", w.Code, w.Body.String())
	}
	// Give the async goroutine a moment, then confirm a deployment record
	// was created.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, total := s.ListDeployments("app_hook", 0, 10)
		if total == 1 && !e.IsRunning("app_hook") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, total := s.ListDeployments("app_hook", 0, 10); total != 1 {
		t.Fatalf("expected 1 deployment record, got %d", total)
	}

	// Gitee form-encoded payload.
	form := url.Values{"payload": []string{`{"ref":"refs/heads/main","ref_name":"main"}`}}
	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/apps/webhook/tok123", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("gitee form payload failed: code=%d body=%s", w.Code, w.Body.String())
	}

	// AutoDeploy disabled -> forbidden.
	_ = s.UpdateApp("app_hook", func(a *Application) error { a.AutoDeploy = false; return nil })
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/tok123", strings.NewReader(githubMain)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("auto_deploy=false should be forbidden: code=%d", w.Code)
	}
}

func TestSameBranchTolerantCompare(t *testing.T) {
	cases := []struct {
		pushed, configured string
		want               bool
	}{
		{"refs/heads/main", "main", true},
		{"main", "refs/heads/main", true},
		{"feature/x", "main", false},
		{"", "main", false},
		{"main", "", true},
	}
	for _, tc := range cases {
		if got := sameBranch(tc.pushed, tc.configured); got != tc.want {
			t.Errorf("sameBranch(%q,%q)=%v want %v", tc.pushed, tc.configured, got, tc.want)
		}
	}
}
