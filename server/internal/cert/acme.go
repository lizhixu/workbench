package cert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// marshalJSON/unmarshalJSON are tiny indirections so the hub file does not
// import encoding/json twice with different needs; kept here for symmetry.
func marshalJSON(v any) ([]byte, error)   { return json.MarshalIndent(v, "", "  ") }
func unmarshalJSON(b []byte, v any) error { return json.Unmarshal(b, v) }

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, v := range b {
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return string(out)
}

// identifierType classifies an ACME identifier per RFC 8738: IP literals use
// the "ip" type, everything else is a "dns" name.
func identifierType(v string) string {
	if net.ParseIP(strings.TrimSpace(v)) != nil {
		return "ip"
	}
	return "dns"
}

// acmeError extracts the error detail from an RFC 8555 problem JSON response.
func acmeError(action string, status int, body []byte) error {
	var prob struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &prob) == nil && prob.Detail != "" {
		if prob.Type != "" {
			return fmt.Errorf("%s: status %d (%s): %s", action, status, prob.Type, prob.Detail)
		}
		return fmt.Errorf("%s: status %d: %s", action, status, prob.Detail)
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	if snippet != "" {
		return fmt.Errorf("%s: status %d %s", action, status, snippet)
	}
	return fmt.Errorf("%s: status %d", action, status)
}

// http01Challenge serves ACME HTTP-01 responses for the duration of an
// issuance. Public CAs only validate http-01 on port 80, so the listener is
// bound briefly (challenge lifetime) and released afterwards; it is only
// needed for "ip" identifiers (e.g. Let's Encrypt IP certificates), which
// cannot use DNS-01 without control of reverse DNS.
type http01Challenge struct {
	mu     sync.Mutex
	values map[string]string // token -> keyAuthorization

	// addr is the listen address; defaults to ":80" (the only port public
	// CAs validate http-01 on). Tests may override it.
	addr string

	ln  net.Listener
	srv *http.Server
}

func (h *http01Challenge) start() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ln != nil {
		return nil
	}
	addr := h.addr
	if addr == "" {
		addr = ":80"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("http-01 验证需要临时监听 80 端口: %w", err)
	}
	h.values = make(map[string]string)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/acme-challenge/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
		h.mu.Lock()
		v, ok := h.values[token]
		h.mu.Unlock()
		if !ok || token == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, v)
	})
	h.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	h.ln = ln
	go func() {
		_ = h.srv.Serve(ln) // closed by stop()
	}()
	return nil
}

func (h *http01Challenge) present(token, keyAuthz string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.values == nil {
		h.values = make(map[string]string)
	}
	h.values[token] = keyAuthz
}

func (h *http01Challenge) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.srv != nil {
		_ = h.srv.Close()
		h.srv = nil
	}
	h.ln = nil
	h.values = nil
}

// dnsMngChallenge implements the DNS-01 challenge by delegating TXT record
// creation/removal to a dns-mng instance (sibling project) which already
// knows which provider account owns each domain.
type dnsMngChallenge struct {
	baseURL            string
	username, password string
	http               *http.Client
}

func (d *dnsMngChallenge) call(path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(d.baseURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(d.username, d.password)
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("dns-mng %s: %d %s", path, resp.StatusCode, string(snippet))
	}
	return nil
}

// Present creates the _acme-challenge TXT record for fqdn.
func (d *dnsMngChallenge) Present(fqdn, value string) error {
	return d.call("/api/acme/dns01/present", map[string]any{
		"fqdn":  fqdn,
		"value": value,
		"ttl":   300,
	})
}

// CleanUp removes the TXT record after validation.
func (d *dnsMngChallenge) CleanUp(fqdn, value string) error {
	return d.call("/api/acme/dns01/cleanup", map[string]any{
		"fqdn":  fqdn,
		"value": value,
	})
}

// acmeClient is a minimal RFC 8555 client supporting the DNS-01 and HTTP-01
// flows: account creation, new-order, authorization challenges, finalize,
// download. DNS identifiers use DNS-01 (delegated to dns-mng); IP
// identifiers (RFC 8738) use HTTP-01 served on port 80.
type acmeClient struct {
	directoryURL string
	email        string
	eabKeyID     string
	eabHMACKey   string
	http         *http.Client
	log          *slog.Logger

	// directory cache
	dir map[string]string
	kid string
}

type acmeAccount struct {
	Status string `json:"status"`
	Kid    string `json:"-"`
	Orders string `json:"orders"`
}

