package ws

import (
	"net/http"
	"testing"
)

func req(origin, host string) *http.Request {
	r, _ := http.NewRequest("GET", "http://"+host+"/api/v1/ws/terminal/s1", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestCheckOrigin(t *testing.T) {
	SetAllowedOrigins(nil)

	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"same origin", "https://watchman.example.com", "watchman.example.com", true},
		{"same origin with port", "http://10.0.0.5:18080", "10.0.0.5:18080", true},
		{"no origin header (non-browser)", "", "watchman.example.com", true},
		{"cross site attacker", "https://evil.example.net", "watchman.example.com", false},
		{"attacker suffix trick", "https://watchman.example.com.evil.net", "watchman.example.com", false},
		{"unparsable origin", "not a url", "watchman.example.com", false},
		{"null origin (sandboxed iframe)", "null", "watchman.example.com", false},
		{"vite dev proxy", "http://localhost:5173", "localhost:18080", true},
		{"loopback ip dev", "http://127.0.0.1:5173", "127.0.0.1:18080", true},
		{"remote page to loopback server", "https://evil.example.net", "127.0.0.1:18080", false},
		{"loopback page to remote server", "http://localhost:5173", "watchman.example.com", false},
	}
	for _, tc := range cases {
		if got := checkOrigin(req(tc.origin, tc.host)); got != tc.want {
			t.Errorf("%s: origin=%q host=%q got %v want %v", tc.name, tc.origin, tc.host, got, tc.want)
		}
	}
}

func TestCheckOriginAllowList(t *testing.T) {
	SetAllowedOrigins([]string{"https://console.example.com", " ops.example.com:8443 "})
	defer SetAllowedOrigins(nil)

	if !checkOrigin(req("https://console.example.com", "watchman.example.com")) {
		t.Error("full-URL allow-list entry should match by host")
	}
	if !checkOrigin(req("https://ops.example.com:8443", "watchman.example.com")) {
		t.Error("host:port allow-list entry should match")
	}
	if checkOrigin(req("https://other.example.com", "watchman.example.com")) {
		t.Error("origin outside the allow list must be rejected")
	}

	SetAllowedOrigins([]string{"*"})
	if !checkOrigin(req("https://evil.example.net", "watchman.example.com")) {
		t.Error(`"*" must disable the check`)
	}
}
