package cert

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// challengeSolver presents ACME challenge responses. DNS-01 (delegated to an
// external dns-mng instance over HTTP) and HTTP-01 (served by a temporary
// :80 listener) implement the same interface, so the issuance flow can pick
// between them — or fall back from one to the other — transparently.
type challengeSolver interface {
	// Type returns the ACME challenge type this solver answers: "dns-01" or "http-01".
	Type() string
	// Present installs the response for one challenge. domain is the
	// identifier value (used to derive _acme-challenge.<domain> for DNS-01;
	// ignored by HTTP-01); token and keyAuthz follow RFC 8555 §8. The
	// returned cleanup removes the installed response.
	Present(domain, token, keyAuthz string) (cleanup func(), err error)
}

// solverSet is the set of usable challenge solvers for one issuance.
type solverSet struct {
	http *http01Challenge
	dns  *dnsMngChallenge
	log  *slog.Logger
}

// http01ListenAddr overrides the HTTP-01 challenge listener address used by
// buildSolvers. It defaults to "" (i.e. ":80", the only port public CAs
// validate http-01 on); tests set it to a loopback address to avoid binding
// the privileged port.
var http01ListenAddr = ""

// ChallengePreference selects which ACME challenge mechanism issuance uses.
// It is surfaced in the UI so users can explicitly pick DNS-01 (via dns-mng)
// instead of relying on the automatic HTTP-01-first fallback.
type ChallengePreference string

const (
	// ChallengeAuto prefers HTTP-01 for plain DNS names and falls back to
	// DNS-01 when HTTP-01 is unavailable or fails (wildcards: DNS-01 only,
	// IP literals: HTTP-01 only).
	ChallengeAuto ChallengePreference = ""
	// ChallengeHTTP01 uses only HTTP-01 (temporary :80 listener).
	ChallengeHTTP01 ChallengePreference = "http-01"
	// ChallengeDNS01 uses only DNS-01 via the configured dns-mng instance.
	ChallengeDNS01 ChallengePreference = "dns-01"
	// ChallengeDNS01Manual uses DNS-01 with manual TXT provisioning: the
	// server computes the required _acme-challenge TXT records and shows
	// them to the administrator, who adds them at their DNS provider and
	// confirms afterwards. No dns-mng integration is needed.
	ChallengeDNS01Manual ChallengePreference = "dns-01-manual"
)

// Valid reports whether the preference value is recognized.
func (c ChallengePreference) Valid() bool {
	switch c {
	case ChallengeAuto, ChallengeHTTP01, ChallengeDNS01, ChallengeDNS01Manual:
		return true
	}
	return false
}

// Label returns the human-readable name for UI display.
func (c ChallengePreference) Label() string {
	switch c {
	case ChallengeHTTP01:
		return "HTTP-01 验证"
	case ChallengeDNS01:
		return "DNS-01 验证"
	case ChallengeDNS01Manual:
		return "DNS-01（手动解析）"
	default:
		return "自动选择（HTTP-01 优先）"
	}
}

// applyPreference returns the solver subset allowed by the preference, or an
// error when the explicitly chosen mechanism is not usable right now.
func (s *solverSet) applyPreference(p ChallengePreference) (*solverSet, error) {
	switch p {
	case ChallengeAuto:
		return s, nil
	case ChallengeHTTP01:
		if s.http == nil {
			return nil, fmt.Errorf("已选择 HTTP-01 验证，但 80 端口无法监听（可能被其他服务占用）")
		}
		return &solverSet{http: s.http, log: s.log}, nil
	case ChallengeDNS01:
		if s.dns == nil {
			return nil, fmt.Errorf("已选择 DNS-01 验证，但证书中心未配置或未启用 dns-mng")
		}
		return &solverSet{dns: s.dns, log: s.log}, nil
	default:
		return nil, fmt.Errorf("未知的验证方式: %q", string(p))
	}
}

// buildSolvers probes which challenge mechanisms are usable right now:
//   - HTTP-01 is available when port 80 can be bound temporarily;
//   - DNS-01 is available when dns-mng is configured and enabled.
//
// The caller must call stop() when done.
func (h *Hub) buildSolvers() *solverSet {
	s := &solverSet{log: h.log}
	if cfg := h.GetConfig(); cfg.Enabled && strings.TrimSpace(cfg.BaseURL) != "" {
		s.dns = &dnsMngChallenge{
			baseURL:  cfg.BaseURL,
			username: cfg.Username,
			password: cfg.Password,
			http:     &http.Client{Timeout: 30 * time.Second},
			log:      h.log,
		}
	}
	hc := &http01Challenge{addr: http01ListenAddr}
	if err := hc.start(); err != nil {
		h.log.Warn("http-01 challenge unavailable", "err", err)
	} else {
		s.http = hc
	}
	return s
}

