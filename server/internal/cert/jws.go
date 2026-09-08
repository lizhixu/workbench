package cert

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := key.Sign(rand.Reader, sum[:], crypto.SHA256)
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
func makeCSR(key *ecdsa.PrivateKey, domains []string) ([]byte, error) {
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: domains[0]},
		DNSNames:           domains,
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	return x509.CreateCertificateRequest(rand.Reader, tmpl, key)
}
