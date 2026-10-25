package registry

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubLookup replaces the DNS resolver for one test; the restore runs via
// Cleanup so a failure cannot leak the stub into other tests.
func stubLookup(t *testing.T, ips map[string][]string, errHost string) {
	t.Helper()
	old := lookupIP
	t.Cleanup(func() { lookupIP = old })
	lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		if host == errHost {
			return nil, &net.DNSError{IsNotFound: true, Name: host}
		}
		var out []net.IP
		for _, s := range ips[host] {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"93.184.216.34", true},
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.9", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // cloud metadata endpoint
		{"100.64.0.1", false},      // CGNAT shared space
		{"0.0.0.0", false},
		{"::1", false},
		{"fe80::1", false},
		{"fc00::1", false},
		{"192.0.2.1", false}, // TEST-NET-1
		{"224.0.0.1", false}, // multicast
		{"2606:4700:4700::1111", true},
	}
	for _, c := range cases {
		if got := isPublicIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isPublicIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestValidatePublicHost(t *testing.T) {
	stubLookup(t, map[string][]string{
		"public.example.com": {"93.184.216.34"},
		"evil.example.com":   {"10.0.0.5"},
		"mixed.example.com":  {"93.184.216.34", "192.168.0.1"},
	}, "missing.example.com")

	cases := []struct {
		hostport string
		wantErr  bool
	}{
		{"8.8.8.8:443", false},
		{"[2606:4700:4700::1111]:443", false},
		{"10.1.2.3:5000", true},  // private literal
		{"127.0.0.1:5000", true}, // loopback literal
		{"public.example.com:443", false},
		{"evil.example.com:443", true},  // DNS resolves private
		{"mixed.example.com:443", true}, // any private answer fails closed
		{"missing.example.com:443", true},
	}
	for _, c := range cases {
		err := validatePublicHost(context.Background(), c.hostport)
		if c.wantErr && err == nil {
			t.Errorf("validatePublicHost(%q) = nil, want error", c.hostport)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validatePublicHost(%q) = %v, want nil", c.hostport, err)
		}
	}
}

func TestCheckRedirect(t *testing.T) {
	stubLookup(t, map[string][]string{
		"blob.core.windows.net": {"52.239.0.1"}, // ghcr-style CDN hop
		"evil.example.com":      {"10.0.0.5"},
	}, "")

	c := &Client{}
	mkReq := func(rawurl string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, rawurl, nil)
		return req
	}
	via := []*http.Request{mkReq("https://ghcr.io/v2/x/blobs/sha256:cfg")}

	// Legitimate CDN redirect stays allowed.
	if err := c.checkRedirect(mkReq("https://blob.core.windows.net/container/blob"), via); err != nil {
		t.Errorf("CDN redirect should be allowed, got %v", err)
	}
	// Redirect to a host resolving to a private IP is refused.
	if err := c.checkRedirect(mkReq("https://evil.example.com/blob"), via); err == nil {
		t.Error("redirect to private-resolving host should be refused")
	}
	// Redirect to a private literal is refused.
	if err := c.checkRedirect(mkReq("http://192.168.1.1/blob"), via); err == nil {
		t.Error("redirect to private literal should be refused")
	}
	// Default 10-redirect cap is preserved.
	longVia := make([]*http.Request, 10)
	for i := range longVia {
		longVia[i] = via[0]
	}
	if err := c.checkRedirect(mkReq("https://blob.core.windows.net/blob"), longVia); err == nil {
		t.Error("11th redirect should hit the cap")
	}
	// Test-only opt-out skips validation (mock servers run on loopback).
	c.allowPrivateHosts = true
	if err := c.checkRedirect(mkReq("http://127.0.0.1/blob"), via); err != nil {
		t.Errorf("allowPrivateHosts should skip redirect validation, got %v", err)
	}
}

func TestSafeDialContextRejectsPrivateLiteral(t *testing.T) {
	c := &Client{}
	// No network is touched: a private literal is refused before dialing.
	if _, err := c.safeDialContext(context.Background(), "tcp", "10.1.2.3:443"); err == nil {
		t.Error("dial to private literal should be refused")
	} else if !strings.Contains(err.Error(), "拒绝连接") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, err := c.safeDialContext(context.Background(), "tcp", "169.254.169.254:80"); err == nil {
		t.Error("dial to cloud metadata address should be refused")
	}
}

func TestFetchTokenRejectsPrivateRealm(t *testing.T) {
	stubLookup(t, map[string][]string{
		"evil.example.com": {"10.0.0.5"},
	}, "")
	c := &Client{}
	// Literal private realm: refused before any HTTP request is attempted.
	if _, err := c.fetchToken(context.Background(), &authChallenge{Realm: "http://10.1.2.3/token"}); err == nil {
		t.Error("private literal realm should be refused")
	}
	// Hostname realm resolving to a private IP: refused as well.
	if _, err := c.fetchToken(context.Background(), &authChallenge{Realm: "https://evil.example.com/token"}); err == nil {
		t.Error("private-resolving realm should be refused")
	}
}