func (a *acmeClient) directory() (map[string]string, error) {
	if a.dir != nil {
		return a.dir, nil
	}
	// The directory itself is a plain GET, no signing.
	resp, err := a.http.Get(a.directoryURL)
	if err != nil {
		return nil, fmt.Errorf("acme directory: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("acme directory read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("acme directory: status %d", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("acme directory parse: %w", err)
	}
	// The directory may contain non-string values (e.g. the "meta" object);
	// only the endpoint URLs (strings) are needed.
	dir := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			dir[k] = s
		}
	}
	for _, key := range []string{"newNonce", "newAccount", "newOrder"} {
		if dir[key] == "" {
			return nil, fmt.Errorf("acme directory: missing %q endpoint", key)
		}
	}
	a.dir = dir
	return dir, nil
}

// ensureAccount registers or locates the ACME account for the account key.
func (a *acmeClient) ensureAccount(accountKey crypto.Signer) error {
	dir, err := a.directory()
	if err != nil {
		return err
	}
	thumbprint, err := keyThumbprint(accountKey)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"termsOfServiceAgreed": true,
	}
	// RFC 8555: contact is optional. Let's Encrypt rejects addresses whose
	// domain lacks a valid public suffix (e.g. the historically seeded
	// admin@watchman.local), so omit the field entirely when no usable
	// email is configured instead of sending a placeholder.
	if email := strings.TrimSpace(a.email); email != "" {
		payload["contact"] = []string{"mailto:" + email}
	}
	if a.eabKeyID != "" && a.eabHMACKey != "" {
		eabJWS, err := computeEAB(dir["newAccount"], a.eabKeyID, a.eabHMACKey, accountKey.Public())
		if err != nil {
			return fmt.Errorf("compute EAB: %w", err)
		}
		payload["externalAccountBinding"] = eabJWS
	}
	body, location, status, err := a.jwsRequest(dir["newAccount"], payload, "", accountKey, true)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return acmeError("newAccount", status, body)
	}
	if location == "" {
		return fmt.Errorf("newAccount: missing Location")
	}
	a.kid = location
	_ = thumbprint
	return nil
}

