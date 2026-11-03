package cert

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// dns01PropagationCheckTimeout is the maximum time to wait for a DNS-01 TXT
// record to propagate and be visible on authoritative nameservers.
var dns01PropagationCheckTimeout = 90 * time.Second

// dns01PropagationCheckInterval is the polling interval between propagation checks.
var dns01PropagationCheckInterval = 5 * time.Second

// dns01PropagationMinWait is the brief quiet period before the first check to
// allow dns-mng synchronous provider writes to settle.
var dns01PropagationMinWait = 2 * time.Second

// dns01AuthoritativeCheck verifies that a DNS-01 TXT record is visible on the
// authoritative nameservers. It is a package-level variable so tests can stub
// it without spinning up real DNS nameservers.
var dns01AuthoritativeCheck = verifyDNSTXTPropagation

// buildDNSQuery builds an RFC 1035 UDP DNS query packet for TXT records of the given FQDN.
func buildDNSQuery(fqdn string, recursionDesired bool) ([]byte, uint16, error) {
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		idBytes = [2]byte{0x12, 0x34}
	}
	id := binary.BigEndian.Uint16(idBytes[:])

	var flags uint16
	if recursionDesired {
		flags |= 0x0100 // RD bit set
	}

	// Header: ID (2), Flags (2), QDCOUNT (2), ANCOUNT (2), NSCOUNT (2), ARCOUNT (2)
	buf := make([]byte, 12)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[2:4], flags)
	binary.BigEndian.PutUint16(buf[4:6], 1) // QDCOUNT = 1

	// Encode QNAME
	clean := strings.TrimSuffix(strings.TrimSpace(fqdn), ".")
	for _, label := range strings.Split(clean, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, 0, fmt.Errorf("invalid DNS label in %q: %q", fqdn, label)
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0) // root label terminator

	// QTYPE = 16 (TXT), QCLASS = 1 (IN)
	var tail [4]byte
	binary.BigEndian.PutUint16(tail[0:2], 16)
	binary.BigEndian.PutUint16(tail[2:4], 1)
	buf = append(buf, tail[:]...)

	return buf, id, nil
}

// readDNSName reads a domain name from a DNS packet starting at offset, handling pointers.
func readDNSName(msg []byte, offset int) (string, int, error) {
	var labels []string
	visited := 0
	curr := offset
	jumped := false
	nextOffset := offset

	for {
		visited++
		if visited > 100 || curr >= len(msg) {
			return "", 0, errors.New("invalid or looping DNS name")
		}
		length := int(msg[curr])
		if length == 0 {
			if !jumped {
				nextOffset = curr + 1
			}
			break
		}
		if length&0xC0 == 0xC0 { // Pointer
			if curr+1 >= len(msg) {
				return "", 0, errors.New("truncated DNS pointer")
			}
			ptr := (int(length&0x3F) << 8) | int(msg[curr+1])
			if !jumped {
				nextOffset = curr + 2
				jumped = true
			}
			curr = ptr
			continue
		}
		if length&0xC0 != 0 {
			return "", 0, errors.New("unsupported label type")
		}
		curr++
		if curr+length > len(msg) {
			return "", 0, errors.New("truncated DNS label")
		}
		labels = append(labels, string(msg[curr:curr+length]))
		curr += length
		if !jumped {
			nextOffset = curr
		}
	}
	return strings.Join(labels, "."), nextOffset, nil
}

