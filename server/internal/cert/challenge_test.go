package cert

import (
	"errors"
	"reflect"
	"testing"
)

func stubSolverSet(httpOK, dnsOK bool) *solverSet {
	s := &solverSet{}
	if httpOK {
		s.http = &http01Challenge{}
	}
	if dnsOK {
		s.dns = &dnsMngChallenge{}
	}
	return s
}

func solverTypeNames(solvers []challengeSolver) []string {
	out := make([]string, 0, len(solvers))
	for _, s := range solvers {
		out = append(out, s.Type())
	}
	return out
}

// TestPickSolvers pins the challenge compatibility matrix: DNS-01 (dns-mng)
// and HTTP-01 (:80) are interchangeable where the ACME rules allow, with a
// fixed preference order per identifier kind.
func TestPickSolvers(t *testing.T) {
	both := map[string]bool{"http-01": true, "dns-01": true}
	dnsOfferedOnly := map[string]bool{"dns-01": true}
	cases := []struct {
		name          string
		set           *solverSet
		idType, value string
		offered       map[string]bool
		want          []string
	}{
		{"dns prefers http-01 then dns-01", stubSolverSet(true, true), "dns", "example.com", both, []string{"http-01", "dns-01"}},
		{"dns without http falls back to dns-01", stubSolverSet(false, true), "dns", "example.com", both, []string{"dns-01"}},
		{"dns without dns-mng uses http-01", stubSolverSet(true, false), "dns", "example.com", both, []string{"http-01"}},
		{"wildcard requires dns-01", stubSolverSet(true, true), "dns", "*.example.com", both, []string{"dns-01"}},
		{"wildcard without dns-mng has no solver", stubSolverSet(true, false), "dns", "*.example.com", both, nil},
		{"ip requires http-01", stubSolverSet(true, true), "ip", "1.2.3.4", both, []string{"http-01"}},
		{"ip without http has no solver", stubSolverSet(false, true), "ip", "1.2.3.4", both, nil},
		{"offered challenge types filter solvers", stubSolverSet(true, true), "dns", "example.com", dnsOfferedOnly, []string{"dns-01"}},
		{"no solvers available", stubSolverSet(false, false), "dns", "example.com", both, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := solverTypeNames(tc.set.pickSolvers(tc.idType, tc.value, tc.offered))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pickSolvers = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSolverSetExcluding verifies the order-retry path drops exactly one
// solver type.
func TestSolverSetExcluding(t *testing.T) {
	full := stubSolverSet(true, true)
	withoutHTTP := full.excluding("http-01")
	if withoutHTTP.http != nil || withoutHTTP.dns == nil {
		t.Fatalf("excluding http-01: http=%v dns=%v", withoutHTTP.http != nil, withoutHTTP.dns != nil)
	}
	if !withoutHTTP.hasAny() {
		t.Fatal("excluding http-01 should leave dns-01")
	}
	empty := full.excluding("http-01").excluding("dns-01")
	if empty.hasAny() {
		t.Fatal("excluding both should leave no solver")
	}
	if got := empty.availableTypes(); len(got) != 0 {
		t.Fatalf("availableTypes = %v, want empty", got)
	}
}

// TestHTTP01SolverPresentCleanup exercises the HTTP-01 solver through the
// challengeSolver interface, including per-challenge cleanup.
func TestHTTP01SolverPresentCleanup(t *testing.T) {
	ch := &http01Challenge{addr: "127.0.0.1:0"}
	if err := ch.start(); err != nil {
		t.Skipf("cannot bind test listener: %v", err)
	}
	defer ch.stop()

	var sv challengeSolver = ch
	if sv.Type() != "http-01" {
		t.Fatalf("Type() = %q, want http-01", sv.Type())
	}
	cleanup, err := sv.Present("example.com", "tok123", "tok123.thumb")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ch.ln.Addr().String() + "/.well-known/acme-challenge/tok123"
	if resp, err := httpGet(url); err != nil || resp != "tok123.thumb" {
		t.Fatalf("challenge response = %q, err = %v", resp, err)
	}
	cleanup()
	if _, err := httpGet(url); err == nil {
		t.Fatal("expected 404 after cleanup")
	}
}

// TestDNSSolverType pins the dns-mng solver's challenge type without network.
func TestDNSSolverType(t *testing.T) {
	var sv challengeSolver = &dnsMngChallenge{}
	if sv.Type() != "dns-01" {
		t.Fatalf("Type() = %q, want dns-01", sv.Type())
	}
}

// TestChallengeFailedErrorUnwrap ensures the order-retry loop can detect the
// failed solver through errors.As even when wrapped.
func TestChallengeFailedErrorUnwrap(t *testing.T) {
	inner := &challengeFailedError{solverType: "http-01", identifier: "example.com", err: errors.New("boom")}
	wrapped := &wrappedErr{err: inner}
	var cfe *challengeFailedError
	if !errors.As(wrapped, &cfe) {
		t.Fatal("errors.As did not find *challengeFailedError through wrapper")
	}
	if cfe.solverType != "http-01" {
		t.Fatalf("solverType = %q, want http-01", cfe.solverType)
	}
}

type wrappedErr struct{ err error }

func (e *wrappedErr) Error() string { return "wrapped: " + e.err.Error() }
func (e *wrappedErr) Unwrap() error { return e.err }

// TestApplyPreference pins the explicit challenge selection: the UI lets the
// user pin DNS-01 (dns-mng) or HTTP-01 instead of the automatic fallback,
// and pinning an unusable mechanism fails fast with a clear error.
func TestApplyPreference(t *testing.T) {
	for _, p := range []ChallengePreference{ChallengeAuto, ChallengeHTTP01, ChallengeDNS01, ""} {
		if !p.Valid() {
			t.Fatalf("preference %q should be valid", string(p))
		}
	}
	if ChallengePreference("tls-alpn-01").Valid() {
		t.Fatal("unknown preference should be invalid")
	}

	both := stubSolverSet(true, true)
	if got, err := both.applyPreference(ChallengeAuto); err != nil || got != both {
		t.Fatalf("auto should return the set unchanged: %v %v", got, err)
	}
	if got, err := both.applyPreference(ChallengeHTTP01); err != nil || got.http == nil || got.dns != nil {
		t.Fatalf("http-01 preference should keep only http: %v %v", got, err)
	}
	if got, err := both.applyPreference(ChallengeDNS01); err != nil || got.dns == nil || got.http != nil {
		t.Fatalf("dns-01 preference should keep only dns: %v %v", got, err)
	}
	// Pinning an unavailable mechanism is a fast, explicit error (not a
	// silent fallback).
	if _, err := stubSolverSet(false, true).applyPreference(ChallengeHTTP01); err == nil {
		t.Fatal("http-01 preference without a usable :80 listener should fail")
	}
	if _, err := stubSolverSet(true, false).applyPreference(ChallengeDNS01); err == nil {
		t.Fatal("dns-01 preference without dns-mng should fail")
	}
	if _, err := both.applyPreference(ChallengePreference("bogus")); err == nil {
		t.Fatal("bogus preference should fail")
	}
}
