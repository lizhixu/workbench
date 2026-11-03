package cert

import (
	"net/http"
	"strings"
	"sync"
)

// Shared HTTP-01 challenge answering.
//
// Public CAs only validate http-01 on port 80, so a challenge response must be
// reachable there. Two serving modes cover the two owners of :80:
//   - the default: nothing else holds :80, and the issuance flow binds its own
//     temporary listener for the challenge lifetime (http01Challenge.start);
//   - a panel with a bound domain listens on :80 itself (panelsec standard
//     ports, "绑定即监听 :80"). The temporary bind then always fails, and the
//     panel's own HTTP stack answers /.well-known/acme-challenge/<token> from
//     this shared registry instead — the same pattern Caddy/nginx use when
//     they own port 80.
//
// buildSolvers consults the availability probe (SetSharedHTTP01Probe) so it
// keeps the http-01 solver usable when the shared stack is serving :80 even
// though the temporary bind failed; without that, every issuance on a
// domain-bound panel would be forced onto DNS-01 and fail outright when
// dns-mng is not configured.

var sharedHTTP01 struct {
	mu     sync.Mutex
	values map[string]string // challenge token -> key authorization
	probe  func() bool        // reports whether an external stack serves :80
}

// SetSharedHTTP01Probe installs the availability probe buildSolvers consults:
// it must report whether the panel's own HTTP stack is currently serving port
// 80 (the primary listener on :80, or the standard-ports reconciler holding
// it). The wiring lives in the api package so this package stays free of the
// panelsec dependency.
func SetSharedHTTP01Probe(fn func() bool) {
	sharedHTTP01.mu.Lock()
	defer sharedHTTP01.mu.Unlock()
	sharedHTTP01.probe = fn
}

// sharedHTTP01Serving reports whether an external stack is currently serving
// the shared challenge answers on :80.
func sharedHTTP01Serving() bool {
	sharedHTTP01.mu.Lock()
	fn := sharedHTTP01.probe
	sharedHTTP01.mu.Unlock()
	return fn != nil && fn()
}

func setSharedHTTP01Token(token, keyAuthz string) {
	sharedHTTP01.mu.Lock()
	defer sharedHTTP01.mu.Unlock()
	if sharedHTTP01.values == nil {
		sharedHTTP01.values = make(map[string]string)
	}
	sharedHTTP01.values[token] = keyAuthz
}

func clearSharedHTTP01Token(token string) {
	sharedHTTP01.mu.Lock()
	defer sharedHTTP01.mu.Unlock()
	delete(sharedHTTP01.values, token)
}

// ServeSharedHTTP01 answers a GET /.well-known/acme-challenge/<token> request
// from the shared registry and reports whether it handled it; callers fall
// through to their normal handler otherwise. Registered by the panel's HTTP
// stack ahead of the security guards: the CA's challenge fetch is an
// infrastructure path — it is always plain HTTP and carries whatever Host the
// validated identifier resolves to (an IP literal for IP certificates), so it
// must not be domain-gated, HTTPS-redirected or hidden behind the secure
// entry. Read-only and stateless, exempt from the operation audit (§7, same
// example as agent enroll / webhooks).
func ServeSharedHTTP01(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !strings.HasPrefix(r.URL.Path, "/.well-known/acme-challenge/") {
		return false
	}
	token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
	if token == "" || strings.Contains(token, "/") {
		return false
	}
	sharedHTTP01.mu.Lock()
	v, ok := sharedHTTP01.values[token]
	sharedHTTP01.mu.Unlock()
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(v))
	return true
}