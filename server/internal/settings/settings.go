// Package settings is the unified settings store (settings-center Phase 2).
//
// It replaces the old per-user terminal prefs store (internal/prefs): a single
// JSON document holds every setting, addressed by dotted keys, in two scopes:
//
//	system – control-plane wide, admin-only (registry is empty in Phase 2;
//	         Phase 3 adds system keys here)
//	user   – per login name, readable and writable by the owning user only
//
// Every key is declared in Definitions with its kind, default and validation
// rules. Writes are validated before they are persisted, so a bad value is
// rejected at the API instead of corrupting the file. Unknown keys are
// rejected as well: the registry is the whitelist.
//
// On first start the store migrates the legacy term_prefs.json automatically
// (renamed to term_prefs.json.migrated afterwards; the original data is kept).
//
// Secrets (AI API keys, Git provider tokens, …) deliberately stay in their
// dedicated stores and never enter this KV: this store holds configuration,
// not credentials.
//
// Persistence follows the same pattern as the other server stores: a single
// JSON file under the data directory, guarded by a mutex, written via
// temp-file + rename so a crash cannot leave a half-written document.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Scope is the visibility/ownership of a setting key.
type Scope string

const (
	// ScopeSystem is control-plane wide and admin-only.
	ScopeSystem Scope = "system"
	// ScopeUser is per login name.
	ScopeUser Scope = "user"
)

// Kind is the value type of a setting key.
type Kind string

const (
	KindString Kind = "string"
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
	KindEnum   Kind = "enum"
)

// Definition describes one setting key: scope, type, default and rules.
type Definition struct {
	Key   string
	Scope Scope
	Kind  Kind
	// Title is the human label; it also feeds the settings search UI later.
	Title string
	// Default is the effective value when the user never saved one.
	Default any
	// Enum lists the accepted values (KindEnum only).
	Enum []string
	// Min/Max bound accepted values (KindInt only).
	Min int
	Max int
	// MaxLen bounds the length (KindString only).
	MaxLen int
	// Reject lists characters that must not appear (KindString only).
	Reject string
	// DefaultOnEmpty maps an empty string / 0 to Default, mirroring the old
	// prefs behavior where an unset field fell back to its default instead
	// of failing validation.
	DefaultOnEmpty bool
	// Validate runs after the generic kind checks for domain-specific rules.
	// A nil Validate means no extra rule.
	Validate func(v any) error
}

// Terminal color themes the web terminal can render. The server validates
// against this list so a typo is rejected instead of silently falling back.
const (
	TermThemeGitHubDark    = "GitHub Dark"
	TermThemeDracula       = "Dracula"
	TermThemeMiku          = "Miku"
	TermThemeSolarizedDark = "Solarized Dark"
	TermThemeMonokai       = "Monokai"
)

// TermThemes lists the selectable terminal themes, for the client to render a
// picker without hardcoding the list a second time.
func TermThemes() []string {
	return []string{
		TermThemeGitHubDark,
		TermThemeDracula,
		TermThemeMiku,
		TermThemeSolarizedDark,
		TermThemeMonokai,
	}
}

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

// DefaultUIFontFamily is the default terminal font stack.
const DefaultUIFontFamily = `Consolas, "Cascadia Code", "Courier New", monospace`

// Definitions is the Phase 2 key registry: the old terminal preferences plus
// the UI theme mode migrated from browser localStorage.
var Definitions = []Definition{
	{Key: "appearance.theme_mode", Scope: ScopeUser, Kind: KindEnum, Title: "界面主题", Default: "dark", Enum: []string{"dark", "light"}},
	{Key: "terminal.theme", Scope: ScopeUser, Kind: KindEnum, Title: "终端配色主题", Default: TermThemeGitHubDark, Enum: TermThemes(), DefaultOnEmpty: true},
	{Key: "terminal.default_shell", Scope: ScopeUser, Kind: KindEnum, Title: "默认 Shell", Default: "", Enum: []string{"", ShellBash, ShellSh, ShellCmd, ShellPowerShell}},
	{Key: "terminal.font_family", Scope: ScopeUser, Kind: KindString, Title: "终端字体", Default: DefaultUIFontFamily, MaxLen: 200, Reject: "{};<>", DefaultOnEmpty: true},
	{Key: "terminal.font_size", Scope: ScopeUser, Kind: KindInt, Title: "终端字号", Default: 14, Min: MinFontSize, Max: MaxFontSize, DefaultOnEmpty: true},
	{Key: "terminal.cursor_blink", Scope: ScopeUser, Kind: KindBool, Title: "光标闪烁", Default: true},
	{Key: "terminal.scrollback", Scope: ScopeUser, Kind: KindInt, Title: "回滚行数", Default: 2000, Min: MinScrollback, Max: MaxScrollback, DefaultOnEmpty: true},
	// Phase 3: the settings AGENTS.md 3.12 requires but no UI had yet.
	{Key: "navigation.default_host_tab", Scope: ScopeUser, Kind: KindEnum, Title: "主机默认页签", Default: "files",
		Enum: []string{"files", "metrics", "sysinfo", "terminal", "docker", "vulnerabilities", "network"}},
	{Key: "appearance.show_tips", Scope: ScopeUser, Kind: KindBool, Title: "显示功能提示语", Default: true},
	{Key: "files.default_path", Scope: ScopeUser, Kind: KindString, Title: "文件管理默认路径", Default: "",
		MaxLen: 500, Validate: validateAbsPath},
}

