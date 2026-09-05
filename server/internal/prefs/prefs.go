// Package prefs stores per-user terminal preferences (AGENTS.md B.8.1
// term_prefs: theme / default shell / font family / font size).
//
// These live on the server rather than in browser localStorage so a user gets
// the same terminal appearance from any machine they log in from.
//
// Persistence follows the same pattern as the other server stores: a single
// JSON file under the data directory, guarded by a mutex.
package prefs

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Terminal color themes the web terminal can render. The server validates
// against this list so a typo is rejected instead of silently falling back.
const (
	ThemeGitHubDark    = "GitHub Dark"
	ThemeDracula       = "Dracula"
	ThemeMiku          = "Miku"
	ThemeSolarizedDark = "Solarized Dark"
	ThemeMonokai       = "Monokai"
)

// Shells a terminal session may request. An empty DefaultShell means "let the
// agent pick the platform default" (bash on Linux, powershell on Windows).
const (
	ShellBash       = "bash"
	ShellSh         = "sh"
	ShellCmd        = "cmd"
	ShellPowerShell = "powershell"
)

// Font size bounds. Below 10 the terminal is unreadable; above 24 an 80-column
// shell no longer fits a normal browser window.
const (
	MinFontSize = 10
	MaxFontSize = 24
)

// Scrollback bounds. The buffer is per-session in browser memory, so an
// unbounded value would let one preference exhaust the tab's heap.
const (
	MinScrollback = 500
	MaxScrollback = 100000
)

// Prefs is one user's terminal preferences.
type Prefs struct {
	Username string `json:"username"`
	Theme    string `json:"theme"`
	// DefaultShell is pre-selected when opening a terminal; empty means the
	// agent's platform default.
	DefaultShell string    `json:"default_shell"`
	FontFamily   string    `json:"font_family"`
	FontSize     int       `json:"font_size"`
	CursorBlink  bool      `json:"cursor_blink"`
	Scrollback   int       `json:"scrollback"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Defaults returns the preferences a user has before saving anything. They
// match what the web terminal rendered before preferences existed, so an
// upgrade does not change anyone's terminal.
func Defaults(username string) Prefs {
	return Prefs{
		Username:     username,
		Theme:        ThemeGitHubDark,
		DefaultShell: "",
		FontFamily:   `Consolas, "Cascadia Code", "Courier New", monospace`,
		FontSize:     14,
		CursorBlink:  true,
		Scrollback:   2000,
	}
}

// Themes lists the selectable themes, for the client to render a picker
// without hardcoding the list a second time.
func Themes() []string {
	return []string{ThemeGitHubDark, ThemeDracula, ThemeMiku, ThemeSolarizedDark, ThemeMonokai}
}

// Store persists terminal preferences keyed by username.
type Store struct {
	mu      sync.Mutex
	dataDir string
	log     *slog.Logger
	entries map[string]Prefs
}

// NewStore loads preferences from disk.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{dataDir: dataDir, log: log, entries: map[string]Prefs{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string {
	return filepath.Join(s.dataDir, "term_prefs.json")
}

func (s *Store) load() error {
	if s.dataDir == "" {
		return nil
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read term_prefs.json: %w", err)
	}
	var entries map[string]Prefs
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse term_prefs.json: %w", err)
	}
	if entries != nil {
		s.entries = entries
	}
	return nil
}

func (s *Store) save() error {
	if s.dataDir == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), data, 0o600)
}

// Get returns the user's saved preferences, falling back to defaults for a
// user who has never saved any. Individual empty fields also fall back, so a
// preference added in a later release applies to existing users.
func (s *Store) Get(username string) Prefs {
	s.mu.Lock()
	defer s.mu.Unlock()

	def := Defaults(username)
	p, ok := s.entries[username]
	if !ok {
		return def
	}
	p.Username = username
	if p.Theme == "" {
		p.Theme = def.Theme
	}
	if p.FontFamily == "" {
		p.FontFamily = def.FontFamily
	}
	if p.FontSize == 0 {
		p.FontSize = def.FontSize
	}
	if p.Scrollback == 0 {
		p.Scrollback = def.Scrollback
	}
	return p
}

// Set validates and stores a user's preferences, returning the stored value.
func (s *Store) Set(username string, in Prefs) (Prefs, error) {
	if username == "" {
		return Prefs{}, errors.New("缺少用户身份")
	}
	if err := validate(&in); err != nil {
		return Prefs{}, err
	}
	in.Username = username
	in.UpdatedAt = time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[username] = in
	if err := s.save(); err != nil {
		return Prefs{}, err
	}
	return in, nil
}

// Delete drops a user's saved preferences, so the next Get returns defaults.
// Called when the user account is removed.
func (s *Store) Delete(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[username]; !ok {
		return
	}
	delete(s.entries, username)
	if err := s.save(); err != nil {
		s.log.Warn("term prefs save after delete", "user", username, "err", err)
	}
}

func validate(p *Prefs) error {
	p.Theme = strings.TrimSpace(p.Theme)
	p.DefaultShell = strings.TrimSpace(p.DefaultShell)
	p.FontFamily = strings.TrimSpace(p.FontFamily)

	def := Defaults("")
	if p.Theme == "" {
		p.Theme = def.Theme
	}
	if !validTheme(p.Theme) {
		return fmt.Errorf("不支持的终端主题: %s", p.Theme)
	}
	switch p.DefaultShell {
	case "", ShellBash, ShellSh, ShellCmd, ShellPowerShell:
	default:
		return fmt.Errorf("不支持的 shell 类型: %s", p.DefaultShell)
	}
	if p.FontFamily == "" {
		p.FontFamily = def.FontFamily
	}
	// A font-family string ends up in a CSS declaration in the browser, so
	// reject the characters that could break out of it.
	if strings.ContainsAny(p.FontFamily, "{};<>") {
		return errors.New("字体名称包含非法字符")
	}
	if len(p.FontFamily) > 200 {
		return errors.New("字体名称过长")
	}
	if p.FontSize == 0 {
		p.FontSize = def.FontSize
	}
	if p.FontSize < MinFontSize || p.FontSize > MaxFontSize {
		return fmt.Errorf("字号需在 %d-%d 之间", MinFontSize, MaxFontSize)
	}
	if p.Scrollback == 0 {
		p.Scrollback = def.Scrollback
	}
	if p.Scrollback < MinScrollback || p.Scrollback > MaxScrollback {
		return fmt.Errorf("回滚行数需在 %d-%d 之间", MinScrollback, MaxScrollback)
	}
	return nil
}

func validTheme(name string) bool {
	for _, t := range Themes() {
		if t == name {
			return true
		}
	}
	return false
}
