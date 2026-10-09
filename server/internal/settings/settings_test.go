package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
		"terminal.theme":        raw(t, "Dracula"),
		"terminal.font_size":    raw(t, 18),
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

func TestSchemaAndEffective(t *testing.T) {
	s := newTestStore(t)
	schema := s.Schema(ScopeUser)
	want := 0
	for _, d := range Definitions {
		if d.Scope == ScopeUser {
			want++
		}
	}
	if len(schema) != want {
		t.Fatalf("schema has %d entries, want %d", len(schema), want)
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
	wantSys := 0
	for _, d := range Definitions {
		if d.Scope == ScopeSystem {
			wantSys++
		}
	}
	sysSchema := s.Schema(ScopeSystem)
	if len(sysSchema) != wantSys {
		t.Fatalf("system schema has %d entries, want %d", len(sysSchema), wantSys)
	}
	for _, k := range []string{
		"security.secure_entry_enabled", "security.secure_entry_path", "system.join_beta_program",
		"server.public_url", "security.panel_domain",
		"security.panel_ssl_enabled", "security.panel_ssl_mode", "security.panel_ssl_cert_id",
		"security.panel_ssl_cert_pem", "security.panel_ssl_key_pem", "security.panel_force_https",
		"certs.auto_renew_days", "certs.expiry_reminder_days",
	} {
		found := false
		for _, e := range sysSchema {
			if e.Key == k {
				found = true
			}
		}
		if !found {
			t.Errorf("system schema missing key %s", k)
		}
	}
}

func TestSetManyPersistFailureKeepsMemory(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"appearance.theme_mode": raw(t, "light"),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	// Break persistence: point the store at a regular file instead of a
	// directory, so the temp-file write fails (works even as root, where
	// permission bits would not stop the write).
	broken := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(broken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.dataDir = broken
	err = s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"appearance.theme_mode": raw(t, "dark"),
		"terminal.font_size":    raw(t, 18),
	})
	if err == nil {
		t.Fatal("expected persist error")
	}
	// In-memory state must be untouched by the failed write.
	if got := s.Appearance("alice").ThemeMode; got != "light" {
		t.Fatalf("memory changed on failed persist: theme_mode = %q, want light", got)
	}
	if got := s.Terminal("alice").FontSize; got != 14 {
		t.Fatalf("memory changed on failed persist: font_size = %d, want 14", got)
	}
	if keys := s.StoredKeys(ScopeUser, "alice"); len(keys) != 1 || keys[0] != "appearance.theme_mode" {
		t.Fatalf("stored keys changed on failed persist: %v", keys)
	}
	// The on-disk document still holds the first write only.
	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Appearance("alice").ThemeMode; got != "light" {
		t.Fatalf("disk changed on failed persist: theme_mode = %q, want light", got)
	}
	// A retry after the directory is fixed succeeds and swaps the new state in.
	s.dataDir = dir
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"appearance.theme_mode": raw(t, "dark"),
		"terminal.font_size":    raw(t, 18),
	}); err != nil {
		t.Fatalf("retry SetMany: %v", err)
	}
	if got := s.Appearance("alice").ThemeMode; got != "dark" {
		t.Fatalf("retry theme_mode = %q, want dark", got)
	}
	if got := s.Terminal("alice").FontSize; got != 18 {
		t.Fatalf("retry font_size = %d, want 18", got)
	}
}

func TestStoredKeys(t *testing.T) {
	s := newTestStore(t)
	if keys := s.StoredKeys(ScopeUser, "alice"); len(keys) != 0 {
		t.Fatalf("fresh store stored keys = %v, want empty", keys)
	}
	// Effective still reports defaults for keys nobody saved.
	if got := s.Effective(ScopeUser, "alice")["appearance.theme_mode"]; got != "dark" {
		t.Fatalf("effective theme_mode = %v, want dark default", got)
	}
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"terminal.font_size":    raw(t, 18),
		"appearance.theme_mode": raw(t, "light"),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	keys := s.StoredKeys(ScopeUser, "alice")
	if len(keys) != 2 || keys[0] != "appearance.theme_mode" || keys[1] != "terminal.font_size" {
		t.Fatalf("stored keys = %v, want sorted [appearance.theme_mode terminal.font_size]", keys)
	}
	// Other users and scopes are isolated.
	if keys := s.StoredKeys(ScopeUser, "bob"); len(keys) != 0 {
		t.Fatalf("bob stored keys = %v, want empty", keys)
	}
	if keys := s.StoredKeys(ScopeSystem, ""); len(keys) != 0 {
		t.Fatalf("system stored keys = %v, want empty", keys)
	}
}