// parseDNSTXTResponse parses a raw DNS response and returns all TXT record strings found in the answer section.
func parseDNSTXTResponse(msg []byte, expectedID uint16) ([]string, error) {
	if len(msg) < 12 {
		return nil, errors.New("DNS message too short")
	}
	id := binary.BigEndian.Uint16(msg[0:2])
	if id != expectedID {
		return nil, fmt.Errorf("DNS message ID mismatch (got %d, want %d)", id, expectedID)
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	if flags&0x8000 == 0 {
		return nil, errors.New("DNS message is not a response")
	}
	rcode := flags & 0x000F
	if rcode != 0 {
		return nil, fmt.Errorf("DNS error RCODE %d", rcode)
	}

	qdCount := int(binary.BigEndian.Uint16(msg[4:6]))
	anCount := int(binary.BigEndian.Uint16(msg[6:8]))

	curr := 12
	// Skip question section
	for i := 0; i < qdCount; i++ {
		_, next, err := readDNSName(msg, curr)
		if err != nil {
			return nil, fmt.Errorf("parsing question name: %w", err)
		}
		curr = next + 4 // skip QTYPE (2) + QCLASS (2)
		if curr > len(msg) {
			return nil, errors.New("truncated question section")
		}
	}

	var txts []string
	// Parse answer section
	for i := 0; i < anCount; i++ {
		if curr >= len(msg) {
			break
		}
		_, next, err := readDNSName(msg, curr)
		if err != nil {
			return nil, fmt.Errorf("parsing answer name: %w", err)
		}
		curr = next
		if curr+10 > len(msg) {
			return nil, errors.New("truncated answer header")
		}
		rtype := binary.BigEndian.Uint16(msg[curr : curr+2])
		rclass := binary.BigEndian.Uint16(msg[curr+2 : curr+4])
		// ttl := binary.BigEndian.Uint32(msg[curr+4 : curr+8])
		rdlen := int(binary.BigEndian.Uint16(msg[curr+8 : curr+10]))
		curr += 10

		if curr+rdlen > len(msg) {
			return nil, errors.New("truncated answer rdata")
		}

		if rtype == 16 && rclass == 1 { // TXT in class IN
			rdata := msg[curr : curr+rdlen]
			var parts []string
			rpos := 0
			for rpos < len(rdata) {
				slen := int(rdata[rpos])
				rpos++
				if rpos+slen > len(rdata) {
					break
				}
				parts = append(parts, string(rdata[rpos:rpos+slen]))
				rpos += slen
			}
			txts = append(txts, strings.Join(parts, ""))
		}
		curr += rdlen
	}

	return txts, nil
}

// queryAuthoritativeServer queries a single DNS server over UDP for the TXT records of fqdn.
func queryAuthoritativeServer(ctx context.Context, serverAddr, fqdn string) ([]string, error) {
	req, id, err := buildDNSQuery(fqdn, false)
	if err != nil {
		return nil, err
	}

	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "udp", serverAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	}

	if _, err := conn.Write(req); err != nil {
		return nil, err
	}

	resp := make([]byte, 4096)
	n, err := conn.Read(resp)
	if err != nil {
		return nil, err
	}

	return parseDNSTXTResponse(resp[:n], id)
}

// findZoneNameservers finds the authoritative nameservers for a given domain by climbing up labels.
func findZoneNameservers(ctx context.Context, fqdn string) ([]string, error) {
	name := strings.TrimPrefix(strings.TrimSuffix(fqdn, "."), "_acme-challenge.")
	labels := strings.Split(name, ".")

	for i := 0; i < len(labels)-1; i++ {
		cand := strings.Join(labels[i:], ".")
		nss, err := net.DefaultResolver.LookupNS(ctx, cand)
		if err == nil && len(nss) > 0 {
			var hosts []string
			for _, ns := range nss {
				h := strings.TrimSuffix(strings.TrimSpace(ns.Host), ".")
				if h != "" {
					hosts = append(hosts, h)
				}
			}
			if len(hosts) > 0 {
				return hosts, nil
			}
		}
	}
	return nil, errors.New("no authoritative nameservers found")
}

// lookupAuthoritativeTXT queries the authoritative nameservers for fqdn and checks
// whether want is present in the returned TXT records. If authoritative query fails,
// it falls back to the system resolver.
func lookupAuthoritativeTXT(ctx context.Context, fqdn, want string) (bool, error) {
	nsHosts, err := findZoneNameservers(ctx, fqdn)
	if err == nil && len(nsHosts) > 0 {
		for _, host := range nsHosts {
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(ips) == 0 {
				continue
			}
			for _, ip := range ips {
				addr := net.JoinHostPort(ip.IP.String(), "53")
				txts, qErr := queryAuthoritativeServer(ctx, addr, fqdn)
				if qErr == nil {
					for _, t := range txts {
						if t == want {
							return true, nil
						}
					}
				}
			}
		}
	}

	// Fallback: check system resolver
	sysTxts, sysErr := net.DefaultResolver.LookupTXT(ctx, fqdn)
	if sysErr == nil {
		for _, t := range sysTxts {
			if t == want {
				return true, nil
			}
		}
	}

	return false, nil
}

// verifyDNSTXTPropagation polls authoritative nameservers until the expected TXT value
// is visible, or the timeout is exceeded.
func verifyDNSTXTPropagation(ctx context.Context, fqdn, want string) error {
	if dns01PropagationMinWait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dns01PropagationMinWait):
		}
	}

	timeout := dns01PropagationCheckTimeout
	interval := dns01PropagationCheckInterval
	deadline := time.Now().Add(timeout)

	for {
		found, err := lookupAuthoritativeTXT(ctx, fqdn, want)
		if err == nil && found {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("DNS 传播校验超时 (%s): %s 的 TXT 记录 %q 未能在权威 NS 生效", timeout, fqdn, want)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
