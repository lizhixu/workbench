package cert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// jwsRequest performs one signed ACME request (RFC 8555 §6.2). A nil payload
// with kid set becomes POST-as-GET. When noKid is true (newAccount), the
// protected header carries the JWK instead of the KID.
func (a *acmeClient) jwsRequest(url string, payload any, kid string, key crypto.Signer, noKid bool) (body []byte, location string, status int, err error) {
	if a.http == nil {
		a.http = &http.Client{Timeout: 30 * time.Second}
	}
	if a.dir == nil {
		if _, err := a.directory(); err != nil {
			return nil, "", 0, err
		}
	}
	nonceReq, err := http.NewRequest(http.MethodHead, a.dir["newNonce"], nil)
	if err != nil {
		return nil, "", 0, err
	}
	nonceResp, err := a.http.Do(nonceReq)
	if err != nil {
		return nil, "", 0, fmt.Errorf("nonce fetch: %w", err)
	}
	nonceResp.Body.Close()
	nonce := nonceResp.Header.Get("Replay-Nonce")
	if nonce == "" {
		return nil, "", 0, fmt.Errorf("no replay nonce from %s", a.dir["newNonce"])
	}

	protected := map[string]any{
		"alg":   "ES256", // account keys are always ECDSA P-256 (see loadOrCreateAccountKey)
		"nonce": nonce,
		"url":   url,
	}
	if noKid {
		jwk, err := jwkJSON(key.Public())
		if err != nil {
			return nil, "", 0, err
		}
		protected["jwk"] = jwk
	} else {
		protected["kid"] = kid
	}

	payloadB64 := ""
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, "", 0, err
		}
		payloadB64 = base64.RawURLEncoding.EncodeToString(b)
	}

	protectedB, err := json.Marshal(protected)
	if err != nil {
		return nil, "", 0, err
	}
	protectedB64 := base64.RawURLEncoding.EncodeToString(protectedB)

	signingInput := protectedB64 + "." + payloadB64
	sig, err := es256Sign(key, signingInput)
	if err != nil {
		return nil, "", 0, err
	}
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	jwsBody, err := json.Marshal(map[string]string{
		"protected": protectedB64,
		"payload":   payloadB64,
		"signature": sigB64,
	})
	if err != nil {
		return nil, "", 0, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(jwsBody))
	if err != nil {
		return nil, "", 0, err
	}
	httpReq.Header.Set("Content-Type", "application/jose+json")
	httpReq.Header.Set("User-Agent", "watchman-cert-hub/1.0")
	httpResp, err := a.http.Do(httpReq)
	if err != nil {
		return nil, "", 0, fmt.Errorf("acme post: %w", err)
	}
	defer httpResp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return nil, "", httpResp.StatusCode, err
	}
	return body, httpResp.Header.Get("Location"), httpResp.StatusCode, nil
}

// postAsGet is the ACME POST-as-GET pattern: an empty-payload signed POST.
func (a *acmeClient) postAsGet(url, kid string, key crypto.Signer) ([]byte, error) {
	body, _, status, err := a.jwsRequest(url, nil, kid, key, false)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		snippet := string(body)
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return nil, fmt.Errorf("post-as-get %s: status %d %s", url, status, snippet)
	}
	return body, nil
}

// es256Sign signs the JWS signing input per RFC 7518 §3.4 (ES256):
// the signature is the raw concatenation R || S, each left-padded to 32
// bytes. Note crypto.Signer.Sign on an ECDSA key returns ASN.1 DER, which
// ACME servers reject, so we encode R and S manually.
func es256Sign(key crypto.Signer, signingInput string) ([]byte, error) {
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("jws: unsupported key type %T (ES256 needs *ecdsa.PrivateKey)", key)
	}
	sum := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, ecKey, sum[:])
	if err != nil {
		return nil, err
	}
	sig := make([]byte, 64)
	rb, sb := r.Bytes(), s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):], sb)
	return sig, nil
}

// jwkJSON renders the JWK for an ECDSA public key (ES256).
func jwkJSON(pub crypto.PublicKey) (map[string]string, error) {
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("jwk: unsupported key type")
	}
	return map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(ecPub.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(ecPub.Y.Bytes()),
	}, nil
}

// keyThumbprint computes the RFC 7638 thumbprint of the account key.
func keyThumbprint(key crypto.Signer) (string, error) {
	jwk, err := jwkJSON(key.Public())
	if err != nil {
		return "", err
	}
	// RFC 7638: lexicographic order of the required members.
	canonical := fmt.Sprintf(`{"crv":"%s","kty":"%s","x":"%s","y":"%s"}`,
		jwk["crv"], jwk["kty"], jwk["x"], jwk["y"])
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// makeCSR builds a DER CSR covering all domains (CN = first domain).
func makeCSR(key *ecdsa.PrivateKey, identifiers []string) ([]byte, error) {
	var dnsNames []string
	var ips []net.IP
	for _, id := range identifiers {
		id = strings.TrimSpace(id)
		if ip := net.ParseIP(id); ip != nil {
			ips = append(ips, ip)
		} else if id != "" {
			dnsNames = append(dnsNames, id)
		}
	}
	cn := ""
	if len(identifiers) > 0 {
		cn = strings.TrimSpace(identifiers[0])
	}
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: cn},
		DNSNames:           dnsNames,
		IPAddresses:        ips,
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	return x509.CreateCertificateRequest(rand.Reader, tmpl, key)
}

// computeEAB builds the RFC 8555 Section 7.3.4 externalAccountBinding object:
// an HS256-signed JWS where the payload is the JWK of the new ACME account key,
// and the key is the MAC key supplied by the CA (Google, ZeroSSL, SSL.com).
func computeEAB(newAccountURL, kid, hmacKeyStr string, pub crypto.PublicKey) (map[string]string, error) {
	macKey, err := decodeHMACKey(hmacKeyStr)
	if err != nil {
		return nil, fmt.Errorf("invalid EAB HMAC key: %w", err)
	}

	jwk, err := jwkJSON(pub)
	if err != nil {
		return nil, err
	}
	payloadJSON, err := json.Marshal(jwk)
	if err != nil {
		return nil, err
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	protected := map[string]string{
		"alg": "HS256",
		"kid": kid,
		"url": newAccountURL,
	}
	protectedJSON, err := json.Marshal(protected)
	if err != nil {
		return nil, err
	}
	protectedB64 := base64.RawURLEncoding.EncodeToString(protectedJSON)

	sigInput := protectedB64 + "." + payloadB64
	mac := hmac.New(sha256.New, macKey)
	mac.Write([]byte(sigInput))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return map[string]string{
		"protected": protectedB64,
		"payload":   payloadB64,
		"signature": sigB64,
	}, nil
}

// decodeHMACKey attempts base64 URL-safe, base64 standard, and raw string fallback.
func decodeHMACKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return b, nil
	}
	// Fallback to literal bytes if not base64
	return []byte(s), nil
}
