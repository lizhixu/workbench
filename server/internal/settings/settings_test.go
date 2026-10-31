package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		"security.panel_domain",
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
	if sec.PanelDomain != "" || sec.SSLEnabled || sec.ForceHTTPS {
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

	// The retired explicit-address key is no longer in the registry, so an
	// old client still writing it gets a clear error instead of silently
	// storing a setting nothing reads.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"server.public_url": raw(t, "https://panel.example.com"),
	}); err == nil {
		t.Fatal("retired server.public_url key accepted by the registry")
	}

	// Valid update: binding the domain is the only address configuration.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": raw(t, "panel.example.com"),
	}); err != nil {
		t.Fatalf("SetMany valid panel security: %v", err)
	}

	// SSL settings are a separate write (the certificate is chosen after
	// the domain is bound).
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_enabled":  raw(t, true),
		"security.panel_ssl_mode":     raw(t, "custom"),
		"security.panel_ssl_cert_pem": raw(t, "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----"),
		"security.panel_ssl_key_pem":  raw(t, "-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----"),
		"security.panel_force_https":  raw(t, true),
	}); err != nil {
		t.Fatalf("SetMany valid panel ssl settings: %v", err)
	}

	updated := s.PanelSecurity()
	if updated.PanelDomain != "panel.example.com" {
		t.Errorf("panel_domain = %q, want panel.example.com", updated.PanelDomain)
	}
	if !updated.SSLEnabled || !updated.ForceHTTPS {
		t.Errorf("expected bool flags to be true, got %+v", updated)
	}
	if updated.SSLMode != "custom" {
		t.Errorf("ssl_mode = %q, want custom", updated.SSLMode)
	}

	// Re-writing the same domain (case-insensitive) is not a change: the
	// SSL configuration must survive.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": raw(t, "Panel.Example.COM"),
	}); err != nil {
		t.Fatalf("SetMany same domain: %v", err)
	}
	if got := s.PanelSecurity(); !got.SSLEnabled || !got.ForceHTTPS || got.SSLMode != "custom" || got.SSLCertPEM == "" {
		t.Fatalf("same-domain write cleared SSL config: %+v", got)
	}

	// Changing the domain clears the certificate binding, panel SSL and
	// force-HTTPS in the same atomic write: a certificate issued for the
	// old domain must not be served for the new one (it would break TLS
	// and, with force-HTTPS on, lock the administrator out).
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_cert_id": raw(t, "crt_123"),
	}); err != nil {
		t.Fatalf("SetMany cert id: %v", err)
	}
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": raw(t, "new.example.com"),
	}); err != nil {
		t.Fatalf("SetMany domain change: %v", err)
	}
	changed := s.PanelSecurity()
	if changed.PanelDomain != "new.example.com" {
		t.Errorf("panel_domain = %q, want new.example.com", changed.PanelDomain)
	}
	if changed.SSLEnabled || changed.ForceHTTPS {
		t.Errorf("domain change left SSL/force-HTTPS enabled: %+v", changed)
	}
	if changed.SSLMode != "cert_center" || changed.SSLCertID != "" || changed.SSLCertPEM != "" || changed.SSLKeyPEM != "" {
		t.Errorf("domain change did not clear certificate binding: %+v", changed)
	}

	// Unbinding clears SSL as well (the certificate was issued for a
	// domain that is no longer the panel's identity).
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_enabled": raw(t, true),
		"security.panel_force_https": raw(t, true),
	}); err != nil {
		t.Fatalf("SetMany re-enable ssl: %v", err)
	}
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": raw(t, ""),
	}); err != nil {
		t.Fatalf("SetMany unbind: %v", err)
	}
	if got := s.PanelSecurity(); got.PanelDomain != "" || got.SSLEnabled || got.ForceHTTPS {
		t.Errorf("unbind did not clear SSL: %+v", got)
	}
}