// validateAbsPath accepts an empty value (OS default applies) or an absolute
// path: Unix-style or a Windows drive path.
func validateAbsPath(v any) error {
	s, _ := v.(string)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "/") {
		return nil
	}
	if len(s) >= 3 && isASCIILetter(s[0]) && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return nil
	}
	return errors.New("文件管理默认路径必须是绝对路径（/ 开头或盘符路径）")
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// defByKey indexes Definitions for validation lookups.
var defByKey = func() map[string]Definition {
	m := make(map[string]Definition, len(Definitions))
	for _, d := range Definitions {
		m[d.Key] = d
	}
	return m
}()

// normalize validates one raw JSON value against the key's definition and
// returns its canonical form. Unknown keys and invalid values are rejected.
func (d Definition) normalize(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: 无效的 JSON 值: %w", d.Key, err)
	}
	switch d.Kind {
	case KindEnum, KindString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s 必须是字符串", d.Key)
		}
		s = strings.TrimSpace(s)
		if s == "" && d.DefaultOnEmpty {
			if def, ok := d.Default.(string); ok {
				s = def
			}
		}
		if d.Kind == KindEnum {
			allowed := false
			for _, e := range d.Enum {
				if s == e {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, fmt.Errorf("%s 不支持的值: %q", d.Key, s)
			}
		} else {
			if d.MaxLen > 0 && len(s) > d.MaxLen {
				return nil, fmt.Errorf("%s 过长（最多 %d 字符）", d.Key, d.MaxLen)
			}
			// A font-family string ends up in a CSS declaration in the
			// browser, so reject the characters that could break out of it.
			if d.Reject != "" && strings.ContainsAny(s, d.Reject) {
				return nil, fmt.Errorf("%s 包含非法字符", d.Key)
			}
		}
		v = s
	case KindInt:
		f, ok := v.(float64)
		if !ok || f != math.Trunc(f) {
			return nil, fmt.Errorf("%s 必须是整数", d.Key)
		}
		n := int(f)
		if n == 0 && d.DefaultOnEmpty {
			if def, ok := d.Default.(int); ok {
				n = def
			}
		}
		if n < d.Min || n > d.Max {
			return nil, fmt.Errorf("%s 需在 %d-%d 之间", d.Key, d.Min, d.Max)
		}
		v = n
	case KindBool:
		if _, ok := v.(bool); !ok {
			return nil, fmt.Errorf("%s 必须是布尔值", d.Key)
		}
	default:
		return nil, fmt.Errorf("%s: 未知的值类型", d.Key)
	}
	if d.Validate != nil {
		if err := d.Validate(v); err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%s: 编码失败: %w", d.Key, err)
	}
	return out, nil
}

// TerminalPrefs is the typed view of the terminal.* keys for one user.
type TerminalPrefs struct {
	Theme        string `json:"theme"`
	DefaultShell string `json:"default_shell"`
	FontFamily   string `json:"font_family"`
	FontSize     int    `json:"font_size"`
	CursorBlink  bool   `json:"cursor_blink"`
	Scrollback   int    `json:"scrollback"`
}

// AppearancePrefs is the typed view of the appearance.* keys for one user.
type AppearancePrefs struct {
	ThemeMode string `json:"theme_mode"`
}

