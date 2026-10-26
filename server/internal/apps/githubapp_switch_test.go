package apps

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"watchman/server/internal/githubapp"
)

func switchRequest(h *AppHandlers, id string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/apps/"+id+"/github-app/switch", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.switchToGitHubApp(c)
	return w
}

func newSwitchHandler(t *testing.T, covered bool, configured bool) (*AppHandlers, *Store) {
	t.Helper()
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutApp(&Application{
		ID: "sw1", Name: "web", HostID: "h1", SourceType: "git",
		RepoURL: "https://github.com/acme/web.git", Branch: "main",
		AutoDeploy: true, WebhookAutoManaged: true, GitHubHookID: 0,
	}); err != nil {
		t.Fatal(err)
	}
	ghStore, err := githubapp.NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		if err := ghStore.SetConfig(&githubapp.Config{AppID: 1, Slug: "test"}); err != nil {
			t.Fatal(err)
		}
		if covered {
			if err := ghStore.SetInstallations([]githubapp.Installation{
				{ID: 11, AccountLogin: "acme", Repos: []string{"acme/web"}},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := NewAppHandlers(nil, store, nil)
	h.SetGitHubAppStore(ghStore)
	return h, store
}

func TestSwitchToGitHubAppCovered(t *testing.T) {
	h, store := newSwitchHandler(t, true, true)
	w := switchRequest(h, "sw1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "已删除") {
		t.Fatalf("no hook existed, message must not claim one was deleted: %s", w.Body.String())
	}
	app, _ := store.GetApp("sw1")
	if app.WebhookAutoManaged || app.GitHubHookID != 0 {
		t.Fatalf("auto-managed flags should be cleared, got %+v", app)
	}
}

// Switching a repository no installation covers must fail BEFORE the
// legacy hook is touched: a success here would silently break auto-deploy.
func TestSwitchToGitHubAppUncovered(t *testing.T) {
	h, store := newSwitchHandler(t, false, true)
	w := switchRequest(h, "sw1")
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	app, _ := store.GetApp("sw1")
	if !app.WebhookAutoManaged {
		t.Fatalf("rejected switch must not clear flags, got %+v", app)
	}
}

func TestSwitchToGitHubAppUnconfigured(t *testing.T) {
	h, _ := newSwitchHandler(t, false, false)
	w := switchRequest(h, "sw1")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}