func TestSetManyOnChangeHook(t *testing.T) {
	s := newTestStore(t)
	calls := 0
	s.SetOnChange(func() { calls++ })

	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"server.timezone": raw(t, "UTC"),
	}); err != nil {
		t.Fatalf("SetMany timezone: %v", err)
	}
	if calls != 1 {
		t.Fatalf("onChange calls = %d after successful system write, want 1", calls)
	}
	// Failed writes must not fire the hook.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{"nope": raw(t, 1)}); err == nil {
		t.Fatal("expected error for unknown key")
	}
	if calls != 1 {
		t.Fatalf("onChange fired on failed write: calls = %d", calls)
	}
	// User-scope writes do not affect panel-wide state; no hook.
	if err := s.SetMany(ScopeUser, "alice", map[string]json.RawMessage{
		"appearance.theme_mode": raw(t, "light"),
	}); err != nil {
		t.Fatalf("SetMany user theme: %v", err)
	}
	if calls != 1 {
		t.Fatalf("onChange fired on user-scope write: calls = %d", calls)
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

func TestPanelSecurityBaseURL(t *testing.T) {
	cases := []struct {
		name      string
		sec       PanelSecurityConfig
		reqScheme string
		reqHost   string
		want      string
		wantSet   bool
	}{
		{
			name:      "bound domain with ssl beats request host",
			sec:       PanelSecurityConfig{PanelDomain: "panel.example.com", SSLEnabled: true},
			reqScheme: "http", reqHost: "10.0.0.5:18789",
			want: "https://panel.example.com", wantSet: true,
		},
		{
			name:      "force https alone implies https",
			sec:       PanelSecurityConfig{PanelDomain: "panel.example.com", ForceHTTPS: true},
			reqScheme: "http", reqHost: "10.0.0.5:18789",
			want: "https://panel.example.com", wantSet: true,
		},
		{
			name:      "bound domain follows request https behind a TLS proxy",
			sec:       PanelSecurityConfig{PanelDomain: "  panel.example.com  "},
			reqScheme: "https", reqHost: "127.0.0.1:18789",
			want: "https://panel.example.com", wantSet: true,
		},
		{
			name:      "plain bound domain over http stays http",
			sec:       PanelSecurityConfig{PanelDomain: "panel.example.com"},
			reqScheme: "http", reqHost: "10.0.0.5:18789",
			want: "http://panel.example.com", wantSet: true,
		},
		{
			name:      "request fallback marks address as unset",
			sec:       PanelSecurityConfig{},
			reqScheme: "https", reqHost: "1.2.3.4",
			want: "https://1.2.3.4", wantSet: false,
		},
		{
			name:      "request fallback normalises unknown scheme",
			sec:       PanelSecurityConfig{},
			reqScheme: "", reqHost: "127.0.0.1:18789",
			want: "http://127.0.0.1:18789", wantSet: false,
		},
		{
			name:    "no host means no url",
			sec:     PanelSecurityConfig{},
			wantSet: false,
		},
	}
	for _, tc := range cases {
		got, set := tc.sec.BaseURL(tc.reqScheme, tc.reqHost)
		if got != tc.want || set != tc.wantSet {
			t.Errorf("%s: BaseURL = (%q, %v), want (%q, %v)", tc.name, got, set, tc.want, tc.wantSet)
		}
	}
}

func TestStoreBaseURLFromSettings(t *testing.T) {
	s := newTestStore(t)
	// No domain bound: the panel is reached by IP, so the request address is
	// what external parties get.
	if got, set := s.BaseURL("http", "10.0.0.5:18789"); got != "http://10.0.0.5:18789" || set {
		t.Fatalf("default BaseURL = (%q, %v)", got, set)
	}
	// Binding the domain is the single act that changes the public address.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": raw(t, "panel.example.com"),
	}); err != nil {
		t.Fatal(err)
	}
	if got, set := s.BaseURL("http", "10.0.0.5:18789"); got != "http://panel.example.com" || !set {
		t.Fatalf("domain BaseURL = (%q, %v)", got, set)
	}
	// Panel SSL turns the derived address into https.
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_enabled": raw(t, true),
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BaseURL("http", "10.0.0.5:18789"); got != "https://panel.example.com" {
		t.Fatalf("ssl BaseURL = %q", got)
	}
	// Unbinding falls back to the request address (direct IP access).
	if err := s.SetMany(ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": raw(t, ""),
	}); err != nil {
		t.Fatal(err)
	}
	if got, set := s.BaseURL("http", "10.0.0.5:18789"); got != "http://10.0.0.5:18789" || set {
		t.Fatalf("unbound BaseURL = (%q, %v)", got, set)
	}
}

