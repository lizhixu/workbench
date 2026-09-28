package apps

import (
	"encoding/base64"
	"strings"
	"testing"

	"watchman/server/internal/network"
)

// TestParseIPv4 extracts the first valid IPv4 from command output.
func TestParseIPv4(t *testing.T) {
	cases := []struct {
		out  string
		want string
	}{
		{"100.64.0.1\n", "100.64.0.1"},
		{"  100.64.0.1  \n", "100.64.0.1"},
		{"some header\n100.64.0.2 extra\n", "100.64.0.2"},
		{"fd7a:115c::1\n100.64.0.3\n", "100.64.0.3"}, // IPv6 skipped
		{"fd7a:115c::1\n", ""},                       // IPv6 only -> no IPv4
		{"", ""},
		{"not an ip\n", ""},
		{"999.999.999.999\n", ""},
	}
	for _, c := range cases {
		if got := parseIPv4(c.out); got != c.want {
			t.Errorf("parseIPv4(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

// TestPickMeshIP: live probe wins; cache is only a fallback when the probe
// fails; empty means fall back to public binding.
func TestPickMeshIP(t *testing.T) {
	cached := network.NodeStatus{HostID: "h1", IP: "100.64.0.9"}

	// Live probe wins, no warning.
	if ip, warn := pickMeshIP("100.64.0.1", cached, true); ip != "100.64.0.1" || warn != "" {
		t.Errorf("live should win: ip=%q warn=%q", ip, warn)
	}

	// Live overrides even a different cached IP (stale cache case).
	if ip, _ := pickMeshIP("100.64.0.5", cached, true); ip != "100.64.0.5" {
		t.Errorf("stale cache must be overridden: ip=%q", ip)
	}

	// Probe failed -> cache fallback with warning.
	ip, warn := pickMeshIP("", cached, true)
	if ip != "100.64.0.9" {
		t.Errorf("cache fallback failed: ip=%q", ip)
	}
	if warn == "" || !strings.Contains(warn, "100.64.0.9") {
		t.Errorf("cache fallback must warn with the cached IP: warn=%q", warn)
	}

	// Probe failed, cache entry missing -> public fallback with warning.
	ip, warn = pickMeshIP("", network.NodeStatus{}, false)
	if ip != "" {
		t.Errorf("expected empty ip for public fallback, got %q", ip)
	}
	if warn == "" {
		t.Error("public fallback must warn")
	}

	// Probe failed, cache entry exists but has no IP -> public fallback.
	ip, warn = pickMeshIP("", network.NodeStatus{HostID: "h1"}, true)
	if ip != "" || warn == "" {
		t.Errorf("expected public fallback, got ip=%q warn=%q", ip, warn)
	}
}

// TestGitAuthArgs ensures the token is carried as an http.extraHeader Basic
// credential and never embedded into a clone/fetch URL (which git would
// persist into the agent's .git/config).
func TestGitAuthArgs(t *testing.T) {
	if got := gitAuthArgs(""); got != "" {
		t.Errorf("gitAuthArgs(\"\") = %q, want empty", got)
	}
	const token = "ghp_testtoken123"
	got := gitAuthArgs(token)
	if !strings.HasPrefix(got, "-c 'http.extraHeader=Authorization: Basic ") {
		t.Errorf("gitAuthArgs(%q) = %q, want -c http.extraHeader fragment", token, got)
	}
	if strings.Contains(got, "https://") || strings.Contains(got, "@") {
		t.Errorf("gitAuthArgs(%q) = %q, must not look like a URL", token, got)
	}
	// The base64 payload must decode to "git:<token>" (same semantics as the
	// old token-in-URL approach).
	b64 := strings.TrimSuffix(strings.TrimPrefix(got, "-c 'http.extraHeader=Authorization: Basic "), "'")
	dec, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(dec) != "git:"+token {
		t.Errorf("decoded header = %q, want %q", dec, "git:"+token)
	}
}

// TestShellQuoteSmoke guards the quoting helper used for every
// user-controlled value interpolated into remote shell commands.
func TestShellQuoteSmoke(t *testing.T) {
	cases := map[string]string{
		"simple":                  "'simple'",
		"with space":              "'with space'",
		"a'b":                     "'a'\\''b'",
		"nginx:alpine":            "'nginx:alpine'",
		"JAVA_OPTS=-Xmx1g -Xms1g": "'JAVA_OPTS=-Xmx1g -Xms1g'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