// obtainCertificate runs order -> authorizations -> finalize -> cert.
// identifiers may mix DNS names and IP literals; each authorization picks its
// challenge from the identifier type ("dns" -> DNS-01 via dnsCh, "ip" ->
// HTTP-01 on port 80). dnsCh may be nil when no DNS identifier is present.
func (a *acmeClient) obtainCertificate(accountKey crypto.Signer, identifiers []string, dnsCh *dnsMngChallenge) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	dir, err := a.directory()
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}

	// Identifiers for the order.
	idents := make([]map[string]string, 0, len(identifiers))
	needHTTP01 := false
	for _, d := range identifiers {
		t := identifierType(d)
		if t == "ip" {
			needHTTP01 = true
		}
		idents = append(idents, map[string]string{"type": t, "value": d})
	}

	// 1. New order. The order URL is the Location response header, used to
	// poll status below.
	orderReq := map[string]any{
		"identifiers": idents,
	}
	if needHTTP01 {
		// RFC 8738 / ACME profile draft: Let's Encrypt (and CAs supporting short-lived IP certs)
		// require "profile": "shortlived" for IP address identifiers (see https://letsencrypt.org/docs/profiles/).
		orderReq["profile"] = "shortlived"
	}

	body, orderURL, status, err := a.jwsRequest(dir["newOrder"], orderReq, a.kid, accountKey, false)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("newOrder: %w", err)
	}
	// If CA rejected the profile parameter (e.g. non-Let's Encrypt CA that doesn't support the profile draft),
	// fall back to order without profile.
	if status/100 != 2 && needHTTP01 && strings.Contains(strings.ToLower(string(body)), "unrecognized") && strings.Contains(strings.ToLower(string(body)), "profile") {
		delete(orderReq, "profile")
		body, orderURL, status, err = a.jwsRequest(dir["newOrder"], orderReq, a.kid, accountKey, false)
		if err != nil {
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("newOrder: %w", err)
		}
	}
	if status/100 != 2 {
		return nil, nil, time.Time{}, time.Time{}, "", acmeError("newOrder", status, body)
	}
	var order struct {
		Status         string          `json:"status"`
		Authorizations []string        `json:"authorizations"`
		Finalize       string          `json:"finalize"`
		Certificate    string          `json:"certificate"`
		Error          json.RawMessage `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("newOrder parse: %w", err)
	}
	if order.Finalize == "" || orderURL == "" {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("order missing finalize/location URL")
	}

	// 2. Satisfy each authorization. The HTTP-01 listener is only bound when
	// an IP identifier is present.
	var httpCh *http01Challenge
	if needHTTP01 {
		httpCh = &http01Challenge{}
		if err := httpCh.start(); err != nil {
			return nil, nil, time.Time{}, time.Time{}, "", err
		}
		defer httpCh.stop()
	}
	for _, authzURL := range order.Authorizations {
		if err := a.satisfyAuthorization(authzURL, accountKey, dnsCh, httpCh); err != nil {
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("authorization: %w", err)
		}
	}

	// 3. Poll order until ready.
	deadline := time.Now().Add(5 * time.Minute)
	for order.Status != "ready" {
		if time.Now().After(deadline) {
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("order timeout: %s", order.Status)
		}
		time.Sleep(3 * time.Second)
		body, _, status, err := a.jwsRequest(orderURL, nil, a.kid, accountKey, false)
		if err != nil {
			return nil, nil, time.Time{}, time.Time{}, "", err
		}
		if status/100 != 2 {
			return nil, nil, time.Time{}, time.Time{}, "", acmeError("poll order", status, body)
		}
		if err := json.Unmarshal(body, &order); err != nil {
			return nil, nil, time.Time{}, time.Time{}, "", err
		}
		if order.Status == "invalid" {
			if len(order.Error) > 0 {
				return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("order invalid: %s", string(order.Error))
			}
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("order invalid")
		}
	}

	// 4. CSR with a fresh key.
	csrKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	csrDer, err := makeCSR(csrKey, identifiers)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	csrB64 := base64.RawURLEncoding.EncodeToString(csrDer)

	body, _, status, err = a.jwsRequest(order.Finalize, map[string]any{"csr": csrB64}, a.kid, accountKey, false)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("finalize: %w", err)
	}
	if status/100 != 2 {
		return nil, nil, time.Time{}, time.Time{}, "", acmeError("finalize", status, body)
	}
	var finOrder struct {
		Status      string          `json:"status"`
		Certificate string          `json:"certificate"`
		Error       json.RawMessage `json:"error,omitempty"`
	}
	_ = json.Unmarshal(body, &finOrder)

	// 5. Poll finalize until valid, then download.
	deadline = time.Now().Add(3 * time.Minute)
	certURL := finOrder.Certificate
	for {
		if time.Now().After(deadline) {
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("finalize timeout: %s", finOrder.Status)
		}
		if certURL == "" {
			body, _, status, err := a.jwsRequest(orderURL, nil, a.kid, accountKey, false)
			if err != nil {
				return nil, nil, time.Time{}, time.Time{}, "", err
			}
			if status/100 != 2 {
				return nil, nil, time.Time{}, time.Time{}, "", acmeError("poll finalize", status, body)
			}
			_ = json.Unmarshal(body, &finOrder)
			certURL = finOrder.Certificate
		}
		if certURL != "" && finOrder.Status == "valid" {
			break
		}
		if finOrder.Status == "invalid" {
			if len(finOrder.Error) > 0 {
				return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("finalize invalid: %s", string(finOrder.Error))
			}
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("finalize invalid")
		}
		time.Sleep(3 * time.Second)
	}

	// Download the certificate chain (PEM) via POST-as-GET.
	respBody, err := a.postAsGet(certURL, a.kid, accountKey)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("download cert: %w", err)
	}

	// Parse the first certificate to extract validity + issuer.
	block, rest := pem.Decode(respBody)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("cert download: not PEM")
	}
	first, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("cert parse: %w", err)
	}
	// Re-encode leaf + chain.
	pemBytes := pem.EncodeToMemory(block)
	if len(rest) > 0 {
		pemBytes = append(pemBytes, rest...)
	}
	// Key PEM.
	keyDer, err := x509.MarshalECPrivateKey(csrKey)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})
	return pemBytes, keyPem, first.NotBefore, first.NotAfter, first.Issuer.CommonName, nil
}

// satisfyAuthorization completes one authorization, picking the challenge
// from the identifier type: "ip" -> HTTP-01, anything else -> DNS-01.
func (a *acmeClient) satisfyAuthorization(authzURL string, accountKey crypto.Signer, dnsCh *dnsMngChallenge, httpCh *http01Challenge) error {
	// POST-as-GET the authorization.
	body, err := a.postAsGet(authzURL, a.kid, accountKey)
	if err != nil {
		return fmt.Errorf("authz fetch: %w", err)
	}
	var authz struct {
		Status     string `json:"status"`
		Identifier struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"identifier"`
		Challenges []struct {
			Type  string `json:"type"`
			URL   string `json:"url"`
			Token string `json:"token"`
		} `json:"challenges"`
	}
	if err := json.Unmarshal(body, &authz); err != nil {
		return fmt.Errorf("authz parse: %w", err)
	}
	if authz.Status == "valid" {
		return nil
	}

	thumbprint, err := keyThumbprint(accountKey)
	if err != nil {
		return err
	}

	if authz.Identifier.Type == "ip" {
		return a.satisfyHTTP01(authzURL, accountKey, thumbprint, authz.Challenges, httpCh)
	}
	return a.satisfyDNS01(authzURL, accountKey, thumbprint, authz.Identifier.Value, authz.Challenges, dnsCh)
}