// writeLegacyDocument seeds settings.json the way an older panel wrote it.
func writeLegacyDocument(t *testing.T, dir, legacy string) string {
	t.Helper()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRetiredPublicURLMigratesToPanelDomain pins the upgrade path for panels
// that only ever configured server.public_url: the hostname is folded into the
// bound panel domain once at load, so agent install commands, Git webhooks,
// the version endpoint and upload-restore keep advertising the domain instead
// of degrading to whatever IP a browser used. The retired key is dropped.
func TestRetiredPublicURLMigratesToPanelDomain(t *testing.T) {
	dir := t.TempDir()
	path := writeLegacyDocument(t, dir, `{"system":{"server.public_url":"https://old.example.com/"},"users":{}}`)

	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.PanelSecurity().PanelDomain; got != "old.example.com" {
		t.Fatalf("panel_domain = %q, want old.example.com", got)
	}
	if got, set := s.BaseURL("http", "10.0.0.5:18789"); got != "http://old.example.com" || !set {
		t.Fatalf("BaseURL = (%q, %v), want the migrated domain", got, set)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "server.public_url") {
		t.Fatalf("settings.json kept the retired key: %s", data)
	}
	if !strings.Contains(string(data), "old.example.com") {
		t.Fatalf("settings.json lost the migrated domain: %s", data)
	}

	// Reopening is stable: the retired key is gone and the domain stays bound.
	s2, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.PanelSecurity().PanelDomain; got != "old.example.com" {
		t.Fatalf("reopen panel_domain = %q, want old.example.com", got)
	}
}

// A domain the admin bound explicitly wins over the retired key, which is
// still dropped so the two sources cannot drift apart.
func TestRetiredPublicURLDoesNotOverrideBoundDomain(t *testing.T) {
	dir := t.TempDir()
	writeLegacyDocument(t, dir, `{"system":{"server.public_url":"https://old.example.com","security.panel_domain":"panel.example.com"},"users":{}}`)

	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.PanelSecurity().PanelDomain; got != "panel.example.com" {
		t.Fatalf("panel_domain = %q, want the bound domain", got)
	}
	if _, ok := s.system["server.public_url"]; ok {
		t.Fatal("retired key survived load")
	}
}

// An IP has no domain equivalent, so the retired address is dropped (with a
// warning) and the panel falls back to the inbound request again.
func TestRetiredPublicURLIPIsDropped(t *testing.T) {
	dir := t.TempDir()
	writeLegacyDocument(t, dir, `{"system":{"server.public_url":"https://203.0.113.7:19443"},"users":{}}`)

	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := s.PanelSecurity().PanelDomain; got != "" {
		t.Fatalf("panel_domain = %q, want empty", got)
	}
	if _, ok := s.system["server.public_url"]; ok {
		t.Fatal("retired key survived load")
	}
	if got, set := s.BaseURL("http", "10.0.0.5:18789"); got != "http://10.0.0.5:18789" || set {
		t.Fatalf("BaseURL = (%q, %v), want the request fallback", got, set)
	}
}

// The panel domain is served on the standard ports, so a non-standard port in
// the retired address has no equivalent: the hostname migrates, the port does
// not. Port 80/443 is not reported as dropped because it is the default.
func TestRetiredPublicURLPortHandling(t *testing.T) {
	for _, tc := range []struct {
		legacy string
		domain string
	}{
		{"https://old.example.com:443/", "old.example.com"},
		{"http://old.example.com:80", "old.example.com"},
		{"https://old.example.com:19443", "old.example.com"},
	} {
		dir := t.TempDir()
		writeLegacyDocument(t, dir, `{"system":{"server.public_url":"`+tc.legacy+`"},"users":{}}`)
		s, err := NewStore(dir, nil)
		if err != nil {
			t.Fatalf("NewStore(%s): %v", tc.legacy, err)
		}
		if got := s.PanelSecurity().PanelDomain; got != tc.domain {
			t.Errorf("%s: panel_domain = %q, want %q", tc.legacy, got, tc.domain)
		}
	}
	// Only absolute http(s) URLs are accepted; anything else is dropped.
	for _, rawURL := range []string{"old.example.com", "ftp://old.example.com", "http://"} {
		domain, _ := legacyPanelDomain(rawURL)
		if domain != "" {
			t.Errorf("legacyPanelDomain(%q) = %q, want empty", rawURL, domain)
		}
	}
}
