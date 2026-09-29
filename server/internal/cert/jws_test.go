package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fake ACME server: directory + newNonce + newAccount. The newAccount handler
// validates the JWS the way Boulder does: protected must carry alg ES256 and
// the signature must be raw R||S verifiable with the embedded JWK.
func TestJWSRequestNewAccountShape(t *testing.T) {
	var (
		protectedB64 string
		payloadB64   string
		sigB64       string
		jwkX, jwkY   string
	)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/directory", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"newNonce":   srv.URL + "/new-nonce",
			"newAccount": srv.URL + "/new-account",
			"newOrder":   srv.URL + "/new-order",
		})
	})
	mux.HandleFunc("/new-nonce", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "test-nonce-123")
	})
	mux.HandleFunc("/new-account", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Protected string `json:"protected"`
			Payload   string `json:"payload"`
			Signature string `json:"signature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode jws: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		protectedB64, payloadB64, sigB64 = body.Protected, body.Payload, body.Signature
		protJSON, _ := base64.RawURLEncoding.DecodeString(body.Protected)
		var prot map[string]any
		_ = json.Unmarshal(protJSON, &prot)
		if prot["alg"] != "ES256" {
			t.Errorf("protected alg = %v, want ES256", prot["alg"])
		}
		if prot["nonce"] != "test-nonce-123" {
			t.Errorf("protected nonce = %v", prot["nonce"])
		}
		if prot["url"] != srv.URL+"/new-account" {
			t.Errorf("protected url = %v", prot["url"])
		}
		jwk, _ := prot["jwk"].(map[string]any)
		if jwk == nil {
			t.Errorf("newAccount JWS must embed jwk, got %v", prot["jwk"])
		} else {
			jwkX, _ = jwk["x"].(string)
			jwkY, _ = jwk["y"].(string)
		}
		w.Header().Set("Location", srv.URL+"/acct/1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	// Point the client at the fake directory.
	c := &acmeClient{directoryURL: srv.URL + "/directory", http: srv.Client()}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ensureAccount(key); err != nil {
		t.Fatalf("ensureAccount: %v", err)
	}
	if c.kid != srv.URL+"/acct/1" {
		t.Fatalf("kid = %q", c.kid)
	}

	// The signature must be 64-byte raw R||S and verify under ES256.
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("signature len = %d, want 64 (raw R||S); ASN.1 DER would be ~70-72", len(sig))
	}
	xb, _ := base64.RawURLEncoding.DecodeString(jwkX)
	yb, _ := base64.RawURLEncoding.DecodeString(jwkY)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}
	sum := sha256.Sum256([]byte(protectedB64 + "." + payloadB64))
	if !ecdsa.Verify(pub, sum[:],
		new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("ES256 signature does not verify against the embedded JWK")
	}
}

func TestES256SignRejectsNonECDSA(t *testing.T) {
	if _, err := es256Sign(fakeSigner{}, "x"); err == nil || !strings.Contains(err.Error(), "unsupported key type") {
		t.Fatalf("want unsupported key type error, got %v", err)
	}
}

type fakeSigner struct{}

func (fakeSigner) Public() crypto.PublicKey { return nil }
func (fakeSigner) Sign(_ io.Reader, _ []byte, _ crypto.SignerOpts) ([]byte, error) {
	return nil, nil
}
