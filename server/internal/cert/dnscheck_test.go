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
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer pc.Close()
	go serveTXT(pc, "some-other-token")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	found, detail := lookupAuthoritativeTXT(ctx, []string{pc.LocalAddr().String()}, "_acme-challenge.example.com", "want-val")
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
	oldQuery := dns01QueryTimeout
	dns01QueryTimeout = 50 * time.Millisecond
	t.Cleanup(func() { dns01QueryTimeout = oldQuery })

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	defer pc.Close()
	go func() { // read and never reply
		buf := make([]byte, 512)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	found, detail := lookupAuthoritativeTXT(context.Background(), []string{pc.LocalAddr().String()}, "_acme-challenge.example.com", "want-val")
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

