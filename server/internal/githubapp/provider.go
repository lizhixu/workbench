package githubapp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// TokenProvider resolves a git-fetch credential for a repository from the
// GitHub App installations. It sits between the app's explicit Vault
// credential and the legacy provider token in the deploy credential chain:
// explicit vault credential > installation token > provider token.
//
// Tokens are minted on demand and cached in memory only, refreshed when
// fewer than 5 minutes of validity remain. They are never persisted.
type TokenProvider struct {
	store  *Store
	client *Client
	mu     sync.Mutex
	cache  map[int64]cachedToken
}

func NewTokenProvider(store *Store, client *Client) *TokenProvider {
	return &TokenProvider{store: store, client: client, cache: map[int64]cachedToken{}}
}

// InstallationTokenForRepo returns an installation token that can read the
// given repository, or false when no installation covers it.
func (p *TokenProvider) InstallationTokenForRepo(repoURL string) (string, bool) {
	norm := NormalizeRepoURL(repoURL)
	// The token is only meaningful for github.com remotes: a bare
	// "owner/repo" string stored for another host must never resolve to a
	// GitHub installation token.
	if norm == "" || !strings.Contains(strings.ToLower(repoURL), "github.com") || !p.store.Configured() {
		return "", false
	}
	if inst, ok := p.store.FindInstallationForRepo(norm); ok {
		if tok, err := p.tokenFor(inst.ID); err == nil {
			return tok, true
		}
		return "", false
	}
	// Cache miss: the repo may have been added to an installation after the
	// last sync. Refresh every installation's repo list once, then retry
	// regardless of whether the refresh fully succeeded — a partial refresh
	// still persists what it fetched (see RefreshRepos).
	p.RefreshRepos()
	if inst, ok := p.store.FindInstallationForRepo(norm); ok {
		if tok, err := p.tokenFor(inst.ID); err == nil {
			return tok, true
		}
	}
	return "", false
}

// ResetCache drops all cached installation tokens. Call it when the App
// configuration changes or is disconnected, so a stale token minted under
// an old identity can never be reused.
func (p *TokenProvider) ResetCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache = map[int64]cachedToken{}
}

func (p *TokenProvider) tokenFor(installationID int64) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg, ok := p.store.GetConfig()
	if !ok {
		return "", errNotConfigured
	}
	if cached, ok := p.cache[installationID]; ok && time.Until(cached.expiresAt) > 5*time.Minute {
		return cached.token, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	jwt, err := MintJWT(cfg.AppID, cfg.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	tok, err := p.client.CreateInstallationToken(ctx, jwt, installationID)
	if err != nil {
		return "", err
	}
	p.cache[installationID] = cachedToken{token: tok.Token, expiresAt: tok.ExpiresAt}
	return tok.Token, nil
}

// RefreshRepos re-lists every installation's repositories from GitHub and
// updates the store cache. Installations are fetched in parallel (bounded
// to 4 concurrent workers) so a multi-installation account syncs in seconds
// rather than one 15s client timeout after another.
//
// Merge semantics (a partial refresh must never silently drop coverage):
// installations whose token/repos fetch fails keep their previous cached
// record; only installations absent from GitHub's fresh list are dropped
// (genuinely uninstalled). An error is still returned naming how many
// installations failed to refresh, so callers can report honestly — but the
// merged state is persisted regardless.
func (p *TokenProvider) RefreshRepos() error {
	cfg, ok := p.store.GetConfig()
	if !ok {
		return errNotConfigured
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	jwt, err := MintJWT(cfg.AppID, cfg.PrivateKeyPEM)
	if err != nil {
		return err
	}
	installations, err := p.client.ListInstallations(ctx, jwt)
	if err != nil {
		return err
	}
	type fetched struct {
		inst   Installation
		failed bool
	}
	results := make([]fetched, len(installations))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, info := range installations {
		wg.Add(1)
		go func(i int, info InstallationInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rec := Installation{
				ID:           info.ID,
				AccountLogin: info.Account.Login,
				AccountType:  info.Account.Type,
				TargetType:   info.TargetType,
				Suspended:    info.Suspended(),
				UpdatedAt:    time.Now(),
			}
			tok, err := p.client.CreateInstallationToken(ctx, jwt, info.ID)
			if err != nil {
				results[i] = fetched{inst: rec, failed: true}
				return
			}
			repos, err := p.client.ListInstallationRepos(ctx, tok.Token)
			if err != nil {
				results[i] = fetched{inst: rec, failed: true}
				return
			}
			rec.Repos = repos
			results[i] = fetched{inst: rec}
		}(i, info)
	}
	wg.Wait()
	old := p.store.Installations()
	oldByID := make(map[int64]Installation, len(old))
	for _, o := range old {
		oldByID[o.ID] = o
	}
	var out []Installation
	failed := 0
	for _, r := range results {
		if r.failed {
			failed++
			// Keep the previous cached record (repos included) instead of
			// dropping the installation from coverage.
			if prev, ok := oldByID[r.inst.ID]; ok {
				out = append(out, prev)
			}
			continue
		}
		out = append(out, r.inst)
	}
	if out == nil {
		out = []Installation{}
	}
	if err := p.store.SetInstallations(out); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d 个安装的仓库清单刷新失败，已保留旧缓存", failed)
	}
	return nil
}

var errNotConfigured = errString("github app 未配置")

type errString string

func (e errString) Error() string { return string(e) }
