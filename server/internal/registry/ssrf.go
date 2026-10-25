package registry

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// lookupIP resolves a hostname to IPs. A variable (rather than a direct
// net.DefaultResolver call) so tests can stub DNS without network access.
var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// reservedNetworks are non-routable or special-purpose ranges that must never
// be dialed by the prober, on top of what net.IP.IsPrivate already covers
// (RFC 1918 + ULA). Dialing any of these from the control plane would turn
// the prober into an SSRF trampoline.
var reservedNetworks []*net.IPNet

func init() {
	for _, cidr := range []string{
		"100.64.0.0/10",   // CGNAT shared address space
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1 (documentation)
		"198.51.100.0/24", // TEST-NET-2 (documentation)
		"203.0.113.0/24",  // TEST-NET-3 (documentation)
		"240.0.0.0/4",     // reserved + limited broadcast
	} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			reservedNetworks = append(reservedNetworks, n)
		}
	}
}

// isPublicIP reports whether ip is a globally routable unicast address.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	for _, n := range reservedNetworks {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// stripHostPort returns the bare host part of a host[:port] value, tolerating
// IPv6 brackets.
func stripHostPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// validatePublicHost requires hostport to resolve (if a hostname) to only
// public IPs. IP literals are checked without DNS. A resolution failure is a
// hard error: failing open would let a hostile name resolve privately later.
func validatePublicHost(ctx context.Context, hostport string) error {
	host := stripHostPort(hostport)
	if host == "" {
		return fmt.Errorf("仓库地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !isPublicIP(ip) {
			return fmt.Errorf("拒绝连接内网/本机地址: %s", host)
		}
		return nil
	}
	ips, err := lookupIP(ctx, host)
	if err != nil {
		return fmt.Errorf("解析仓库地址失败: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("仓库地址无法解析: %s", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("拒绝连接解析到内网地址的仓库主机: %s", host)
		}
	}
	return nil
}

// resolvePublicDialTarget resolves host and returns one public IP to dial.
// Every resolved address must be public; the caller dials the returned IP
// directly, so the address validated here is exactly the address connected
// to — there is no DNS-rebinding window between validation and dial.
func resolvePublicDialTarget(ctx context.Context, host string) (net.IP, error) {
	ips, err := lookupIP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("解析仓库地址失败: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("仓库地址无法解析: %s", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("拒绝连接解析到内网地址的仓库主机: %s", host)
		}
	}
	return ips[0], nil
}

// proxyHostFromEnv returns the hostname of the HTTP(S) proxy configured via
// environment, or "" when no proxy is set. The transport dials the proxy
// itself (not the target) in that case, so the safe dialer must not apply
// target-host validation to the proxy connection.
func proxyHostFromEnv() string {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		raw := os.Getenv(key)
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			return strings.ToLower(u.Hostname())
		}
	}
	return ""
}

// safeDialContext is the transport dialer for registry probing. It resolves
// the target hostname, rejects any non-public resolution, and dials the
// validated IP directly (TLS ServerName verification still uses the URL
// hostname, so certificate checks are unaffected). Proxy connections are
// passed through untouched: with a proxy configured, target resolution
// happens at the proxy, and the proxy itself is trusted infrastructure.
func (c *Client) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if c.allowPrivateHosts {
		return d.DialContext(ctx, network, addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ph := proxyHostFromEnv(); ph != "" && strings.ToLower(stripHostPort(host)) == ph {
		return d.DialContext(ctx, network, addr)
	}
	if ip := net.ParseIP(stripHostPort(host)); ip != nil {
		if !isPublicIP(ip) {
			return nil, fmt.Errorf("拒绝连接内网/本机地址: %s", host)
		}
		return d.DialContext(ctx, network, addr)
	}
	target, err := resolvePublicDialTarget(ctx, stripHostPort(host))
	if err != nil {
		return nil, err
	}
	return d.DialContext(ctx, network, net.JoinHostPort(target.String(), port))
}

// checkRedirect validates every redirect target before following it: a blob
// CDN hop (e.g. ghcr.io → blob.core.windows.net) is legitimate and stays
// allowed as long as it resolves to public IPs, while a hop to a private or
// loopback address is refused. It mirrors net/http's default 10-redirect cap.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if c.allowPrivateHosts {
		return nil
	}
	return validatePublicHost(req.Context(), req.URL.Host)
}
