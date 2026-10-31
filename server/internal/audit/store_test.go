package audit

import (
	"net/http/httptest"
	"testing"
)

func TestRequestInfoIgnoresForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest("GET", "http://10.0.0.5:18789/api/v1/version", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	req.Header.Set("X-Real-IP", "1.2.3.4")
	req.Header.Set("X-Forwarded-For", "5.6.7.8, 9.10.11.12")
	req.Header.Set("User-Agent", "test-agent/1.0")

	ip, ua := RequestInfo(req)
	if ip != "203.0.113.7" {
		t.Fatalf("IP = %q, want 203.0.113.7 (forwarded headers must not override RemoteAddr)", ip)
	}
	if ua != "test-agent/1.0" {
		t.Fatalf("User-Agent = %q, want test-agent/1.0", ua)
	}
}

func TestRequestInfoWithoutRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "http://10.0.0.5:18789/api/v1/version", nil)
	req.RemoteAddr = ""
	req.Header.Set("X-Forwarded-For", "5.6.7.8")

	ip, _ := RequestInfo(req)
	if ip != "" {
		t.Fatalf("IP = %q, want empty (no RemoteAddr, forwarded header alone must not supply an IP)", ip)
	}
}
