package cert

import (
	"context"
	"encoding/binary"
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

func TestVerifyDNSTXTPropagationTimeout(t *testing.T) {
	oldTimeout := dns01PropagationCheckTimeout
	oldInterval := dns01PropagationCheckInterval
	oldMinWait := dns01PropagationMinWait
	dns01PropagationCheckTimeout = 50 * time.Millisecond
	dns01PropagationCheckInterval = 10 * time.Millisecond
	dns01PropagationMinWait = 0
	defer func() {
		dns01PropagationCheckTimeout = oldTimeout
		dns01PropagationCheckInterval = oldInterval
		dns01PropagationMinWait = oldMinWait
	}()

	ctx := context.Background()
	err := verifyDNSTXTPropagation(ctx, "nonexistent-domain-for-propagation-test.invalid", "want-val")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "超时") && !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error = %q, want timeout explanation", err)
	}
}
