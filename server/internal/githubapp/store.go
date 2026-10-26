package githubapp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const storeFileName = "github_app.json"

// Installation is one account (personal or organization) the GitHub App is
// installed on. Repos lists the installation's repository full names as of
// the last sync; it is a matching cache, not an authorization source — the
// actual push routing only needs the repo name, and git fetch re-resolves
// the installation token through the GitHub API.
type Installation struct {
	ID           int64     `json:"id"`
	AccountLogin string    `json:"account_login"`
	AccountType  string    `json:"account_type"` // User | Organization
	TargetType   string    `json:"target_type"`  // all | selected
	Repos        []string  `json:"repos,omitempty"`
	Suspended    bool      `json:"suspended,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Config is the GitHub App identity. PrivateKeyPEM and WebhookSecret are
// secrets: they are persisted (0600, same precedent as git_provider.json)
// but never leave the server through any API response.
type Config struct {
	AppID         int64     `json:"app_id"`
	Slug          string    `json:"slug"`
	PrivateKeyPEM string    `json:"private_key_pem"`
	WebhookSecret string    `json:"webhook_secret"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type fileState struct {
	Config        *Config        `json:"config,omitempty"`
	Installations []Installation `json:"installations,omitempty"`
}

// Store persists the GitHub App configuration and installation cache under
// the server data dir. It deliberately does NOT use the user-facing Vault:
// the App private key is a system secret, and putting it in the Vault would
// surface it in the Vault UI. The gitprovider precedent (git_provider.json,
// 0600, API never echoes the token) is the matching convention.
type Store struct {
	mu            sync.RWMutex
	dir           string
	log           *slog.Logger
	config        *Config
	installations []Installation
}

func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	s := &Store{dir: dataDir, log: log}
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
		s.log.Warn("github_app.json corrupted, starting empty", "err", err)
		return nil
	}
	s.config = state.Config
	s.installations = state.Installations
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	state := fileState{Config: s.config, Installations: s.installations}
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

// Configured reports whether a GitHub App has been set up.
func (s *Store) Configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config != nil && s.config.AppID > 0
}

// GetConfig returns a copy of the stored App identity, if any.
func (s *Store) GetConfig() (*Config, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.config == nil {
		return nil, false
	}
	cp := *s.config
	return &cp, true
}

// SetConfig stores (or replaces) the App identity.
func (s *Store) SetConfig(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.config != nil {
		cfg.CreatedAt = s.config.CreatedAt
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = now
	}
	cfg.UpdatedAt = now
	s.config = cfg
	return s.saveLocked()
}

// Clear removes the App identity and all installation records.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = nil
	s.installations = nil
	return s.saveLocked()
}

// Installations returns a copy of the cached installation list.
func (s *Store) Installations() []Installation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Installation, len(s.installations))
	copy(out, s.installations)
	return out
}

// SetInstallations replaces the cached installation list.
func (s *Store) SetInstallations(list []Installation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installations = list
	return s.saveLocked()
}

// UpsertInstallation inserts or replaces one installation record.
func (s *Store) UpsertInstallation(inst Installation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst.UpdatedAt = time.Now()
	for i := range s.installations {
		if s.installations[i].ID == inst.ID {
			if len(inst.Repos) == 0 {
				inst.Repos = s.installations[i].Repos
			}
			s.installations[i] = inst
			return s.saveLocked()
		}
	}
	s.installations = append(s.installations, inst)
	return s.saveLocked()
}

// RemoveInstallation drops one installation record.
func (s *Store) RemoveInstallation(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]Installation, 0, len(s.installations))
	for _, inst := range s.installations {
		if inst.ID != id {
			kept = append(kept, inst)
		}
	}
	s.installations = kept
	return s.saveLocked()
}

// FindInstallationForRepo returns the installation whose cached repo list
// contains the normalized "owner/repo" name.
func (s *Store) FindInstallationForRepo(normalizedRepo string) (Installation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inst := range s.installations {
		if inst.Suspended {
			continue
		}
		for _, r := range inst.Repos {
			if r == normalizedRepo {
				return inst, true
			}
		}
	}
	return Installation{}, false
}