// document is the on-disk shape of settings.json.
type document struct {
	System map[string]json.RawMessage            `json:"system"`
	Users  map[string]map[string]json.RawMessage `json:"users"`
}

// Store persists settings keyed by scope (and username for user scope).
type Store struct {
	mu      sync.Mutex
	dataDir string
	log     *slog.Logger
	system  map[string]json.RawMessage
	users   map[string]map[string]json.RawMessage
}

// NewStore loads settings from disk, migrating the legacy term_prefs.json on
// first run.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		dataDir: dataDir,
		log:     log,
		system:  map[string]json.RawMessage{},
		users:   map[string]map[string]json.RawMessage{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string {
	return filepath.Join(s.dataDir, "settings.json")
}

func (s *Store) legacyPath() string {
	return filepath.Join(s.dataDir, "term_prefs.json")
}

func (s *Store) load() error {
	if s.dataDir == "" {
		return nil
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.migrateLegacy()
		}
		return fmt.Errorf("read settings.json: %w", err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse settings.json: %w", err)
	}
	if doc.System != nil {
		s.system = doc.System
	}
	if doc.Users != nil {
		s.users = doc.Users
	}
	return nil
}

// save persists the live document via temp-file + rename so a crash cannot
// leave a half-written settings.json behind.
func (s *Store) save() error {
	return writeDocument(s.dataDir, document{System: s.system, Users: s.users})
}

// writeDocument persists an arbitrary document the same way save does, so
// SetMany can write its candidate maps to disk before swapping them into the
// live in-memory state.
func writeDocument(dataDir string, doc document) error {
	if dataDir == "" {
		return nil
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, "settings-*.json")
	if err != nil {
		return fmt.Errorf("create temp settings file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp settings file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp settings file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp settings file: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dataDir, "settings.json")); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replace settings.json: %w", err)
	}
	return nil
}

// legacyPrefs mirrors the old prefs.Prefs JSON shape for migration.
type legacyPrefs struct {
	Theme        string `json:"theme"`
	DefaultShell string `json:"default_shell"`
	FontFamily   string `json:"font_family"`
	FontSize     int    `json:"font_size"`
	CursorBlink  bool   `json:"cursor_blink"`
	Scrollback   int    `json:"scrollback"`
}

// migrateLegacy imports term_prefs.json into the new document. It runs only
// when settings.json does not exist yet. The legacy file is renamed (not
// deleted) so the original data is always recoverable.
func (s *Store) migrateLegacy() error {
	data, err := os.ReadFile(s.legacyPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read term_prefs.json: %w", err)
	}
	var entries map[string]legacyPrefs
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse term_prefs.json: %w", err)
	}
	migrated := 0
	for username, p := range entries {
		kv := map[string]json.RawMessage{}
		set := func(key string, v any) {
			raw, err := json.Marshal(v)
			if err != nil {
				return
			}
			if norm, err := defByKey[key].normalize(raw); err == nil {
				kv[key] = norm
			} else {
				s.log.Warn("legacy pref failed validation, using default",
					"user", username, "key", key, "err", err)
			}
		}
		set("terminal.theme", p.Theme)
		set("terminal.default_shell", p.DefaultShell)
		set("terminal.font_family", p.FontFamily)
		set("terminal.font_size", p.FontSize)
		set("terminal.cursor_blink", p.CursorBlink)
		set("terminal.scrollback", p.Scrollback)
		s.users[username] = kv
		migrated++
	}
	if err := s.save(); err != nil {
		return err
	}
	bak := s.legacyPath() + ".migrated"
	if err := os.Rename(s.legacyPath(), bak); err != nil {
		s.log.Warn("migrated prefs but could not rename legacy file", "err", err)
		return nil
	}
	s.log.Info("migrated legacy term_prefs.json to settings.json", "users", migrated)
	return nil
}

// effective returns the stored value for key, or the definition default when
// nothing was saved. Callers must hold at least a read lock; all public
// methods lock.
func (s *Store) effective(scope Scope, username, key string) json.RawMessage {
	def, ok := defByKey[key]
	if !ok || def.Scope != scope {
		return nil
	}
	if scope == ScopeSystem {
		if raw, ok := s.system[key]; ok {
			return raw
		}
	} else {
		if kv, ok := s.users[username]; ok {
			if raw, ok := kv[key]; ok {
				return raw
			}
		}
	}
	raw, _ := json.Marshal(def.Default)
	return raw
}

