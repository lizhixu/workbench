package cert

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBuildDNSQuery(t *testing.T) {
	fqdn := "_acme-challenge.example.com."
	buf, id, err := buildDNSQuery(fqdn, false)
	if err != nil {
		t.Fatalf("buildDNSQuery: %v", err)
	}
	if len(buf) < 12+len(fqdn)+4 {
		t.Fatalf("query packet too short: %d bytes", len(buf))
	}
	gotID := binary.BigEndian.Uint16(buf[0:2])
	if gotID != id {
		t.Errorf("got ID %d, want %d", gotID, id)
	}
	flags := binary.BigEndian.Uint16(buf[2:4])
	if flags != 0 {
		t.Errorf("flags = %x, want 0 (RD=0)", flags)
	}
	qdCount := binary.BigEndian.Uint16(buf[4:6])
	if qdCount != 1 {
		t.Errorf("QDCOUNT = %d, want 1", qdCount)
	}

	// Verify QTYPE=16 and QCLASS=1 at the tail
	qtype := binary.BigEndian.Uint16(buf[len(buf)-4 : len(buf)-2])
	qclass := binary.BigEndian.Uint16(buf[len(buf)-2:])
	if qtype != 16 || qclass != 1 {
		t.Errorf("QTYPE=%d, QCLASS=%d, want 16 and 1", qtype, qclass)
	}
}

func TestParseDNSTXTResponse(t *testing.T) {
	// Construct a synthetic RFC 1035 DNS response with a single TXT answer.
	fqdn := "_acme-challenge.example.com"
	req, id, err := buildDNSQuery(fqdn, false)
	if err != nil {
		t.Fatal(err)
	}

	txtValue := "test-verification-token-12345"
	resp := make([]byte, len(req))
	copy(resp, req)
	// Flags: QR=1 (response), AA=1 (authoritative) -> 0x8400
	binary.BigEndian.PutUint16(resp[2:4], 0x8400)
	// ANCOUNT = 1
	binary.BigEndian.PutUint16(resp[6:8], 1)

	// Answer RR:
	// Name: pointer to QNAME at offset 12 -> 0xC00C
	var ans []byte
	ans = append(ans, 0xC0, 0x0C)
	// TYPE: TXT (16), CLASS: IN (1)
	var typeClass [4]byte
	binary.BigEndian.PutUint16(typeClass[0:2], 16)
	binary.BigEndian.PutUint16(typeClass[2:4], 1)
	ans = append(ans, typeClass[:]...)
	// TTL: 60s
	var ttl [4]byte
	binary.BigEndian.PutUint32(ttl[:], 60)
	ans = append(ans, ttl[:]...)
	// RDLENGTH: 1 byte len + len(txtValue)
	var rdlen [2]byte
	binary.BigEndian.PutUint16(rdlen[:], uint16(1+len(txtValue)))
	ans = append(ans, rdlen[:]...)
	// RDATA: character-string
	ans = append(ans, byte(len(txtValue)))
	ans = append(ans, txtValue...)

	resp = append(resp, ans...)

	txts, err := parseDNSTXTResponse(resp, id)
	if err != nil {
		t.Fatalf("parseDNSTXTResponse: %v", err)
	}
	if len(txts) != 1 || txts[0] != txtValue {
		t.Fatalf("got TXT %v, want [%q]", txts, txtValue)
	}

	// Mismatched ID should fail
	if _, err := parseDNSTXTResponse(resp, id+1); err == nil {
		t.Error("expected error on ID mismatch")
	}

	// Truncated packet should fail
	if _, err := parseDNSTXTResponse(resp[:8], id); err == nil {
		t.Error("expected error on truncated packet")
	}
}

func TestQueryAuthoritativeServerUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer pc.Close()

	wantTXT := "auth-ns-txt-token"
	go func() {
		buf := make([]byte, 512)
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 12 {
			return
		}
		// Send response with wantTXT matching request ID
		resp := make([]byte, n)
		copy(resp, buf[:n])
		binary.BigEndian.PutUint16(resp[2:4], 0x8400) // QR=1, AA=1
		binary.BigEndian.PutUint16(resp[6:8], 1)      // ANCOUNT=1

		var ans []byte
		ans = append(ans, 0xC0, 0x0C) // pointer to question
		var tc [4]byte
		binary.BigEndian.PutUint16(tc[0:2], 16)
		binary.BigEndian.PutUint16(tc[2:4], 1)
		ans = append(ans, tc[:]...)
		var ttl [4]byte
		binary.BigEndian.PutUint32(ttl[:], 60)
		ans = append(ans, ttl[:]...)
		var rdlen [2]byte
		binary.BigEndian.PutUint16(rdlen[:], uint16(1+len(wantTXT)))
		ans = append(ans, rdlen[:]...)
		ans = append(ans, byte(len(wantTXT)))
		ans = append(ans, wantTXT...)
		resp = append(resp, ans...)

		_, _ = pc.WriteTo(resp, addr)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	txts, err := queryAuthoritativeServer(ctx, pc.LocalAddr().String(), "_acme-challenge.example.com")
	if err != nil {
		t.Fatalf("queryAuthoritativeServer: %v", err)
	}
	if len(txts) != 1 || txts[0] != wantTXT {
		t.Fatalf("txts = %v, want [%s]", txts, wantTXT)
	}
}

// stubTestResolver replaces net.DefaultResolver with one that fails every
// lookup instantly, making DNS-01 propagation tests deterministic on any
// machine (no dependence on the host's DNS or network reachability). Raw UDP
// queries via queryAuthoritativeServer bypass the resolver and are unaffected.
func stubTestResolver(t *testing.T) {
	t.Helper()
	oldResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("resolver stubbed for test")
		},
	}
	t.Cleanup(func() { net.DefaultResolver = oldResolver })
}

// serveTXT answers every TXT query arriving on pc with a single TXT record
// holding value.
func serveTXT(pc net.PacketConn, value string) {
	buf := make([]byte, 512)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		resp := make([]byte, n)
		copy(resp, buf[:n])
		binary.BigEndian.PutUint16(resp[2:4], 0x8400) // QR=1, AA=1
		binary.BigEndian.PutUint16(resp[6:8], 1)      // ANCOUNT=1
		var ans []byte
		ans = append(ans, 0xC0, 0x0C) // pointer to the question name
		var tc [4]byte
		binary.BigEndian.PutUint16(tc[0:2], 16) // TXT
		binary.BigEndian.PutUint16(tc[2:4], 1)  // IN
		ans = append(ans, tc[:]...)
		var ttl [4]byte
		binary.BigEndian.PutUint32(ttl[:], 60)
		ans = append(ans, ttl[:]...)
		var rdlen [2]byte
		binary.BigEndian.PutUint16(rdlen[:], uint16(1+len(value)))
		ans = append(ans, rdlen[:]...)
		ans = append(ans, byte(len(value)))
		ans = append(ans, value...)
		resp = append(resp, ans...)
		_, _ = pc.WriteTo(resp, addr)
	}
}

func TestVerifyDNSTXTPropagationTimeout(t *testing.T) {
	stubTestResolver(t)
	oldTimeout := dns01PropagationCheckTimeout
	oldInterval := dns01PropagationCheckInterval
	oldMinWait := dns01PropagationMinWait
	oldSchedule := dns01PollSchedule
	dns01PropagationCheckTimeout = 200 * time.Millisecond
	dns01PropagationCheckInterval = 10 * time.Millisecond
	dns01PropagationMinWait = 0
	dns01PollSchedule = func(time.Duration) time.Duration { return 10 * time.Millisecond }
	t.Cleanup(func() {
		dns01PropagationCheckTimeout = oldTimeout
		dns01PropagationCheckInterval = oldInterval
		dns01PropagationMinWait = oldMinWait
		dns01PollSchedule = oldSchedule
	})

	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	err := verifyDNSTXTPropagation(context.Background(), logf, "nonexistent-domain-for-propagation-test.invalid", "want-val")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "超时") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want timeout explanation", err.Error())
	}
	if !strings.Contains(err.Error(), "共探测") {
		t.Errorf("error = %q, want attempt count in timeout message", err.Error())
	}
	// The check must visibly poll: several attempts, each logged, before
	// giving up — not one silent 90-second hang.
	if len(lines) < 3 {
		t.Errorf("got %d progress lines, want every polling attempt logged: %v", len(lines), lines)
	}
}

