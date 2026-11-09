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
// record to propagate and be visible on authoritative nameservers. 600s
// covers the real convergence window of the major Chinese DNS providers:
// the record reaches the provider's API in seconds (their default TTL of
// 600 only governs caching resolvers, but their NS clusters keep syncing
// across nodes for minutes, and the queried NS IP set can include late or
// filtered nodes — hitting i/o timeouts one poll at a time). The check
// therefore requires a strict majority of the discovered NS addresses to
// serve the value, and then cross-checks it through the system resolver
// (the external recursive view the CA sees), before the CA is notified; a
// single synced node no longer passes.
var dns01PropagationCheckTimeout = 600 * time.Second

// dns01PropagationCheckInterval is the polling interval between propagation checks.
var dns01PropagationCheckInterval = 5 * time.Second

// dns01PropagationMinWait is the brief quiet period before the first check to
// allow dns-mng synchronous provider writes to settle.
var dns01PropagationMinWait = 2 * time.Second

// dns01AttemptTimeout bounds one full polling attempt (authoritative queries
// plus the system-resolver fallback). Without it, a dropped UDP reply or an
// unreachable NS consumed the entire propagation window inside a single
// attempt — the loop then logged nothing between the first line and the raw
// deadline error, i.e. it did not visibly poll at all.
var dns01AttemptTimeout = 10 * time.Second

// dns01QueryTimeout bounds a single authoritative UDP query round trip. It is
// a variable so tests can shorten it.
var dns01QueryTimeout = 3 * time.Second

// dns01AuthoritativeCheck verifies that a DNS-01 TXT record is visible on the
// authoritative nameservers, polling until the record shows up or the
// propagation budget runs out; logf receives one progress line per attempt.
// It is a package-level variable so tests can stub it without spinning up
// real DNS nameservers.
var dns01AuthoritativeCheck = verifyDNSTXTPropagation

// systemTXTLookup is the system-resolver TXT lookup behind both the
// authoritative check's fallback and the public-resolver cross-check. It
// reads net.DefaultResolver at call time (not a bound method value) so
// tests that replace the resolver still take effect, and so tests can stub
// it outright.
var systemTXTLookup = func(ctx context.Context, fqdn string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, fqdn)
}

// dns01PublicCheckTimeout bounds the public-resolver cross-check that runs
// after the authoritative quorum passes. The CA validates through its own
// recursive resolvers; three minutes is enough for a converged record to
// show up in the public view, and a longer wait would only burn the shared
// propagation budget on a broken host resolver.
var dns01PublicCheckTimeout = 3 * time.Minute

