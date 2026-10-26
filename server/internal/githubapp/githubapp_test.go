package githubapp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
)

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func testPrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func TestMintJWTStructure(t *testing.T) {
	tok, err := MintJWT(12345, testPrivateKey(t))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT should have 3 parts, got %d", len(parts))
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(header), `"RS256"`) {
		t.Fatalf("header missing RS256: %s", header)
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
		Iat int64  `json:"iat"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "12345" {
		t.Fatalf("iss = %q, want 12345", claims.Iss)
	}
	if claims.Exp-claims.Iat > 10*60 {
		t.Fatalf("JWT validity %ds exceeds GitHub's 10 minute maximum", claims.Exp-claims.Iat)
	}
	if _, err := MintJWT(1, "not a pem"); err == nil {
		t.Fatal("invalid PEM should fail")
	}
}

func TestVerifySignature(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{"ref":"refs/heads/main"}`)
	// Compute a valid signature the same way GitHub does.
	sig := "sha256=" + hmacHex(secret, body)
	if !VerifySignature(secret, body, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature(secret, body, "sha256=00") {
		t.Fatal("wrong signature accepted")
	}
	if VerifySignature(secret, []byte("tampered"), sig) {
		t.Fatal("tampered body accepted")
	}
	if VerifySignature(secret, body, "") {
		t.Fatal("empty signature accepted")
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/Acme/Web.git":   "acme/web",
		"https://github.com/acme/web":       "acme/web",
		"git@github.com:acme/web.git":       "acme/web",
		"ssh://git@github.com/acme/web.git": "acme/web",
		"acme/web":                          "acme/web",
		"https://gitlab.com/acme/web":       "",
		"not-a-repo":                        "",
		"https://github.com/acme":           "acme",
		"":                                  "",
	}
	for in, want := range cases {
		if got := NormalizeRepoURL(in); got != want {
			t.Errorf("NormalizeRepoURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePushEvent(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main","repository":{"full_name":"Acme/Web"},"installation":{"id":42}}`)
	ev, err := ParsePushEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Branch() != "main" {
		t.Fatalf("branch = %q, want main", ev.Branch())
	}
	if ev.RepoFullName != "acme/web" {
		t.Fatalf("repo = %q, want acme/web", ev.RepoFullName)
	}
	if ev.InstallationID != 42 {
		t.Fatalf("installation = %d, want 42", ev.InstallationID)
	}
	if _, err := ParsePushEvent([]byte(`{"ref":"refs/heads/main"}`)); err == nil {
		t.Fatal("push without repository should fail")
	}
}

func TestBuildManifestMinimalPermissions(t *testing.T) {
	m := BuildManifest("https://panel.example.com:18789", "Watchman-Test-App", "state123")
	perms, ok := m["default_permissions"].(map[string]string)
	if !ok {
		t.Fatal("default_permissions missing")
	}
	if len(perms) != 2 || perms["contents"] != "read" || perms["metadata"] != "read" {
		t.Fatalf("v1 permissions must be exactly contents:read + metadata:read, got %v", perms)
	}
	events, ok := m["default_events"].([]string)
	if !ok || len(events) != 1 || events[0] != "push" {
		t.Fatalf("v1 must subscribe to push only, got %v", m["default_events"])
	}
	hook, ok := m["hook_attributes"].(map[string]any)
	if !ok || hook["url"] != "https://panel.example.com:18789/api/v1/github-app/webhook" {
		t.Fatalf("webhook url wrong: %v", m["hook_attributes"])
	}
	// The CSRF state rides in redirect_url: GitHub appends the code to it,
	// preserving the query string (the submit URL's own query is not
	// guaranteed to survive).
	if m["redirect_url"] != "https://panel.example.com:18789/settings?s=git&github_app_state=state123" {
		t.Fatalf("redirect url wrong: %v", m["redirect_url"])
	}
	// Empty state leaves redirect_url bare.
	m2 := BuildManifest("https://panel.example.com:18789", "Watchman-Test-App", "")
	if m2["redirect_url"] != "https://panel.example.com:18789/settings?s=git" {
		t.Fatalf("empty-state redirect url wrong: %v", m2["redirect_url"])
	}
}

func TestManifestSubmitURL(t *testing.T) {
	if got := ManifestSubmitURL("personal", ""); got != "https://github.com/settings/apps/new" {
		t.Fatalf("personal submit url = %q", got)
	}
	if got := ManifestSubmitURL("organization", "acme"); got != "https://github.com/organizations/acme/settings/apps/new" {
		t.Fatalf("org submit url = %q", got)
	}
}