// TestQueryAuthoritativeServerQueryBounded regression-tests the single-query
// deadline: with a long-lived context (as the propagation flow uses), one
// unanswered UDP query must cost dns01QueryTimeout — not the whole context.
func TestQueryAuthoritativeServerQueryBounded(t *testing.T) {
	oldQuery := dns01QueryTimeout
	dns01QueryTimeout = 50 * time.Millisecond
	t.Cleanup(func() { dns01QueryTimeout = oldQuery })

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer pc.Close()
	// Read and never reply: the query hangs until the read deadline.
	go func() {
		buf := make([]byte, 512)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := queryAuthoritativeServer(ctx, pc.LocalAddr().String(), "_acme-challenge.example.com"); err == nil {
		t.Fatal("expected read timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("query took %s, want bounded by dns01QueryTimeout (one dropped reply must not eat the whole context)", elapsed)
	}
}

// TestLookupAuthoritativeTXTDetails pins the miss explanation for an
// answering-but-not-yet-propagated NS: the normal state while propagation is
// in flight, not a reachability failure.
func TestLookupAuthoritativeTXTDetails(t *testing.T) {
	stubNSTransport(t, func(context.Context, string, string) ([]string, error) {
		return []string{"some-other-token"}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	found, _, _, detail := lookupAuthoritativeTXT(ctx, []string{"ns1:53"}, "_acme-challenge.example.com", "want-val")
	if found {
		t.Fatal("expected miss for a different TXT value")
	}
	if !strings.Contains(detail, "DNS 已应答但尚未返回该 TXT 记录") {
		t.Errorf("detail = %q, want propagation-in-flight explanation", detail)
	}
}

// TestLookupAuthoritativeTXTDeadNS pins the unreachable-NS explanation: query
// failures must be surfaced, not silently swallowed into a bare "not found".
func TestLookupAuthoritativeTXTDeadNS(t *testing.T) {
	stubTestResolver(t) // system-resolver fallback fails instantly, deterministically
	stubNSTransport(t, func(context.Context, string, string) ([]string, error) {
		return nil, errors.New("i/o timeout")
	})

	found, _, _, detail := lookupAuthoritativeTXT(context.Background(), []string{"ns1:53"}, "_acme-challenge.example.com", "want-val")
	if found {
		t.Fatal("expected miss against a dead NS")
	}
	if !strings.Contains(detail, "权威 NS 查询失败") {
		t.Errorf("detail = %q, want authoritative query failure explanation", detail)
	}
}

func TestDNS01PollSchedule(t *testing.T) {
	cases := []struct {
		elapsed  time.Duration
		expected time.Duration
	}{
		{0, 3 * time.Second},
		{30 * time.Second, 3 * time.Second},
		{59 * time.Second, 3 * time.Second},
		{60 * time.Second, 10 * time.Second},
		{120 * time.Second, 10 * time.Second},
		{179 * time.Second, 10 * time.Second},
		{180 * time.Second, 30 * time.Second},
		{300 * time.Second, 30 * time.Second},
		{600 * time.Second, 30 * time.Second},
	}
	for _, tc := range cases {
		got := dns01PollSchedule(tc.elapsed)
		if got != tc.expected {
			t.Errorf("dns01PollSchedule(%s) = %s, want %s", tc.elapsed, got, tc.expected)
		}
	}
}

func TestDetectReplacement(t *testing.T) {
	fqdn := "_acme-challenge.example.com"
	stub := func(vals []string, err error) func(context.Context, string) ([]string, error) {
		return func(context.Context, string) ([]string, error) { return vals, err }
	}
	called := false
	neverProbe := func(context.Context, string) ([]string, error) {
		called = true
		return nil, nil
	}

	if hint := detectReplacement(fqdn, "missing-val", []string{"sib-val"}, stub([]string{"sib-val"}, nil)); !strings.Contains(hint, "覆盖") || !strings.Contains(hint, "追加") {
		t.Errorf("sibling visible, missing absent: hint = %q, want replacement hint", hint)
	}
	if hint := detectReplacement(fqdn, "missing-val", []string{"sib-val"}, stub([]string{"missing-val", "sib-val"}, nil)); hint != "" {
		t.Errorf("missing record present: hint = %q, want empty", hint)
	}
	if hint := detectReplacement(fqdn, "missing-val", []string{"sib-val"}, stub(nil, nil)); hint != "" {
		t.Errorf("neither visible: hint = %q, want empty", hint)
	}
	if hint := detectReplacement(fqdn, "missing-val", []string{"sib-val"}, stub(nil, errors.New("boom"))); hint != "" {
		t.Errorf("probe failed: hint = %q, want empty", hint)
	}
	if hint := detectReplacement(fqdn, "missing-val", nil, neverProbe); hint != "" || called {
		t.Errorf("no siblings: hint = %q called = %v, want empty and no probe", hint, called)
	}
}

// The per-attempt budget is shared across the whole NS list. Rotation must
// give every address a turn at the head, otherwise one slow/filtered server
// can starve the rest forever (AGENTS.md §3.7 NS 轮转).
func TestRotateNSAddrs(t *testing.T) {
	ns := []string{"1.1.1.1:53", "2.2.2.2:53", "3.3.3.3:53"}
	seen := map[string]bool{}
	for attempt := 1; attempt <= len(ns); attempt++ {
		got := rotateNSAddrs(ns, attempt)
		want := append(append([]string{}, ns[attempt-1:]...), ns[:attempt-1]...)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("attempt %d: got %v, want %v", attempt, got, want)
		}
		seen[got[0]] = true
	}
	if len(seen) != len(ns) {
		t.Errorf("rotation never gave all %d nameservers a turn at the head: %v", len(ns), seen)
	}
	if got := rotateNSAddrs(ns, len(ns)+1); strings.Join(got, ",") != strings.Join(ns, ",") {
		t.Errorf("rotation must wrap back to the original order: %v", got)
	}
	if strings.Join(ns, ",") != "1.1.1.1:53,2.2.2.2:53,3.3.3.3:53" {
		t.Errorf("rotation mutated the caller's slice: %v", ns)
	}
	if got := rotateNSAddrs([]string{"only:53"}, 3); len(got) != 1 || got[0] != "only:53" {
		t.Errorf("single NS must stay untouched: %v", got)
	}
	if got := rotateNSAddrs(nil, 2); len(got) != 0 {
		t.Errorf("empty NS list must stay empty: %v", got)
	}
}

// stubNSTransport replaces the authoritative-NS UDP transport with canned
// per-address answers, so quorum counting is pinned without real sockets
// (loopback UDP replies are not deliverable in some sandboxes).
func stubNSTransport(t *testing.T, fn func(context.Context, string, string) ([]string, error)) {
	t.Helper()
	old := queryAuthoritativeTXTServer
	queryAuthoritativeTXTServer = fn
	t.Cleanup(func() { queryAuthoritativeTXTServer = old })
}

// stubSystemTXTLookup replaces the system-resolver TXT lookup seam.
func stubSystemTXTLookup(t *testing.T, fn func(context.Context, string) ([]string, error)) {
	t.Helper()
	old := systemTXTLookup
	systemTXTLookup = fn
	t.Cleanup(func() { systemTXTLookup = old })
}

// TestLookupAuthoritativeTXTQuorum pins the majority rule: a single synced
// node must not pass the check (the ZeroSSL failure — one of 16 NS had the
// value after 3 seconds while the rest of the cluster had not converged),
// a strict majority must.
func TestLookupAuthoritativeTXTQuorum(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fqdn := "_acme-challenge.example.com"
	want := "want-val"

	twoOfThree := map[string][]string{
		"ns1:53": {want},
		"ns2:53": {want},
		"ns3:53": {"other-val"},
	}
	stubNSTransport(t, func(_ context.Context, addr, _ string) ([]string, error) {
		return twoOfThree[addr], nil
	})
	found, hits, total, _ := lookupAuthoritativeTXT(ctx, []string{"ns1:53", "ns2:53", "ns3:53"}, fqdn, want)
	if !found || hits != 2 || total != 3 {
		t.Fatalf("2 of 3 synced: found=%v hits=%d total=%d, want true/2/3", found, hits, total)
	}

	oneOfThree := map[string][]string{
		"ns1:53": {want},
		"ns2:53": {"other-val"},
		"ns3:53": {"other-val"},
	}
	stubNSTransport(t, func(_ context.Context, addr, _ string) ([]string, error) {
		return oneOfThree[addr], nil
	})
	found, hits, total, detail := lookupAuthoritativeTXT(ctx, []string{"ns1:53", "ns2:53", "ns3:53"}, fqdn, want)
	if found {
		t.Fatal("1 of 3 synced must not pass the quorum")
	}
	if hits != 1 || total != 3 {
		t.Fatalf("1 of 3 synced: hits=%d total=%d, want 1/3", hits, total)
	}
	if !strings.Contains(detail, "仅 1/3") || !strings.Contains(detail, "需至少 2 个") {
		t.Errorf("detail = %q, want partial-quorum explanation", detail)
	}

	// A single nameserver keeps the old behavior: quorum of one is one.
	stubNSTransport(t, func(context.Context, string, string) ([]string, error) {
		return []string{want}, nil
	})
	found, hits, total, _ = lookupAuthoritativeTXT(ctx, []string{"ns1:53"}, fqdn, want)
	if !found || hits != 1 || total != 1 {
		t.Fatalf("single NS: found=%v hits=%d total=%d, want true/1/1", found, hits, total)
	}
}

// TestLookupAuthoritativeTXTResolverFallback pins when the system resolver
// may speak: no nameservers at all, or every answer carrying no TXT records
// (CNAME delegation). It must never override nameservers that do return TXT
// records — that is the quorum's job.
func TestLookupAuthoritativeTXTResolverFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fqdn := "_acme-challenge.example.com"
	want := "want-val"

	stubSystemTXTLookup(t, func(context.Context, string) ([]string, error) {
		return []string{want}, nil
	})
	found, _, _, detail := lookupAuthoritativeTXT(ctx, nil, fqdn, want)
	if !found || !strings.Contains(detail, "系统解析器已见记录") {
		t.Fatalf("no NS + resolver hit: found=%v detail=%q, want pass via resolver", found, detail)
	}

	stubSystemTXTLookup(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("resolver down")
	})
	found, _, _, detail = lookupAuthoritativeTXT(ctx, nil, fqdn, want)
	if found || !strings.Contains(detail, "未定位到权威 NS，系统解析器失败") {
		t.Fatalf("no NS + resolver down: found=%v detail=%q, want miss with both errors", found, detail)
	}

	// Nameserver answers with no TXT at all: resolver may still pass it.
	stubNSTransport(t, func(context.Context, string, string) ([]string, error) {
		return nil, nil
	})
	stubSystemTXTLookup(t, func(context.Context, string) ([]string, error) {
		return []string{want}, nil
	})
	found, _, _, detail = lookupAuthoritativeTXT(ctx, []string{"ns1:53"}, fqdn, want)
	if !found || !strings.Contains(detail, "权威 NS 未返回 TXT 记录") {
		t.Fatalf("empty answer + resolver hit: found=%v detail=%q, want pass via resolver", found, detail)
	}
	stubSystemTXTLookup(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("resolver down")
	})
	found, _, _, detail = lookupAuthoritativeTXT(ctx, []string{"ns1:53"}, fqdn, want)
	if found || !strings.Contains(detail, "DNS 已应答但尚未返回该 TXT 记录") {
		t.Fatalf("empty answer + resolver down: found=%v detail=%q, want plain in-flight miss", found, detail)
	}
}