// discoverAuthoritativeNS resolves the authoritative nameserver addresses
// for a zone. It is a package-level variable so tests can point the
// propagation check at fake nameservers without real DNS.
var discoverAuthoritativeNS = discoverAuthoritativeAddrs

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

	d := net.Dialer{Timeout: dns01QueryTimeout}
	conn, err := d.DialContext(ctx, "udp", serverAddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// One query costs at most dns01QueryTimeout — never the caller's whole
	// remaining budget. Setting the connection deadline straight to the
	// context deadline let a single dropped UDP reply stall the entire
	// propagation window with no polling in between.
	readDeadline := time.Now().Add(dns01QueryTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(readDeadline) {
		readDeadline = ctxDeadline
	}
	_ = conn.SetDeadline(readDeadline)

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

// discoverAuthoritativeAddrs resolves the authoritative nameservers for fqdn
// (see findZoneNameservers) down to UDP query addresses, bounded by one
// attempt's budget. Discovery happens once per verification: re-resolving NS
// and A/AAAA records on every polling attempt only wastes budget on resolver
// round trips.
func discoverAuthoritativeAddrs(ctx context.Context, budget time.Duration, fqdn string) ([]string, error) {
	if budget > dns01AttemptTimeout {
		budget = dns01AttemptTimeout
	}
	if budget < 0 {
		budget = 0
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	hosts, err := findZoneNameservers(ctx, fqdn)
	if err != nil {
		return nil, err
	}
	var addrs []string
	for _, host := range hosts {
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			addrs = append(addrs, net.JoinHostPort(ip.IP.String(), "53"))
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("authoritative nameservers resolved to no queryable addresses")
	}
	return addrs, nil
}

// rotateNSAddrs rotates the authoritative NS order once per attempt. The
// per-attempt budget is shared across the list, so a fixed order would let a
// slow or filtered head server starve every trailing nameserver. Extracted as
// a pure helper so the rotation can be pinned by unit tests without spinning
// up UDP servers.
func rotateNSAddrs(nsAddrs []string, attempt int) []string {
	n := len(nsAddrs)
	if n <= 1 {
		return nsAddrs
	}
	r := (attempt - 1) % n
	if r < 0 {
		r += n
	}
	return append(append([]string{}, nsAddrs[r:]...), nsAddrs[:r]...)
}

// queryAuthoritativeTXTServer queries one authoritative nameserver for the
// TXT records of fqdn. It is a package-level variable so tests can stub the
// transport and pin the quorum logic without real UDP sockets (loopback UDP
// replies are not deliverable in some sandboxes).
var queryAuthoritativeTXTServer = queryAuthoritativeServer

// lookupAuthoritativeTXT queries the pre-resolved authoritative nameserver
// addresses for fqdn and checks whether want is present in the returned TXT
// records. It passes only when a strict majority of the nameservers already
// serve the value (quorum = len(nsAddrs)/2+1; e.g. 9 of 16) — a single
// synced node is not enough, because the CA may query any node, and a
// first-hit pass let an issuance notify the CA while most of the
// provider's cluster had not converged yet (ZeroSSL, 2026-11-05).
//
// It returns the number of nameservers that served the value and the total
// discovered, so the caller can log the quorum and diagnose a partial sync.
// When no addresses were discovered, or no authoritative query answered at
// all, it falls back to the system resolver — the path the no-NS case
// relies on. The fallback is also consulted when every answering
// nameserver returned no TXT records at all (a CNAME-delegated
// _acme-challenge, where the zone NS answers with a CNAME instead of TXT);
// when nameservers do return TXT records, the system resolver never
// overrides them. detail explains the miss (or a resolver-side pass) so
// the polling log and the final timeout error can distinguish "DNS
// answered, record not propagated yet" from "NS unreachable".
func lookupAuthoritativeTXT(ctx context.Context, nsAddrs []string, fqdn, want string) (found bool, hits, total int, detail string) {
	total = len(nsAddrs)
	quorum := total/2 + 1
	var answered, anyTXT bool
	var failures int
	var lastErr error
	for _, addr := range nsAddrs {
		if err := ctx.Err(); err != nil {
			break
		}
		txts, qErr := queryAuthoritativeTXTServer(ctx, addr, fqdn)
		if qErr != nil {
			failures++
			lastErr = qErr
			continue
		}
		answered = true
		if len(txts) > 0 {
			anyTXT = true
		}
		for _, t := range txts {
			if t == want {
				hits++
				break
			}
		}
		if hits >= quorum {
			return true, hits, total, ""
		}
	}

	// The system resolver is the source of truth only when the
	// authoritative path yielded nothing usable: no nameserver answered,
	// or every answer carried no TXT records at all (CNAME delegation).
	if !answered || !anyTXT {
		if sysTxts, err := systemTXTLookup(ctx, fqdn); err == nil {
			for _, t := range sysTxts {
				if t == want {
					if len(nsAddrs) == 0 {
						return true, 0, total, "系统解析器已见记录"
					}
					return true, 0, total, "系统解析器已见记录（权威 NS 未返回 TXT 记录）"
				}
			}
		} else if !answered {
			// Both paths failed and nothing answered: surface both errors.
			if lastErr != nil {
				return false, hits, total, fmt.Sprintf("权威 NS 查询失败: %v；系统解析器失败: %v", lastErr, err)
			}
			return false, hits, total, fmt.Sprintf("未定位到权威 NS，系统解析器失败: %v", err)
		}
	}

	failSuffix := ""
	if failures > 0 {
		failSuffix = fmt.Sprintf("；另有 %d 个 NS 查询失败（最后一次: %v）", failures, lastErr)
	}
	if hits > 0 {
		return false, hits, total, fmt.Sprintf("仅 %d/%d 个权威 NS 见到该 TXT 记录（需至少 %d 个）%s", hits, total, quorum, failSuffix)
	}
	if answered {
		// Reachable and answering — the record simply has not shown up yet,
		// which is the normal state while propagation is in flight.
		return false, hits, total, "DNS 已应答但尚未返回该 TXT 记录" + failSuffix
	}
	if lastErr != nil {
		return false, hits, total, fmt.Sprintf("权威 NS 查询失败: %v；系统解析器未见记录%s", lastErr, failSuffix)
	}
	return false, hits, total, "未定位到权威 NS，系统解析器未见记录"
}

// lookupAuthoritativeTXTValues returns every TXT string currently published
// at fqdn, asking the first authoritative server that answers (system
// resolver as fallback for CNAME-delegated names). Used only on the failure
// path to tell "record not propagated yet" apart from "the DNS backend
// replaced our record set".
func lookupAuthoritativeTXTValues(ctx context.Context, fqdn string) ([]string, error) {
	if addrs, err := discoverAuthoritativeNS(ctx, 10*time.Second, fqdn); err == nil {
		for _, addr := range addrs {
			if txts, qErr := queryAuthoritativeTXTServer(ctx, addr, fqdn); qErr == nil {
				return txts, nil
			}
		}
	}
	return net.DefaultResolver.LookupTXT(ctx, fqdn)
}

// detectReplacement checks, after a propagation failure, whether a sibling
// batch record for the same FQDN is already visible while the missing one
// is not. Batch present only works if the DNS backend appends values for a
// shared FQDN; if it replaces them, the earlier record vanishes while the
// later one shows up, which is exactly this signature. It returns a
// human-readable hint, or "" when there is no such evidence (no siblings,
// probe failure, or the missing record showing up after all). probe is a
// seam so tests can stub the DNS lookup.
func detectReplacement(fqdn, missing string, siblingTXTs []string, probe func(context.Context, string) ([]string, error)) string {
	if len(siblingTXTs) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	vals, err := probe(ctx, fqdn)
	if err != nil {
		return ""
	}
	present := make(map[string]bool, len(vals))
	for _, v := range vals {
		present[v] = true
	}
	if present[missing] {
		return ""
	}
	for _, sib := range siblingTXTs {
		if present[sib] {
			return fmt.Sprintf("同一域名 %s 下已有其他待验证记录生效、唯独本条缺失，DNS 服务端可能以覆盖而非追加方式写入 TXT（请检查 dns-mng 的 present 是否为追加语义）", fqdn)
		}
	}
	return ""
}

// dns01PollSchedule returns the polling interval for the given elapsed time since
// the start of propagation checks.
//   - 0 to 60s: every 3s (dense polling for fast-converging DNS providers);
//   - 60s to 180s: every 10s;
//   - after 180s (up to 600s total budget): every 30s (prevents log spamming
//     over the 10-minute window while continuing to monitor slow NS clusters).
var dns01PollSchedule = func(elapsed time.Duration) time.Duration {
	if elapsed < 60*time.Second {
		return 3 * time.Second
	}
	if elapsed < 180*time.Second {
		return 10 * time.Second
	}
	return 30 * time.Second
}

// verifyDNSTXTPropagation polls authoritative nameservers until the expected
// TXT value is visible on a strict majority of them, then cross-checks it
// through the system resolver (the external, recursive view the CA sees)
// before reporting success. Every attempt is reported through logf, and the
// timeout error always carries the attempt count and the last attempt's
// reason — never a bare context deadline.
func verifyDNSTXTPropagation(ctx context.Context, logf func(format string, args ...any), fqdn, want string) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if dns01PropagationMinWait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dns01PropagationMinWait):
		}
	}

	start := time.Now()
	// The loop's own deadline is what produces the diagnostic timeout
	// message, so clamp it to the caller's context too: a batch DNS-01 order
	// shares one context across records, and the record that runs out of
	// shared budget must still end with the readable timeout instead of the
	// raw context.DeadlineExceeded.
	deadline := start.Add(dns01PropagationCheckTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	nsAddrs, nsErr := discoverAuthoritativeNS(ctx, time.Until(deadline), fqdn)
	if len(nsAddrs) == 0 {
		if nsErr != nil {
			logf("未定位到权威 NS（%v），改用系统解析器轮询 TXT 生效", nsErr)
		} else {
			logf("未定位到权威 NS，改用系统解析器轮询 TXT 生效")
		}
	} else {
		logf("已定位权威 NS（%d 个地址），采用阶梯式轮询，最长等待 %s",
			len(nsAddrs), time.Until(deadline).Round(time.Second))
	}

	attempt := 0
	detail := "尚未探测"
	bestHits, bestTotal := 0, 0
	timeoutErr := func() error {
		msg := fmt.Sprintf("DNS 传播校验超时（已等待 %s）: %s 的 TXT 记录 %q 未能在权威 NS 生效（共探测 %d 次，最后一次: %s）",
			time.Since(start).Round(time.Second), fqdn, want, attempt, detail)
		if bestTotal > 0 {
			msg += fmt.Sprintf("；最多 %d/%d 个权威 NS 见到记录", bestHits, bestTotal)
		}
		return errors.New(msg)
	}
	quorumHits, quorumTotal := 0, 0
	for {
		elapsed := time.Since(start)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return timeoutErr()
		}
		budget := dns01AttemptTimeout
		if remaining < budget {
			budget = remaining
		}
		attempt++
		attemptCtx, cancel := context.WithTimeout(ctx, budget)
		// Rotate the NS order each attempt: the per-attempt budget is
		// shared across the list, so a fixed order would starve trailing
		// nameservers whenever the head ones are slow or filtered.
		// Rotation spreads the budget across all of them over attempts.
		addrs := rotateNSAddrs(nsAddrs, attempt)
		found, hits, total, d := lookupAuthoritativeTXT(attemptCtx, addrs, fqdn, want)
		cancel()
		if hits > bestHits {
			bestHits, bestTotal = hits, total
		}
		if found {
			quorumHits, quorumTotal = hits, total
			if hits > 0 {
				logf("第 %d 次探测命中（%d/%d 个权威 NS 已见记录），TXT 记录已生效", attempt, hits, total)
			} else {
				// Passed via the system resolver (no NS discovered, or the
				// zone NS returned no TXT at all): the external view is
				// already proven by the same resolver, so stage 2 is moot.
				logf("第 %d 次探测命中（%s），TXT 记录已生效", attempt, d)
				return nil
			}
			break
		}
		if attempt == 1 || !errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
			detail = d
		}
		logf("第 %d 次探测未见记录（%s），继续轮询...", attempt, detail)

		wait := dns01PollSchedule(elapsed)
		if remaining = time.Until(deadline); remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			if !time.Now().Before(deadline) {
				return timeoutErr()
			}
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	// Stage 2: public-resolver cross-check. The authoritative quorum proves
	// the provider's cluster converged, but the CA validates through its
	// own recursive resolvers — a second, external view must see the value
	// before the CA is notified.
	return verifyPublicTXTPropagation(ctx, logf, fqdn, want, deadline, quorumHits, quorumTotal)
}

