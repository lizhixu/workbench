//go:build linux

package metrics

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
)

// defaultRouteIfaces returns the set of interfaces that carry a default
// route (destination 0.0.0.0 in /proc/net/route), i.e. the external NIC(s)
// through which traffic to the internet leaves the host. It returns nil
// when the routing table cannot be read or no default route exists, in
// which case traffic accounting falls back to all non-loopback interfaces
// rather than counting nothing.
func defaultRouteIfaces() map[string]bool {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseDefaultRouteIfaces(f)
}

// parseDefaultRouteIfaces extracts the interfaces with a default route
// from a /proc/net/route table. Lines are:
// Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
// The destination is hex, little-endian; 00000000 is the default route.
// Only routes that are up (RTF_UP, flag bit 0x1) are considered.
func parseDefaultRouteIfaces(r io.Reader) map[string]bool {
	var out map[string]bool
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[0] == "Iface" {
			continue
		}
		if fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x1 == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[fields[0]] = true
	}
	return out
}