// TestVerifyDNSTXTPropagationPublicCheck pins stage 2: after the
// authoritative quorum passes, the value must also be visible through the
// system resolver before success is reported; and when stage 1 already
// passed via the system resolver, stage 2 is skipped as redundant.
func TestVerifyDNSTXTPropagationPublicCheck(t *testing.T) {
	fqdn := "_acme-challenge.example.com"
	want := "want-val"
	oldMinWait := dns01PropagationMinWait
	oldSchedule := dns01PollSchedule
	oldCheckTimeout := dns01PropagationCheckTimeout
	oldPublicTimeout := dns01PublicCheckTimeout
	oldDiscover := discoverAuthoritativeNS
	dns01PropagationMinWait = 0
	dns01PollSchedule = func(time.Duration) time.Duration { return 10 * time.Millisecond }
	dns01PropagationCheckTimeout = 30 * time.Second
	dns01PublicCheckTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		dns01PropagationMinWait = oldMinWait
		dns01PollSchedule = oldSchedule
		dns01PropagationCheckTimeout = oldCheckTimeout
		dns01PublicCheckTimeout = oldPublicTimeout
		discoverAuthoritativeNS = oldDiscover
	})

	collectLogs := func() (*[]string, func(string, ...any)) {
		lines := &[]string{}
		return lines, func(format string, args ...any) {
			*lines = append(*lines, fmt.Sprintf(format, args...))
		}
	}
	joinLines := func(lines *[]string) string { return strings.Join(*lines, "\n") }

	// Quorum passes (one stubbed NS) and the public view sees it: success,
	// with the cross-check logged.
	discoverAuthoritativeNS = func(context.Context, time.Duration, string) ([]string, error) {
		return []string{"ns1:53"}, nil
	}
	stubNSTransport(t, func(context.Context, string, string) ([]string, error) {
		return []string{want}, nil
	})
	stubSystemTXTLookup(t, func(context.Context, string) ([]string, error) {
		return []string{want}, nil
	})
	lines, logf := collectLogs()
	if err := verifyDNSTXTPropagation(context.Background(), logf, fqdn, want); err != nil {
		t.Fatalf("quorum + public view: unexpected error: %v", err)
	}
	if !strings.Contains(joinLines(lines), "公共解析器已见") {
		t.Errorf("logs = %v, want the public cross-check line", *lines)
	}

	// Quorum passes but the public view never sees it: a timeout naming
	// the public view, not the authoritative NS.
	stubSystemTXTLookup(t, func(context.Context, string) ([]string, error) {
		return nil, errors.New("resolver down")
	})
	lines, logf = collectLogs()
	err := verifyDNSTXTPropagation(context.Background(), logf, fqdn, want)
	if err == nil {
		t.Fatal("public view missing: expected timeout error")
	}
	if !strings.Contains(err.Error(), "公共解析器") || !strings.Contains(err.Error(), "多数权威 NS") {
		t.Errorf("error = %q, want the public-view timeout naming the quorum", err.Error())
	}

	// Stage 1 passed via the system resolver itself (no NS discovered):
	// stage 2 is the same view, so it is skipped.
	discoverAuthoritativeNS = func(context.Context, time.Duration, string) ([]string, error) {
		return nil, errors.New("no NS discovered")
	}
	stubSystemTXTLookup(t, func(context.Context, string) ([]string, error) {
		return []string{want}, nil
	})
	lines, logf = collectLogs()
	if err := verifyDNSTXTPropagation(context.Background(), logf, fqdn, want); err != nil {
		t.Fatalf("resolver-pass: unexpected error: %v", err)
	}
	if strings.Contains(joinLines(lines), "公共解析器") {
		t.Errorf("logs = %v, want no public cross-check when stage 1 already passed via the resolver", *lines)
	}
}
