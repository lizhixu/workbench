package panelsec

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/server/internal/settings"
)

func newTestSettings(t *testing.T) *settings.Store {
	t.Helper()
	s, err := settings.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func generateTestCertPEM(t *testing.T, domain string) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: domain,
		},
		DNSNames:  []string{domain},
		NotBefore: time.Now().Add(-1 * time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	certBuf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}
	keyBuf := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	return string(certBuf), string(keyBuf)
}

func TestDomainMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := newTestSettings(t)
	guard := NewGuard(st, nil)

	r := gin.New()
	r.Use(guard.DomainMiddleware())
	r.GET("/test", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/api/v1/hosts/enroll", func(c *gin.Context) { c.String(http.StatusOK, "enroll-ok") })

	// When strict domain is disabled:
	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "192.168.1.100:18789"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disabled: got status %d, want 200", w.Code)
	}

	// Bind a domain: strict domain check is now automatic (no separate switch)
	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_domain": rawJSON(t, "panel.example.com"),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}

	// 1. Direct IP access -> 403 Forbidden
	req = httptest.NewRequest("GET", "/test", nil)
	req.Host = "1.2.3.4:18789"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("direct IP: got status %d, want 403", w.Code)
	}

	// 2. Bound domain access -> 200 OK
	req = httptest.NewRequest("GET", "/test", nil)
	req.Host = "panel.example.com:18789"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("bound domain: got status %d, want 200", w.Code)
	}

	// 3. Localhost / loopback -> 200 OK (maintenance exception)
	req = httptest.NewRequest("GET", "/test", nil)
	req.Host = "127.0.0.1:18789"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback IP: got status %d, want 200", w.Code)
	}

	// 4. Exempt path on direct IP -> 200 OK
	req = httptest.NewRequest("GET", "/api/v1/hosts/enroll", nil)
	req.Host = "1.2.3.4:18789"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("exempt path on direct IP: got status %d, want 200", w.Code)
	}

	// 5. Bracketed IPv6 loopback without port -> 200 OK (maintenance exception)
	for _, h := range []string{"[::1]", "[::1]:18789", "localhost", "localhost:18789"} {
		req = httptest.NewRequest("GET", "/test", nil)
		req.Host = h
		w = httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("loopback host %q: got status %d, want 200", h, w.Code)
		}
	}
}

func TestHTTPSMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := newTestSettings(t)
	guard := NewGuard(st, nil)

	r := gin.New()
	r.Use(guard.HTTPSMiddleware())
	r.GET("/dashboard", func(c *gin.Context) { c.String(http.StatusOK, "dashboard-ok") })

	// Disabled -> plain HTTP passes
	req := httptest.NewRequest("GET", "/dashboard", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ssl disabled: got status %d, want 200", w.Code)
	}

	// Enable SSL & force HTTPS
	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_enabled": rawJSON(t, true),
		"security.panel_force_https": rawJSON(t, true),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}

	// Plaintext HTTP request -> 301 Redirect to https://
	req = httptest.NewRequest("GET", "/dashboard?tab=files", nil)
	req.Host = "panel.example.com:18789"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("plain HTTP when force https is on: got status %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "https://panel.example.com:18789/dashboard?tab=files" {
		t.Fatalf("Location = %q, want https://panel.example.com:18789/dashboard?tab=files", loc)
	}

	// HTTPS request with X-Forwarded-Proto -> 200 OK
	req = httptest.NewRequest("GET", "/dashboard", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("https header: got status %d, want 200", w.Code)
	}
}

func TestCertProviderCustomPEM(t *testing.T) {
	st := newTestSettings(t)
	certPEM, keyPEM := generateTestCertPEM(t, "panel.example.com")

	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_enabled":  rawJSON(t, true),
		"security.panel_ssl_mode":     rawJSON(t, "custom"),
		"security.panel_ssl_cert_pem": rawJSON(t, certPEM),
		"security.panel_ssl_key_pem":  rawJSON(t, keyPEM),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}

	cp := NewCertProvider(st, nil, nil, nil)
	cert, err := cp.GetCertificate(&tls.ClientHelloInfo{ServerName: "panel.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("empty certificate chain")
	}

	summary, err := cp.InspectActiveCert()
	if err != nil {
		t.Fatalf("InspectActiveCert: %v", err)
	}
	if summary.Subject != "panel.example.com" {
		t.Fatalf("Subject = %q, want panel.example.com", summary.Subject)
	}
	if !summary.Valid {
		t.Fatal("certificate should be valid")
	}
}

func TestDynamicListenerAcceptsPlaintextAndTLS(t *testing.T) {
	st := newTestSettings(t)
	certPEM, keyPEM := generateTestCertPEM(t, "127.0.0.1")

	if err := st.SetMany(settings.ScopeSystem, "", map[string]json.RawMessage{
		"security.panel_ssl_enabled":  rawJSON(t, true),
		"security.panel_ssl_mode":     rawJSON(t, "custom"),
		"security.panel_ssl_cert_pem": rawJSON(t, certPEM),
		"security.panel_ssl_key_pem":  rawJSON(t, keyPEM),
	}); err != nil {
		t.Fatalf("SetMany: %v", err)
	}

	cp := NewCertProvider(st, nil, nil, nil)
	tlsCfg := &tls.Config{
		GetCertificate: cp.GetCertificate,
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	dynListener := NewDynamicListener(l, tlsCfg, st, nil)

	// Test 1: Plaintext HTTP write and read
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := dynListener.Accept()
		if err != nil {
			t.Errorf("Accept plaintext: %v", err)
			return
		}
		defer conn.Close()

		buf := make([]byte, 4)
		_, err = io.ReadFull(conn, buf)
		if err != nil || string(buf) != "PING" {
			t.Errorf("read: got %q, err %v", string(buf), err)
			return
		}
		_, _ = conn.Write([]byte("PONG"))
	}()

	clientConn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_, _ = clientConn.Write([]byte("PING"))
	resp := make([]byte, 4)
	_, _ = io.ReadFull(clientConn, resp)
	clientConn.Close()
	if string(resp) != "PONG" {
		t.Fatalf("plaintext resp = %q, want PONG", string(resp))
	}
	<-done

	// Test 2: TLS Client connects over DynamicListener
	tlsDone := make(chan struct{})
	go func() {
		defer close(tlsDone)
		conn, err := dynListener.Accept()
		if err != nil {
			t.Errorf("Accept TLS: %v", err)
			return
		}
		defer conn.Close()

		buf := make([]byte, 11)
		_, err = io.ReadFull(conn, buf)
		if err != nil || string(buf) != "SECURE_PING" {
			t.Errorf("tls read: got %q, err %v", string(buf), err)
			return
		}
		_, _ = conn.Write([]byte("SECURE_PONG"))
	}()

	tlsClientConn, err := tls.Dial("tcp", l.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("TLS Dial: %v", err)
	}
	_, _ = tlsClientConn.Write([]byte("SECURE_PING"))
	tlsResp := make([]byte, 11)
	_, _ = io.ReadFull(tlsClientConn, tlsResp)
	tlsClientConn.Close()
	if string(tlsResp) != "SECURE_PONG" {
		t.Fatalf("tls resp = %q, want SECURE_PONG", string(tlsResp))
	}
	<-tlsDone
}