func TestGetUserExposesStoredKeys(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"appearance.theme_mode": raw(t, "light"),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	h := NewHandlers(s)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("username", "alice")
	h.getUser(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Data       map[string]any `json:"data"`
		StoredKeys []string       `json:"stored_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The default-filled theme must not masquerade as an explicit choice.
	if body.Data["terminal.theme"] == nil {
		t.Fatal("data should include default-filled keys")
	}
	if len(body.StoredKeys) != 1 || body.StoredKeys[0] != "appearance.theme_mode" {
		t.Fatalf("stored_keys = %v, want [appearance.theme_mode]", body.StoredKeys)
	}
	if body.Data["appearance.theme_mode"] != "light" {
		t.Fatalf("data theme_mode = %v, want light", body.Data["appearance.theme_mode"])
	}
}

// TestSnapshotConsistentUnderConcurrency hammers SetMany from several
// goroutines while taking snapshots: every snapshot must be internally
// consistent — a key listed in stored_keys must carry its stored value in
// data, never a default from another SetMany generation. This nails down the
// reason Snapshot exists instead of calling Effective + StoredKeys separately.
func TestSnapshotConsistentUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				v := "dark"
				if (i+w)%2 == 1 {
					v = "light"
				}
				_ = s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
					"appearance.theme_mode": raw(t, v),
				})
			}
		}(w)
	}
	for i := 0; i < 2000; i++ {
		data, stored := s.Snapshot(ScopeUser, "alice")
		listed := false
		for _, k := range stored {
			if k == "appearance.theme_mode" {
				listed = true
			}
		}
		if listed {
			v, ok := data["appearance.theme_mode"].(string)
			if !ok || (v != "dark" && v != "light") {
				t.Fatalf("torn snapshot: stored_keys lists theme_mode but data has %v", data["appearance.theme_mode"])
			}
		}
	}
		close(stop)
		wg.Wait()
	}

	func TestPanelSecuritySettings(t *testing.T) {
		s := newTestStore(t)
		// Check defaults
		sec := s.PanelSecurity()
		if sec.PublicURL != "" || sec.PanelDomain != "" || sec.SSLEnabled || sec.ForceHTTPS {
			t.Fatalf("unexpected defaults: %+v", sec)
		}
		if sec.SSLMode != "cert_center" {
			t.Fatalf("unexpected default ssl_mode: %q", sec.SSLMode)
		}

		// Validation errors
		badDomain := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
			"security.panel_domain": raw(t, "http://invalid-domain.com:18789"),
		})
		if badDomain == nil {
			t.Fatal("expected error on invalid panel domain containing scheme/port")
		}

		badURL := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
			"server.public_url": raw(t, "ftp://bad-url"),
		})
		if badURL == nil {
			t.Fatal("expected error on invalid public URL")
		}

		// Valid update
		if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
			"server.public_url":            raw(t, "https://panel.example.com:18789"),
			"security.panel_domain":        raw(t, "panel.example.com"),
			"security.panel_ssl_enabled":   raw(t, true),
			"security.panel_ssl_mode":      raw(t, "custom"),
			"security.panel_ssl_cert_pem":  raw(t, "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----"),
			"security.panel_ssl_key_pem":   raw(t, "-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----"),
			"security.panel_force_https":   raw(t, true),
		}); err != nil {
			t.Fatalf("SetMany valid panel security: %v", err)
		}

		updated := s.PanelSecurity()
		if updated.PublicURL != "https://panel.example.com:18789" {
			t.Errorf("public_url = %q, want https://panel.example.com:18789", updated.PublicURL)
		}
		if updated.PanelDomain != "panel.example.com" {
			t.Errorf("panel_domain = %q, want panel.example.com", updated.PanelDomain)
		}
		if !updated.SSLEnabled || !updated.ForceHTTPS {
			t.Errorf("expected bool flags to be true, got %+v", updated)
		}
		if updated.SSLMode != "custom" {
			t.Errorf("ssl_mode = %q, want custom", updated.SSLMode)
		}
	}

func TestCertTimingKeys(t *testing.T) {
	s := newTestStore(t)

	// Defaults apply before anything is stored.
	if got := s.CertAutoRenewDays(); got != 30 {
		t.Errorf("default CertAutoRenewDays = %d, want 30", got)
	}
	if got := s.CertExpiryReminderDays(); got != 30 {
		t.Errorf("default CertExpiryReminderDays = %d, want 30", got)
	}

	// Valid values round-trip through the registry.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"certs.auto_renew_days":      raw(t, 45),
		"certs.expiry_reminder_days": raw(t, 7),
	}); err != nil {
		t.Fatalf("SetMany valid cert timing keys: %v", err)
	}
	if got := s.CertAutoRenewDays(); got != 45 {
		t.Errorf("CertAutoRenewDays = %d, want 45", got)
	}
	if got := s.CertExpiryReminderDays(); got != 7 {
		t.Errorf("CertExpiryReminderDays = %d, want 7", got)
	}

	// Out-of-range values are rejected by validation. (0 is not rejected:
	// DefaultOnEmpty normalizes it to the default, matching terminal.font_size.)
	for _, tc := range []struct {
		key string
		val any
	}{
		{"certs.auto_renew_days", 91},
		{"certs.auto_renew_days", -5},
		{"certs.expiry_reminder_days", 91},
		{"certs.expiry_reminder_days", "30"},
	} {
		if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{tc.key: raw(t, tc.val)}); err == nil {
			t.Errorf("key %s accepted invalid value %v", tc.key, tc.val)
		}
	}
	// 0 normalizes to the default rather than erroring.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{"certs.auto_renew_days": raw(t, 0)}); err != nil {
		t.Fatalf("SetMany 0: %v", err)
	}
	if got := s.CertAutoRenewDays(); got != 30 {
		t.Errorf("0 normalized to %d, want default 30", got)
	}

	// User scope is rejected: these are admin-only system keys.
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{"certs.auto_renew_days": raw(t, 10)}); err == nil {
		t.Error("certs.auto_renew_days accepted in user scope")
	}

	// A hand-edited settings.json bypassing validation must not produce a
	// nonsensical threshold: accessors clamp back to the default.
	s.system["certs.auto_renew_days"] = raw(t, 500)
	s.system["certs.expiry_reminder_days"] = raw(t, -3)
	if got := s.CertAutoRenewDays(); got != 30 {
		t.Errorf("clamped CertAutoRenewDays = %d, want 30", got)
	}
	if got := s.CertExpiryReminderDays(); got != 30 {
		t.Errorf("clamped CertExpiryReminderDays = %d, want 30", got)
	}
}

func TestTimezoneValidationAndApply(t *testing.T) {
	s := newTestStore(t)
	// Other tests share this process: never leak a mutated time.Local.
	t.Cleanup(func() { time.Local = osLocal })

	// Unset by default: panel follows the server OS timezone.
	if got := s.Timezone(); got != "" {
		t.Fatalf("default timezone = %q, want empty", got)
	}

	// A bogus IANA name is rejected (Go would silently fall back to UTC).
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{"server.timezone": raw(t, "Mars/Olympus")}); err == nil {
		t.Fatal("invalid timezone name accepted")
	}
	// System scope only: user-scope writes are refused.
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{"server.timezone": raw(t, "UTC")}); err == nil {
		t.Fatal("server.timezone accepted in user scope")
	}

	// A valid name is stored and ApplyTimezone points time.Local at it.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{"server.timezone": raw(t, "Asia/Shanghai")}); err != nil {
		t.Fatalf("valid timezone rejected: %v", err)
	}
	if got := s.Timezone(); got != "Asia/Shanghai" {
		t.Fatalf("timezone = %q, want Asia/Shanghai", got)
	}
	s.ApplyTimezone()
	if time.Local.String() != "Asia/Shanghai" {
		t.Fatalf("time.Local = %v, want Asia/Shanghai", time.Local)
	}

	// Clearing the setting restores the OS zone captured at init.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{"server.timezone": raw(t, "")}); err != nil {
		t.Fatalf("clearing timezone: %v", err)
	}
	s.ApplyTimezone()
	if time.Local != osLocal {
		t.Fatalf("time.Local = %v, want OS zone %v", time.Local, osLocal)
	}
}