// verifyPublicTXTPropagation is the second stage of the propagation check:
// after the authoritative quorum passes, the TXT value must also be visible
// through the system resolver (the external, recursive view the CA sees)
// before the CA is notified. It polls within the shared propagation
// deadline, capped by dns01PublicCheckTimeout, and returns a diagnostic
// that names the public view explicitly so it is never confused with an
// authoritative-NS timeout.
func verifyPublicTXTPropagation(ctx context.Context, logf func(format string, args ...any), fqdn, want string, deadline time.Time, quorumHits, quorumTotal int) error {
	start := time.Now()
	stageDeadline := deadline
	if cap := start.Add(dns01PublicCheckTimeout); cap.Before(stageDeadline) {
		stageDeadline = cap
	}
	detail := "尚未探测"
	timeoutErr := func() error {
		quorumClause := "已在系统解析器生效"
		if quorumTotal > 0 {
			quorumClause = fmt.Sprintf("已在多数权威 NS 生效（%d/%d）", quorumHits, quorumTotal)
		}
		return fmt.Errorf("DNS 公共解析器校验超时（已等待 %s）: %s 的 TXT 记录 %q %s，但公共解析器未见记录（最后一次: %s）；CA 可能同样查不到，请等待 DNS 传播完成后再试",
			time.Since(start).Round(time.Second), fqdn, want, quorumClause, detail)
	}
	elapsed := time.Duration(0)
	for {
		if !time.Now().Before(stageDeadline) {
			return timeoutErr()
		}
		lookupCtx, cancel := context.WithTimeout(ctx, dns01AttemptTimeout)
		txts, err := systemTXTLookup(lookupCtx, fqdn)
		cancel()
		if err == nil {
			seen := false
			for _, t := range txts {
				if t == want {
					seen = true
					break
				}
			}
			if seen {
				logf("公共解析器已见 TXT 记录，外部视角确认")
				return nil
			}
			detail = "公共解析器已应答但尚未返回该 TXT 记录"
		} else {
			detail = fmt.Sprintf("公共解析器查询失败: %v", err)
		}
		logf("公共解析器未见记录（%s），继续等待...", detail)

		wait := dns01PollSchedule(elapsed)
		if remaining := time.Until(stageDeadline); remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			if !time.Now().Before(stageDeadline) {
				return timeoutErr()
			}
			return ctx.Err()
		case <-time.After(wait):
		}
		elapsed = time.Since(start)
	}
}