// pollAuthorization waits until the authorization becomes valid.
func (a *acmeClient) pollAuthorization(authzURL string, accountKey crypto.Signer) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("authorization timeout")
		}
		time.Sleep(3 * time.Second)
		body, err := a.postAsGet(authzURL, a.kid, accountKey)
		if err != nil {
			continue
		}
			var st struct {
				Status     string `json:"status"`
				Challenges []struct {
					Type  string          `json:"type"`
					Error json.RawMessage `json:"error,omitempty"`
				} `json:"challenges"`
			}
			if json.Unmarshal(body, &st) == nil {
				switch st.Status {
				case "valid":
					return nil
				case "invalid", "expired":
					var chalErr string
					for _, ch := range st.Challenges {
						if len(ch.Error) > 0 {
							chalErr = fmt.Sprintf(" (%s: %s)", ch.Type, string(ch.Error))
							break
						}
					}
					return fmt.Errorf("authorization %s%s", st.Status, chalErr)
				}
			}
	}
}

// satisfyHTTP01 completes one authorization using HTTP-01 on port 80.
func (a *acmeClient) satisfyHTTP01(authzURL string, accountKey crypto.Signer, thumbprint string, challenges []struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Token string `json:"token"`
}, httpCh *http01Challenge) error {
	if httpCh == nil {
		return fmt.Errorf("http-01 challenge unavailable")
	}
	var chURL, token string
	for _, ch := range challenges {
		if ch.Type == "http-01" {
			chURL, token = ch.URL, ch.Token
			break
		}
	}
	if token == "" {
		return fmt.Errorf("no http-01 challenge offered")
	}

	keyAuthz := token + "." + thumbprint
	httpCh.present(token, keyAuthz)

	if _, _, _, err := a.jwsRequest(chURL, map[string]any{}, a.kid, accountKey, false); err != nil {
		return fmt.Errorf("challenge notify: %w", err)
	}
	return a.pollAuthorization(authzURL, accountKey)
}

// satisfyDNS01 completes one authorization using DNS-01 via dns-mng.
func (a *acmeClient) satisfyDNS01(authzURL string, accountKey crypto.Signer, thumbprint, domain string, challenges []struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Token string `json:"token"`
}, dnsCh *dnsMngChallenge) error {
	if dnsCh == nil {
		return fmt.Errorf("dns-01 challenge unavailable: 证书中心未配置 dns-mng")
	}
	var chURL, token string
	for _, ch := range challenges {
		if ch.Type == "dns-01" {
			chURL, token = ch.URL, ch.Token
			break
		}
	}
	if token == "" {
		return fmt.Errorf("no dns-01 challenge offered")
	}

	// keyAuthz = token.thumbprint; TXT value = base64url(sha256(keyAuthz)).
	keyAuthz := token + "." + thumbprint
	sum := sha256.Sum256([]byte(keyAuthz))
	txtValue := base64.RawURLEncoding.EncodeToString(sum[:])
	fqdn := "_acme-challenge." + domain

	// Create the TXT record and notify the ACME server.
	if err := dnsCh.Present(fqdn, txtValue); err != nil {
		return fmt.Errorf("present TXT: %w", err)
	}
	defer func() {
		if err := dnsCh.CleanUp(fqdn, txtValue); err != nil {
			a.log.Warn("dns-01 cleanup failed", "fqdn", fqdn, "err", err)
		}
	}()

	// DNS propagation: dns-mng publishes synchronously to the provider, but
	// authoritative resolvers need some time to catch up.
	time.Sleep(20 * time.Second)

	if _, _, _, err := a.jwsRequest(chURL, map[string]any{}, a.kid, accountKey, false); err != nil {
		return fmt.Errorf("challenge notify: %w", err)
	}
	return a.pollAuthorization(authzURL, accountKey)
}
