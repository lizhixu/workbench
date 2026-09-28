package network

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestHandlers(t *testing.T) *Handlers {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	store, err := NewStore(dir, log)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	return NewHandlers(store, nil, nil)
}

// TestBuildJoinArgsQuotesInjection: user-controlled join parameters must be
// shell-quoted — an unquoted --hostname='x; rm -rf / #' would execute
// arbitrary commands on the remote host under a benign "join" audit record.
func TestBuildJoinArgsQuotesInjection(t *testing.T) {
	args := buildJoinArgs("bash", "tskey-auth-abc123", "https://hs.example.com", true,
		"10.0.0.0/8; evil", false, "web; rm -rf / #", false)
	cmd := strings.Join(args, " ")

	// Both hostile values must appear only inside single quotes.
	if !strings.Contains(cmd, "'web; rm -rf / #'") {
		t.Errorf("hostname not properly quoted: %q", cmd)
	}
	if !strings.Contains(cmd, "'10.0.0.0/8; evil'") {
		t.Errorf("advertise-routes not properly quoted: %q", cmd)
	}
	// After removing the quoted segments, no shell metacharacter may remain.
	rest := cmd
	for _, q := range []string{"'web; rm -rf / #'", "'10.0.0.0/8; evil'"} {
		rest = strings.Replace(rest, q, "", 1)
	}
	if strings.ContainsAny(rest, ";|&<>$`") {
		t.Fatalf("unquoted shell metacharacter remains: %q", rest)
	}
	// Values without special characters stay unquoted (byte-identical to before).
	for _, want := range []string{"--authkey=tskey-auth-abc123", "--login-server=https://hs.example.com"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("expected %q in %q", want, cmd)
		}
	}
	if !strings.Contains(cmd, "--accept-routes=true") || !strings.Contains(cmd, "--accept-dns=false") {
		t.Errorf("expected route/dns flags in %q", cmd)
	}
}

// TestBuildJoinArgsResetOptIn: --reset must never appear unless explicitly requested.
func TestBuildJoinArgsResetOptIn(t *testing.T) {
	plain := strings.Join(buildJoinArgs("bash", "k", "", true, "", false, "", false), " ")
	if strings.Contains(plain, "--reset") {
		t.Errorf("--reset must not be present unless explicitly requested: %q", plain)
	}
	withReset := strings.Join(buildJoinArgs("bash", "k", "", true, "", false, "", true), " ")
	if !strings.Contains(withReset, "--reset") {
		t.Errorf("--reset missing when requested: %q", withReset)
	}
}

func TestShellQuoteArg(t *testing.T) {
	if got := shellQuoteArg("bash", "plain-host"); got != "plain-host" {
		t.Errorf("plain value must stay unquoted, got %q", got)
	}
	if got := shellQuoteArg("bash", "o'clock"); got != "'o'\\''clock'" {
		t.Errorf("posix quoting wrong: %q", got)
	}
	// PowerShell single-quoted strings escape a quote by doubling it.
	if got := shellQuoteArg("powershell", "o'clock"); got != "'o''clock'" {
		t.Errorf("powershell quoting wrong: %q", got)
	}
	if got := shellQuoteArg("powershell", "a; b"); got != "'a; b'" {
		t.Errorf("powershell quoting wrong: %q", got)
	}
}

// TestGetConfigMasksAuthKey: the enrollment credential must never be echoed
// back — this endpoint is reachable by any authenticated user, including
// read-only viewers.
func TestGetConfigMasksAuthKey(t *testing.T) {
	h := newTestHandlers(t)
	if err := h.store.UpdateConfig(NetworkConfig{
		ControlPlane: "headscale",
		ServerURL:    "https://hs.example.com",
		AuthKey:      "tskey-auth-SUPERSECRET",
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/network/config", nil)
	h.getConfig(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if key, _ := resp.Data["auth_key"].(string); key != "" {
		t.Errorf("auth_key leaked in GET response: %q", key)
	}
	if set, _ := resp.Data["auth_key_set"].(bool); !set {
		t.Errorf("auth_key_set should be true, got %v", resp.Data["auth_key_set"])
	}
	if resp.Data["server_url"] != "https://hs.example.com" {
		t.Errorf("non-secret fields must stay visible: %v", resp.Data)
	}
}

// TestUpdateConfigKeepsKeyWhenEmpty: since GET never reveals the key, the
// frontend resubmits "" when the operator didn't change it — that must not
// wipe the stored credential.
func TestUpdateConfigKeepsKeyWhenEmpty(t *testing.T) {
	h := newTestHandlers(t)
	if err := h.store.UpdateConfig(NetworkConfig{ControlPlane: "headscale", AuthKey: "tskey-auth-ORIGINAL"}); err != nil {
		t.Fatal(err)
	}

	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/network/config", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.updateConfig(c)
		return w
	}

	w := put(`{"control_plane":"headscale","server_url":"https://hs.example.com","auth_key":"","accept_routes":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := h.store.GetConfig().AuthKey; got != "tskey-auth-ORIGINAL" {
		t.Errorf("stored key was wiped/changed: %q", got)
	}
	// Response must be masked too.
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if key, _ := resp.Data["auth_key"].(string); key != "" {
		t.Errorf("auth_key leaked in PUT response: %q", key)
	}

	// A non-empty value rotates the key.
	w2 := put(`{"control_plane":"headscale","auth_key":"tskey-auth-NEW"}`)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d", w2.Code)
	}
	if got := h.store.GetConfig().AuthKey; got != "tskey-auth-NEW" {
		t.Errorf("key rotation failed, got %q", got)
	}
}
