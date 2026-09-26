package prefs

import (
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

func TestGetReturnsDefaultsForUnknownUser(t *testing.T) {
	s := newTestStore(t)
	got := s.Get("alice")
	want := Defaults("alice")
	if got.Theme != want.Theme || got.FontSize != want.FontSize ||
		got.FontFamily != want.FontFamily || got.Scrollback != want.Scrollback {
		t.Fatalf("defaults not returned: %+v", got)
	}
	if got.Username != "alice" {
		t.Errorf("username = %q, want alice", got.Username)
	}
}

func TestSetGetRoundTripAndIsolation(t *testing.T) {
	s := newTestStore(t)

	in := Prefs{Theme: ThemeDracula, DefaultShell: ShellPowerShell, FontSize: 18, Scrollback: 5000}
	saved, err := s.Set("alice", in)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if saved.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not stamped")
	}
	// An omitted font family must be filled from defaults, not stored empty.
	if saved.FontFamily != Defaults("").FontFamily {
		t.Errorf("FontFamily = %q, want default", saved.FontFamily)
	}

	got := s.Get("alice")
	if got.Theme != ThemeDracula || got.DefaultShell != ShellPowerShell || got.FontSize != 18 || got.Scrollback != 5000 {
		t.Errorf("round trip mismatch: %+v", got)
	}
	// Another user must not inherit alice's preferences.
	if other := s.Get("bob"); other.Theme != Defaults("").Theme {
		t.Errorf("bob got alice's theme: %+v", other)
	}
}

func TestPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := s1.Set("alice", Prefs{Theme: ThemeMonokai, FontSize: 12}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reload NewStore: %v", err)
	}
	got := s2.Get("alice")
	if got.Theme != ThemeMonokai || got.FontSize != 12 {
		t.Errorf("after reload: %+v", got)
	}
	if _, err := filepath.Abs(filepath.Join(dir, "term_prefs.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSetRejectsInvalidInput(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name string
		in   Prefs
	}{
		{"unknown theme", Prefs{Theme: "Neon"}},
		{"unknown shell", Prefs{DefaultShell: "fish"}},
		{"font too small", Prefs{FontSize: MinFontSize - 1}},
		{"font too large", Prefs{FontSize: MaxFontSize + 1}},
		{"scrollback too small", Prefs{Scrollback: MinScrollback - 1}},
		{"scrollback too large", Prefs{Scrollback: MaxScrollback + 1}},
		{"css injection in font", Prefs{FontFamily: "x; } body{display:none"}},
	}
	for _, tc := range cases {
		if _, err := s.Set("alice", tc.in); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
	if _, err := s.Set("", Prefs{}); err == nil {
		t.Error("empty username: expected error, got nil")
	}
}

func TestDelete(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Set("alice", Prefs{Theme: ThemeMiku}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	s.Delete("alice")
	if got := s.Get("alice"); got.Theme != Defaults("").Theme {
		t.Errorf("after delete: %+v", got)
	}
	// Deleting a user with no saved prefs must be a no-op, not a panic.
	s.Delete("nobody")
}
