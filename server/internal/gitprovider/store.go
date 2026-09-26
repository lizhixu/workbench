package gitprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const storeFileName = "git_provider.json"

type fileState struct {
	GitHubAccount *Account     `json:"github_account,omitempty"`
	GitHubOAuth   *OAuthConfig `json:"github_oauth,omitempty"`
}

// Store persists GitHub authorization data under dataDir.
type Store struct {
	mu      sync.RWMutex
	dir     string
	log     *slog.Logger
	account *Account
	oauth   *OAuthConfig
	client  *Client
}

// NewStore loads or initializes the Git provider store.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		dir:    dataDir,
		log:    log,
		client: NewClient(),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) filePath() string {
	return filepath.Join(s.dir, storeFileName)
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.filePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", storeFileName, err)
	}
	var state fileState
	if err := json.Unmarshal(b, &state); err != nil {
		s.log.Warn("git_provider.json corrupted, starting empty", "err", err)
		return nil
	}
	s.account = state.GitHubAccount
	s.oauth = state.GitHubOAuth
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	state := fileState{
		GitHubAccount: s.account,
		GitHubOAuth:   s.oauth,
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, fmt.Sprintf("%s.tmp.%d", storeFileName, time.Now().UnixNano()))
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath())
}

// GetAccount returns the current authorized GitHub account, if any.
func (s *Store) GetAccount() (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil {
		return nil, false
	}
	cp := *s.account
	return &cp, true
}

// GetToken returns the raw token for GitHub API calls and Git cloning.
func (s *Store) GetToken() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil || s.account.Token == "" {
		return "", false
	}
	return s.account.Token, true
}

// SetAccount saves or replaces the authorized account.
func (s *Store) SetAccount(acc *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = acc
	return s.saveLocked()
}

// DeleteAccount removes the GitHub connection.
func (s *Store) DeleteAccount() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = nil
	return s.saveLocked()
}

// GetOAuthConfig returns OAuth App settings.
func (s *Store) GetOAuthConfig() (*OAuthConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.oauth == nil {
		return nil, false
	}
	cp := *s.oauth
	return &cp, true
}

// SetOAuthConfig updates OAuth App settings.
func (s *Store) SetOAuthConfig(cfg *OAuthConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.oauth = cfg
	return s.saveLocked()
}

// ParseGitHubRepo parses owner and repository name from various GitHub URL formats.
func ParseGitHubRepo(rawURL string) (owner, repo string, ok bool) {
	raw := strings.TrimSpace(rawURL)
	if strings.HasPrefix(raw, "git@github.com:") {
		path := strings.TrimPrefix(raw, "git@github.com:")
		path = strings.TrimSuffix(path, ".git")
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], true
		}
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	if !strings.Contains(strings.ToLower(u.Host), "github.com") {
		return "", "", false
	}
	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

// EnsureRepoWebhook checks if the Git repository belongs to GitHub and, if an authorized
// GitHub account exists, registers the specified webhook URL.
func (s *Store) EnsureRepoWebhook(ctx context.Context, repoURL, webhookURL string) (int64, error) {
	owner, repo, ok := ParseGitHubRepo(repoURL)
	if !ok {
		return 0, fmt.Errorf("不是有效的 GitHub 仓库地址: %s", repoURL)
	}
	token, ok := s.GetToken()
	if !ok || token == "" {
		return 0, fmt.Errorf("尚未连接 GitHub 账号，无法自动注册 Webhook")
	}
	return s.client.EnsureRepoWebhook(ctx, token, owner, repo, webhookURL)
}

// DeleteRepoWebhook removes the webhook from GitHub if configured.
func (s *Store) DeleteRepoWebhook(ctx context.Context, repoURL string, hookID int64) error {
	if hookID <= 0 {
		return nil
	}
	owner, repo, ok := ParseGitHubRepo(repoURL)
	if !ok {
		return nil
	}
	token, ok := s.GetToken()
	if !ok || token == "" {
		return nil
	}
	return s.client.DeleteRepoWebhook(ctx, token, owner, repo, hookID)
}