// Get returns the effective value of one key (stored value or default).
func (s *Store) Get(scope Scope, username, key string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effective(scope, username, key)
}

// Effective returns every registered key of a scope with its effective value.
func (s *Store) Effective(scope Scope, username string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(Definitions))
	for _, d := range Definitions {
		if d.Scope != scope {
			continue
		}
		var v any
		if err := json.Unmarshal(s.effective(scope, username, d.Key), &v); err == nil {
			out[d.Key] = v
		}
	}
	return out
}

// StoredKeys returns the keys explicitly saved for a scope (and username),
// sorted. Unlike Effective it does not include defaults: the client uses it
// to tell "the user chose this value" apart from "the server filled in a
// default" — e.g. to decide whether a browser-local theme should be migrated
// up on first login. Keys no longer in the registry are skipped.
func (s *Store) StoredKeys(scope Scope, username string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := []string{}
	collect := func(kv map[string]json.RawMessage) {
		for k := range kv {
			if def, ok := defByKey[k]; ok && def.Scope == scope {
				keys = append(keys, k)
			}
		}
	}
	if scope == ScopeSystem {
		collect(s.system)
	} else {
		collect(s.users[username])
	}
	sort.Strings(keys)
	return keys
}

// SetMany validates every item first and then applies them atomically: if any
// key is unknown or any value is invalid, nothing is written. The update is
// also atomic against the in-memory state: candidate maps are built as copies
// and persisted first, and only swapped into the live state after the write
// succeeds, so a disk failure leaves the previous in-memory document — and
// therefore every subsequent read — untouched.
func (s *Store) SetMany(scope Scope, username string, items map[string]json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normed := make(map[string]json.RawMessage, len(items))
	for key, raw := range items {
		def, ok := defByKey[key]
		if !ok {
			return fmt.Errorf("未知的配置项: %s", key)
		}
		if def.Scope != scope {
			return fmt.Errorf("配置项 %s 不属于 %s 作用域", key, scope)
		}
		n, err := def.normalize(raw)
		if err != nil {
			return err
		}
		normed[key] = n
	}
	// Build the candidate document as copies: the live maps must not be
	// mutated until the write has succeeded.
	newSystem := make(map[string]json.RawMessage, len(s.system)+len(normed))
	for k, v := range s.system {
		newSystem[k] = v
	}
	newUsers := make(map[string]map[string]json.RawMessage, len(s.users)+1)
	for u, kv := range s.users {
		newUsers[u] = kv
	}
	switch scope {
	case ScopeSystem:
		for k, v := range normed {
			newSystem[k] = v
		}
	case ScopeUser:
		kv := make(map[string]json.RawMessage, len(s.users[username])+len(normed))
		for k, v := range s.users[username] {
			kv[k] = v
		}
		for k, v := range normed {
			kv[k] = v
		}
		newUsers[username] = kv
	default:
		return fmt.Errorf("未知的作用域: %s", scope)
	}
	if err := writeDocument(s.dataDir, document{System: newSystem, Users: newUsers}); err != nil {
		return err
	}
	s.system = newSystem
	s.users = newUsers
	return nil
}

// DeleteUser drops every user-scope setting of a username, so a recreated
// account starts fresh.
func (s *Store) DeleteUser(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users, username)
	_ = s.save()
}

// decode fetches one effective key into v; used by the typed accessors.
func (s *Store) decode(scope Scope, username, key string, v any) {
	raw := s.Get(scope, username, key)
	_ = json.Unmarshal(raw, v)
}

// Terminal returns the user's typed terminal preferences.
func (s *Store) Terminal(username string) TerminalPrefs {
	var p TerminalPrefs
	s.decode(ScopeUser, username, "terminal.theme", &p.Theme)
	s.decode(ScopeUser, username, "terminal.default_shell", &p.DefaultShell)
	s.decode(ScopeUser, username, "terminal.font_family", &p.FontFamily)
	s.decode(ScopeUser, username, "terminal.font_size", &p.FontSize)
	s.decode(ScopeUser, username, "terminal.cursor_blink", &p.CursorBlink)
	s.decode(ScopeUser, username, "terminal.scrollback", &p.Scrollback)
	return p
}

// Appearance returns the user's typed appearance preferences.
func (s *Store) Appearance(username string) AppearancePrefs {
	var p AppearancePrefs
	s.decode(ScopeUser, username, "appearance.theme_mode", &p.ThemeMode)
	return p
}