func (s *solverSet) stop() {
	if s.http != nil {
		s.http.stop()
	}
}

// hasAny reports whether at least one solver is usable.
func (s *solverSet) hasAny() bool { return s.http != nil || s.dns != nil }

// availableTypes lists the challenge types with a usable solver.
func (s *solverSet) availableTypes() []string {
	var out []string
	if s.http != nil {
		out = append(out, "http-01")
	}
	if s.dns != nil {
		out = append(out, "dns-01")
	}
	return out
}

// excluding returns a copy of the set without the given challenge type.
func (s *solverSet) excluding(challengeType string) *solverSet {
	cp := &solverSet{log: s.log}
	if challengeType != "http-01" {
		cp.http = s.http
	}
	if challengeType != "dns-01" {
		cp.dns = s.dns
	}
	return cp
}

// pickSolvers returns candidate solvers for one authorization, in preference
// order. Constraints:
//   - "ip" identifiers can only be validated with HTTP-01 (RFC 8738);
//   - wildcard DNS identifiers ("*.example.com") can only use DNS-01;
//   - plain DNS identifiers prefer HTTP-01 (no external dependency) and fall
//     back to DNS-01 when HTTP-01 is unavailable.
//
// Only solvers whose challenge type was offered by the CA are returned.
func (s *solverSet) pickSolvers(idType, value string, offered map[string]bool) []challengeSolver {
	var order []challengeSolver
	switch {
	case idType == "ip":
		if s.http != nil {
			order = []challengeSolver{s.http}
		}
	case strings.HasPrefix(value, "*."):
		if s.dns != nil {
			order = []challengeSolver{s.dns}
		}
	default:
		if s.http != nil {
			order = append(order, s.http)
		}
		if s.dns != nil {
			order = append(order, s.dns)
		}
	}
	out := make([]challengeSolver, 0, len(order))
	for _, sv := range order {
		if offered[sv.Type()] {
			out = append(out, sv)
		}
	}
	return out
}

// dns01TXTValue computes the DNS-01 TXT record value for a key authorization
// (RFC 8555 §8.4): base64url(sha256(keyAuthorization)).
func dns01TXTValue(keyAuthz string) string {
	sum := sha256.Sum256([]byte(keyAuthz))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// dns01RecordHost derives the TXT record host for an identifier value
// ("_acme-challenge.<domain>"; the "*." prefix of wildcards is stripped
// because the record always lives under the base domain).
func dns01RecordHost(domain string) string {
	domain = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(domain), "."), "*.")
	return "_acme-challenge." + domain
}

// ---- dnsMngChallenge as a challengeSolver ----

// Type implements challengeSolver.
func (d *dnsMngChallenge) Type() string { return "dns-01" }

// Present implements challengeSolver: it computes the DNS-01 TXT value and
// delegates record creation to dns-mng, waiting briefly for propagation.
func (d *dnsMngChallenge) Present(domain, token, keyAuthz string) (func(), error) {
	// keyAuthz = token.thumbprint; TXT value = base64url(sha256(keyAuthz)).
	txtValue := dns01TXTValue(keyAuthz)
	fqdn := dns01RecordHost(domain)
	if err := d.presentTXT(fqdn, txtValue); err != nil {
		return nil, err
	}
	// DNS propagation: dns-mng publishes synchronously to the provider, but
	// authoritative resolvers need some time to catch up.
	time.Sleep(20 * time.Second)
	return func() {
		if err := d.cleanupTXT(fqdn, txtValue); err != nil && d.log != nil {
			d.log.Warn("dns-01 cleanup failed", "fqdn", fqdn, "err", err)
		}
	}, nil
}

// ---- http01Challenge as a challengeSolver ----

// Type implements challengeSolver.
func (h *http01Challenge) Type() string { return "http-01" }

// Present implements challengeSolver: the domain is irrelevant for HTTP-01,
// the token selects the response served under /.well-known/acme-challenge/.
func (h *http01Challenge) Present(domain, token, keyAuthz string) (func(), error) {
	h.present(token, keyAuthz)
	return func() { h.remove(token) }, nil
}

func (h *http01Challenge) remove(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.values, token)
}

// challengeFailedError reports that an authorization failed after the CA was
// asked to validate a challenge served by solverType. A failed challenge
// poisons the authorization, so the caller must start a fresh order (without
// that solver) to fall back.
type challengeFailedError struct {
	solverType string
	identifier string
	err        error
}

func (e *challengeFailedError) Error() string {
	return fmt.Sprintf("%s 验证 %q 失败: %v", e.solverType, e.identifier, e.err)
}

func (e *challengeFailedError) Unwrap() error { return e.err }
