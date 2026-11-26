package apps

import (
	"encoding/json"
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
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, total := s.ListDeployments("app_hook", 0, 10)
		if total == 2 && !e.IsRunning("app_hook") {
			break
		}
		time.Sleep(20 * time.Millisecond)
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

func TestWebhookWatchPaths(t *testing.T) {
	r, s, e := newWebhookRouter(t)
	watched := &Application{
		ID: "app_watch", Name: "watched", HostID: "h1",
		RepoURL: "https://github.com/example/repo.git",
		Branch:  "main", AutoDeploy: true,
		WebhookToken: "tokwatch", WatchPaths: []string{"apps/web"},
		CreatedAt: time.Now(),
	}
	if err := s.PutApp(watched); err != nil {
		t.Fatal(err)
	}
	deployments := func() int {
		_, total := s.ListDeployments("app_watch", 0, 10)
		return total
	}
	waitSettled := func(want int) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if deployments() == want && !e.IsRunning("app_watch") {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/tokwatch", strings.NewReader(body)))
		return w
	}

	// Push touching a watched path deploys.
	w := post(`{"ref":"refs/heads/main","commits":[{"added":["apps/web/src/index.ts"]}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("watched-path push failed: code=%d body=%s", w.Code, w.Body.String())
	}
	waitSettled(1)
	if deployments() != 1 {
		t.Fatalf("watched-path push should deploy, got %d records", deployments())
	}

	// Push touching nothing watched is skipped, with no new record.
	w = post(`{"ref":"refs/heads/main","commits":[{"modified":["README.md"]}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "no watched paths changed") {
		t.Fatalf("untouched push should be skipped: code=%d body=%s", w.Code, w.Body.String())
	}
	if deployments() != 1 {
		t.Fatalf("untouched push must not deploy, got %d records", deployments())
	}

	// Push without a file list deploys (fail-open).
	w = post(`{"ref":"refs/heads/main","head_commit":{"id":"abc123"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("no-list push failed: code=%d body=%s", w.Code, w.Body.String())
	}
	waitSettled(2)
	if deployments() != 2 {
		t.Fatalf("no-list push should deploy (fail-open), got %d records", deployments())
	}
}

func TestCIWebhookImageAndBearerAndTriggers(t *testing.T) {
	r, s, e := newWebhookRouter(t)
	waitSettled := func(id string) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if !e.IsRunning(id) {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	}

	app := &Application{
		ID: "app_ci", Name: "ci-app", HostID: "h1",
		SourceType: "image", Image: "ghcr.io/acme/web:latest",
		AutoDeploy: true, WebhookToken: "tok_ci", CreatedAt: time.Now(),
	}
	if err := s.PutApp(app); err != nil {
		t.Fatal(err)
	}

	// 1. Same-repository tag update via CI webhook updates Image & enqueues CI deploy.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/tok_ci", strings.NewReader(`{"tag":"sha-abc123","actor":"github-actions"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("CI tag webhook failed: code=%d body=%s", w.Code, w.Body.String())
	}
	waitSettled("app_ci")
	updated, ok := s.GetApp("app_ci")
	if !ok || updated.Image != "ghcr.io/acme/web:sha-abc123" {
		t.Fatalf("expected Image updated to sha-abc123, got %+v", updated)
	}
	deps, _ := s.ListDeployments("app_ci", 0, 10)
	if len(deps) != 1 || deps[0].Trigger != TriggerCI {
		t.Fatalf("expected 1 CI deployment, got %+v", deps)
	}

	// 2. Cross-repository image replacement is rejected with 400.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/tok_ci", strings.NewReader(`{"image":"evil.io/other/app:v1"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-repo hijack must fail 400, got %d body=%s", w.Code, w.Body.String())
	}

	// 3. Invalid tag format rejected with 400.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/apps/webhook/tok_ci", strings.NewReader(`{"tag":"bad tag!"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid tag must fail 400, got %d body=%s", w.Code, w.Body.String())
	}

	// 4. Token via Authorization: Bearer on /apps/webhook/_ with empty body triggers rolling update.
	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/apps/webhook/_", nil)
	req.Header.Set("Authorization", "Bearer tok_ci")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("bearer webhook failed: code=%d body=%s", w.Code, w.Body.String())
	}
	waitSettled("app_ci")

	// 5. GET /apps/:id/triggers returns recorded triggered & rejected events.
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/apps/app_ci/triggers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list triggers failed: code=%d body=%s", w.Code, w.Body.String())
	}
	var trResp struct {
		Data []TriggerEvent `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &trResp); err != nil {
		t.Fatal(err)
	}
	if len(trResp.Data) < 3 {
		t.Fatalf("expected at least 3 trigger events, got %d: %+v", len(trResp.Data), trResp.Data)
	}

	// 6. Image app rollback uses historical Deployment.ImageRef snapshot.
	if err := s.AddDeployment(&Deployment{
		ID: "dep_img_prev", AppID: "app_ci", Trigger: TriggerCI,
		Status: DeploySuccess, ImageRef: "ghcr.io/acme/web:v1.0.0", StartedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/apps/app_ci/rollback?deployment_id=dep_img_prev", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("image rollback failed: code=%d body=%s", w.Code, w.Body.String())
	}
	waitSettled("app_ci")
	var rbResp struct {
		Data Deployment `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rbResp); err != nil {
		t.Fatal(err)
	}
	if rbResp.Data.Trigger != "rollback" {
		t.Fatalf("expected rollback trigger, got %+v", rbResp.Data)
	}
}

func TestUpdateAppClearLegacyNotice(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.PutApp(&Application{
		ID: "app_leg", Name: "legacy", HostID: "h1", SourceType: "git",
		RepoURL: "https://github.com/acme/web", AutoDeploy: true,
		WebhookAutoManaged: true, GitHubHookID: 42, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Reloading promotes legacy auto-managed flag into dismissible LegacyWebhookNotice.
	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.GetApp("app_leg")
	if !ok || !got.LegacyWebhookNotice || got.WebhookAutoManaged || got.GitHubHookID != 0 {
		t.Fatalf("expected legacy flag promoted on reload, got %+v", got)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	e := NewEngine(nil, s2, nil, nil)
	h := NewAppHandlers(nil, s2, e)
	h.RegisterAppRoutes(r.Group(""), r.Group(""), r.Group("/api/v1"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/apps/app_leg", strings.NewReader(`{"legacy_webhook_notice":false}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("clear legacy_webhook_notice failed: %d %s", w.Code, w.Body.String())
	}
	got2, _ := s2.GetApp("app_leg")
	if got2.LegacyWebhookNotice {
		t.Fatal("legacy_webhook_notice should be cleared")
	}
}
