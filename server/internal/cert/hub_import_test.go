package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// makeTestCertPEM generates a self-signed certificate + matching key.
func makeTestCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "example.com"},
		DNSNames:              []string{"example.com", "*.example.com"},
		IPAddresses:           []net.IP{net.ParseIP("10.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDer, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})
	return certPEM, keyPEM
}

// TestImportStoresAndLists verifies a manually uploaded certificate lands in
// the hub and shows up in the issued list (this is what the panel's custom
// upload and the cert-center upload both rely on).
func TestImportStoresAndLists(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := makeTestCertPEM(t)
	c, err := h.Import(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !strings.HasPrefix(c.ID, "crt_") {
		t.Fatalf("ID = %q, want crt_ prefix", c.ID)
	}
	for _, want := range []string{"example.com", "*.example.com", "10.0.0.1"} {
		found := false
		for _, d := range c.Domains {
			if d == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Domains = %v, missing %q", c.Domains, want)
		}
	}
	if c.AutoRenew {
		t.Error("imported certificate must not auto-renew via ACME")
	}
	if c.AccountID != "" {
		t.Errorf("AccountID = %q, want empty for imported cert", c.AccountID)
	}

	found := false
	for _, e := range h.List() {
		if e.ID == c.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("imported certificate missing from List()")
	}
	got, ok := h.Get(c.ID)
	if !ok || got.CertPEM == "" || got.KeyPEM == "" {
		t.Fatal("Get() did not return stored PEM material")
	}

	// Survives a hub reopen (index + files on disk).
	h2, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h2.Get(c.ID); !ok {
		t.Fatal("imported certificate missing after hub reopen")
	}
}

func TestImportRejects(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHub(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _ := makeTestCertPEM(t)
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherDer, err := x509.MarshalECPrivateKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	otherKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: otherDer})

	cases := []struct {
		name    string
		certPEM []byte
		keyPEM  []byte
	}{
		{"empty", nil, nil},
		{"garbage", []byte("not a pem"), []byte("not a pem")},
		{"mismatched key", certPEM, otherKeyPEM},
		{"cert only", certPEM, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.Import(tc.certPEM, tc.keyPEM); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
