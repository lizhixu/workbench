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
	"net/http"
	"strings"
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

// acmeClient is a minimal RFC 8555 client supporting the DNS-01 flow:
// account creation, new-order, authorization challenges, finalize, download.
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
	var dir map[string]string
	if err := json.Unmarshal(body, &dir); err != nil {
		return nil, fmt.Errorf("acme directory parse: %w", err)
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
		"contact":              []string{"mailto:" + a.email},
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
	_ = body
	if status/100 != 2 {
		return fmt.Errorf("newAccount: status %d", status)
	}
	if location == "" {
		return fmt.Errorf("newAccount: missing Location")
	}
	a.kid = location
	_ = thumbprint
	return nil
}

// obtainCertificate runs order -> authorizations (DNS-01) -> finalize -> cert.
func (a *acmeClient) obtainCertificate(accountKey crypto.Signer, domains []string, challenger *dnsMngChallenge) (certPEM, keyPEM []byte, notBefore, notAfter time.Time, issuer string, err error) {
	dir, err := a.directory()
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}

	// Identifiers for the order.
	identifiers := make([]map[string]string, 0, len(domains))
	for _, d := range domains {
		identifiers = append(identifiers, map[string]string{"type": "dns", "value": d})
	}

	// 1. New order. The order URL is the Location response header, used to
	// poll status below.
	body, orderURL, status, err := a.jwsRequest(dir["newOrder"], map[string]any{
		"identifiers": identifiers,
	}, a.kid, accountKey, false)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("newOrder: %w", err)
	}
	if status/100 != 2 {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("newOrder: status %d", status)
	}
	var order struct {
		Status         string   `json:"status"`
		Authorizations []string `json:"authorizations"`
		Finalize       string   `json:"finalize"`
		Certificate    string   `json:"certificate"`
	}
	if err := json.Unmarshal(body, &order); err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("newOrder parse: %w", err)
	}
	if order.Finalize == "" || orderURL == "" {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("order missing finalize/location URL")
	}

	// 2. Satisfy each authorization via DNS-01.
	for _, authzURL := range order.Authorizations {
		if err := a.satisfyAuthorization(authzURL, accountKey, challenger); err != nil {
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
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("poll order: status %d", status)
		}
		if err := json.Unmarshal(body, &order); err != nil {
			return nil, nil, time.Time{}, time.Time{}, "", err
		}
		if order.Status == "invalid" {
			return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("order invalid")
		}
	}

	// 4. CSR with a fresh key.
	csrKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	csrDer, err := makeCSR(csrKey, domains)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", err
	}
	csrB64 := base64.RawURLEncoding.EncodeToString(csrDer)

	body, _, _, err = a.jwsRequest(order.Finalize, map[string]any{"csr": csrB64}, a.kid, accountKey, false)
	if err != nil {
		return nil, nil, time.Time{}, time.Time{}, "", fmt.Errorf("finalize: %w", err)
	}
	var finOrder struct {
		Status      string `json:"status"`
		Certificate string `json:"certificate"`
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
			body, _, _, err := a.jwsRequest(orderURL, nil, a.kid, accountKey, false)
			if err != nil {
				return nil, nil, time.Time{}, time.Time{}, "", err
			}
			_ = json.Unmarshal(body, &finOrder)
			certURL = finOrder.Certificate
		}
		if certURL != "" && finOrder.Status == "valid" {
			break
		}
		if finOrder.Status == "invalid" {
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

// satisfyAuthorization completes one authorization using DNS-01.
func (a *acmeClient) satisfyAuthorization(authzURL string, accountKey crypto.Signer, challenger *dnsMngChallenge) error {
	// POST-as-GET the authorization.
	body, err := a.postAsGet(authzURL, a.kid, accountKey)
	if err != nil {
		return fmt.Errorf("authz fetch: %w", err)
	}
	var authz struct {
		Status     string `json:"status"`
		Identifier struct {
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
	var ch *struct {
		Type  string `json:"type"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	for i := range authz.Challenges {
		if authz.Challenges[i].Type == "dns-01" {
			ch = &authz.Challenges[i]
			break
		}
	}
	if ch == nil {
		return fmt.Errorf("no dns-01 challenge offered")
	}

	// keyAuthz = token.thumbprint; TXT value = base64url(sha256(keyAuthz)).
	thumbprint, err := keyThumbprint(accountKey)
	if err != nil {
		return err
	}
	keyAuthz := ch.Token + "." + thumbprint
	sum := sha256.Sum256([]byte(keyAuthz))
	txtValue := base64.RawURLEncoding.EncodeToString(sum[:])
	fqdn := "_acme-challenge." + authz.Identifier.Value

	// Create the TXT record and notify the ACME server.
	if err := challenger.Present(fqdn, txtValue); err != nil {
		return fmt.Errorf("present TXT: %w", err)
	}
	defer func() {
		if err := challenger.CleanUp(fqdn, txtValue); err != nil {
			a.log.Warn("dns-01 cleanup failed", "fqdn", fqdn, "err", err)
		}
	}()

	// DNS propagation: dns-mng publishes synchronously to the provider, but
	// authoritative resolvers need some time to catch up.
	time.Sleep(20 * time.Second)

	if _, _, _, err := a.jwsRequest(ch.URL, map[string]any{}, a.kid, accountKey, false); err != nil {
		return fmt.Errorf("challenge notify: %w", err)
	}

	// Poll the authorization until valid.
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
			Status string `json:"status"`
		}
		if json.Unmarshal(body, &st) == nil {
			switch st.Status {
			case "valid":
				return nil
			case "invalid", "expired":
				return fmt.Errorf("authorization %s", st.Status)
			}
		}
	}
}
