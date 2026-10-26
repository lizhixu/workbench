package githubapp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Fake GitHub API for RefreshRepos: installation 11 refreshes cleanly,
// installation 22 fails at the token step. A partial refresh must keep 22's
// previous cached repos instead of dropping the installation from coverage.
func TestRefreshReposKeepsFailedInstallations(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":11,"account":{"login":"acme","type":"Organization"},"target_type":"selected","suspended_by":null},
			{"id":22,"account":{"login":"other","type":"User"},"target_type":"selected","suspended_by":null}
		]`))
	})
	mux.HandleFunc("/app/installations/11/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "tok-11",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/app/installations/22/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-11" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repositories": []map[string]string{{"full_name": "Acme/Web"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = oldBase }()

	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfig(&Config{AppID: 1, Slug: "test", PrivateKeyPEM: testPrivateKey(t), WebhookSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetInstallations([]Installation{
		{ID: 11, AccountLogin: "acme", Repos: []string{"acme/old"}},
		{ID: 22, AccountLogin: "other", Repos: []string{"acme/legacy"}},
	}); err != nil {
		t.Fatal(err)
	}

	p := NewTokenProvider(store, NewClient())
	err = p.RefreshRepos()
	if err == nil {
		t.Fatal("partial refresh should report an error")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Fatalf("error should name the failed count, got: %v", err)
	}
	byID := map[int64]Installation{}
	for _, inst := range store.Installations() {
		byID[inst.ID] = inst
	}
	if got := byID[11].Repos; len(got) != 1 || got[0] != "acme/web" {
		t.Fatalf("installation 11 should carry refreshed repos, got %v", got)
	}
	if got := byID[22].Repos; len(got) != 1 || got[0] != "acme/legacy" {
		t.Fatalf("installation 22 must keep its previous repos, got %v", got)
	}
}

// A repository URL that is not github.com must never resolve to an
// installation token, even when a same-named repository is installed.
func TestInstallationTokenForRepoRejectsNonGitHub(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetConfig(&Config{AppID: 1, PrivateKeyPEM: testPrivateKey(t), WebhookSecret: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetInstallations([]Installation{
		{ID: 11, AccountLogin: "acme", Repos: []string{"acme/web"}},
	}); err != nil {
		t.Fatal(err)
	}
	p := NewTokenProvider(store, NewClient())
	for _, url := range []string{
		"https://gitlab.example.com/acme/web",
		"acme/web",
	} {
		if tok, ok := p.InstallationTokenForRepo(url); ok || tok != "" {
			t.Fatalf("non-GitHub URL %q must not resolve a token", url)
		}
	}
}
