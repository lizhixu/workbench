package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"io"
	"net/http"
	"testing"
	"time"
)

func httpGet(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", io.EOF
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return string(b), err
}

func TestIdentifierType(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"panel.example.com", "dns"},
		{"*.example.com", "dns"},
		{"1.2.3.4", "ip"},
		{" 1.2.3.4 ", "ip"},
		{"2001:db8::1", "ip"},
		{"localhost", "dns"},
		{"", "dns"},
	}
	for _, c := range cases {
		if got := identifierType(c.in); got != c.want {
			t.Errorf("identifierType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMakeCSRWithIP(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := makeCSR(key, []string{"203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.IPAddresses) != 1 || req.IPAddresses[0].String() != "203.0.113.10" {
		t.Errorf("IPAddresses = %v, want [203.0.113.10]", req.IPAddresses)
	}
	if len(req.DNSNames) != 0 {
		t.Errorf("DNSNames = %v, want empty", req.DNSNames)
	}
	if err := req.CheckSignature(); err != nil {
		t.Errorf("CSR signature invalid: %v", err)
	}
}

func TestMakeCSRMixed(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := makeCSR(key, []string{"panel.example.com", "203.0.113.10"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.DNSNames) != 1 || req.DNSNames[0] != "panel.example.com" {
		t.Errorf("DNSNames = %v, want [panel.example.com]", req.DNSNames)
	}
	if len(req.IPAddresses) != 1 || req.IPAddresses[0].String() != "203.0.113.10" {
		t.Errorf("IPAddresses = %v, want [203.0.113.10]", req.IPAddresses)
	}
}

func TestHTTP01ChallengeServe(t *testing.T) {
	ch := &http01Challenge{addr: "127.0.0.1:0"}
	if err := ch.start(); err != nil {
		t.Skipf("cannot bind test listener: %v", err)
	}
	defer ch.stop()

	ch.present("test-token", "test-token.thumbprint")
	url := "http://" + ch.ln.Addr().String() + "/.well-known/acme-challenge/test-token"
	resp, err := httpGet(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp != "test-token.thumbprint" {
		t.Errorf("challenge response = %q, want keyAuthorization", resp)
	}
	// Unknown token -> 404.
	if _, err := httpGet("http://" + ch.ln.Addr().String() + "/.well-known/acme-challenge/nope"); err == nil {
		t.Error("expected error for unknown token")
	}
}
