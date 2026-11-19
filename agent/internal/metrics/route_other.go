//go:build !linux

package metrics

// defaultRouteIfaces is Linux-only (/proc/net/route). On other platforms
// the routing table is not inspected and traffic accounting falls back to
// all non-loopback interfaces.
func defaultRouteIfaces() map[string]bool {
	return nil
}
