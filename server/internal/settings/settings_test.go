package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestDefaultsForUnknownUser(t *testing.T) {
	s := newTestStore(t)
	tp := s.Terminal("nobody")
	if tp.Theme != TermThemeGitHubDark || tp.FontSize != 14 || !tp.CursorBlink || tp.Scrollback != 2000 {
		t.Fatalf("unexpected defaults: %+v", tp)
	}
	if got := s.Appearance("nobody").ThemeMode; got != "dark" {
		t.Fatalf("default theme_mode = %q, want dark", got)
	}
}

func TestSetManyRoundtrip(t *testing.T) {
	s := newTestStore(t)
	err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"terminal.theme":     raw(t, "Dracula"),
		"terminal.font_size": raw(t, 18),
		"appearance.theme_mode": raw(t, "light"),
	})
	if err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	tp := s.Terminal("alice")
	if tp.Theme != "Dracula" || tp.FontSize != 18 {
		t.Fatalf("roundtrip mismatch: %+v", tp)
	}
	if got := s.Appearance("alice").ThemeMode; got != "light" {
		t.Fatalf("theme_mode = %q, want light", got)
	}
	// Other users are unaffected.
	if got := s.Appearance("bob").ThemeMode; got != "dark" {
		t.Fatalf("bob theme_mode = %q, want dark", got)
	}
}

func TestSetManyRejectsUnknownKey(t *testing.T) {
	s := newTestStore(t)
	err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"nope.not_real": raw(t, 1),
	})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestSetManyRejectsWrongScope(t *testing.T) {
	s := newTestStore(t)
	err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"terminal.theme": raw(t, "Dracula"),
	})
	if err == nil {
		t.Fatal("expected error for user key in system scope")
	}
}

func TestSetManyIsAtomic(t *testing.T) {
	s := newTestStore(t)
	err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"terminal.theme":     raw(t, "Dracula"),
		"terminal.font_size": raw(t, 999), // invalid
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	// The valid key must not have been applied either.
	if got := s.Terminal("alice").Theme; got != TermThemeGitHubDark {
		t.Fatalf("atomicity violated: theme = %q", got)
	}
}

func TestValidationRules(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		key string
		val any
	}{
		{"terminal.theme", "Not A Theme"},
		{"terminal.theme", 123},
		{"terminal.default_shell", "fish"},
		{"terminal.font_size", 9},
		{"terminal.font_size", 25},
		{"terminal.font_size", 14.5},
		{"terminal.font_size", "14"},
		{"terminal.scrollback", 499},
		{"terminal.scrollback", 100001},
		{"terminal.font_family", "bad;font"},
		{"terminal.cursor_blink", "yes"},
		{"appearance.theme_mode", "blue"},
	}
	for _, tc := range cases {
		if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{tc.key: raw(t, tc.val)}); err == nil {
			t.Errorf("key %s accepted invalid value %v", tc.key, tc.val)
		}
	}
}

func TestEmptyFallsBackToDefault(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"terminal.theme":       raw(t, ""),
		"terminal.font_family": raw(t, ""),
		"terminal.font_size":   raw(t, 0),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	tp := s.Terminal("alice")
	if tp.Theme != TermThemeGitHubDark || tp.FontFamily != DefaultUIFontFamily || tp.FontSize != 14 {
		t.Fatalf("empty did not fall back to defaults: %+v", tp)
	}
}

func TestDeleteUser(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"appearance.theme_mode": raw(t, "light"),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	s.DeleteUser("alice")
	if got := s.Appearance("alice").ThemeMode; got != "dark" {
		t.Fatalf("after delete theme_mode = %q, want dark", got)
	}
}

func TestMigrateLegacy(t *testing.T) {
	dir := t.TempDir()
	legacy := map[string]legacyPrefs{
		"alice": {
			Theme:        "Monokai",
			DefaultShell: "bash",
			FontFamily:   "JetBrains Mono",
			FontSize:     16,
			CursorBlink:  false,
			Scrollback:   5000,
		},
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "term_prefs.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	tp := s.Terminal("alice")
	if tp.Theme != "Monokai" || tp.DefaultShell != "bash" || tp.FontFamily != "JetBrains Mono" ||
		tp.FontSize != 16 || tp.CursorBlink || tp.Scrollback != 5000 {
		t.Fatalf("migration mismatch: %+v", tp)
	}
	// Legacy file is renamed, not deleted; new file exists.
	if _, err := os.Stat(filepath.Join(dir, "term_prefs.json")); !os.IsNotExist(err) {
		t.Fatal("term_prefs.json should have been renamed")
	}
	if _, err := os.Stat(filepath.Join(dir, "term_prefs.json.migrated")); err != nil {
		t.Fatal("term_prefs.json.migrated should exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatal("settings.json should exist")
	}
	// Reopening does not migrate again.
	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if s2.Terminal("alice").Theme != "Monokai" {
		t.Fatal("reopen lost migrated values")
	}
}

func TestPhase3Keys(t *testing.T) {
	s := newTestStore(t)
	// Defaults.
	if got := s.Get(ScopeUser, "nobody", "navigation.default_host_tab"); string(got) != `"files"` {
		t.Fatalf("default_host_tab default = %s, want \"files\"", got)
	}
	if got := s.Get(ScopeUser, "nobody", "appearance.show_tips"); string(got) != `true` {
		t.Fatalf("show_tips default = %s, want true", got)
	}
	// Valid values round-trip.
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"navigation.default_host_tab": raw(t, "terminal"),
		"appearance.show_tips":        raw(t, false),
		"files.default_path":          raw(t, "/var/log"),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	// Invalid values rejected.
	for key, val := range map[string]any{
		"navigation.default_host_tab": "market", // removed tab
		"appearance.show_tips":        "yes",
		"files.default_path":          "relative/path",
	} {
		if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{key: raw(t, val)}); err == nil {
			t.Errorf("key %s accepted invalid value %v", key, val)
		}
	}
	// Windows drive paths accepted; empty means OS default.
	for _, p := range []string{`C:\`, `D:/data`} {
		if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{"files.default_path": raw(t, p)}); err != nil {
			t.Errorf("files.default_path rejected %q: %v", p, err)
		}
	}
}

func TestSchemaAndEffective(t *testing.T) {	s := newTestStore(t)
	schema := s.Schema(ScopeUser)
	if len(schema) != len(Definitions) {
		t.Fatalf("schema has %d entries, want %d", len(schema), len(Definitions))
	}
	eff := s.Effective(ScopeUser, "nobody")
	for _, d := range Definitions {
		if d.Scope != ScopeUser {
			continue
		}
		if _, ok := eff[d.Key]; !ok {
			t.Errorf("effective missing key %s", d.Key)
		}
	}
	if len(s.Schema(ScopeSystem)) != 0 {
		t.Fatal("system schema should be empty in Phase 2")
	}
}
