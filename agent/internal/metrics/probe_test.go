package metrics

import (
	"errors"
	"net"
	"testing"
	"time"
)

// listenerAddr starts a local TCP listener and returns its "host:port".
func listenerAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Accept in the background so dials complete.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return ln.Addr().String()
}

// A reachable target yields a positive latency and zero loss. The dial is
// faked with a controlled delay: a real loopback connect can finish faster
// than the host clock's resolution (measured as exactly 0 on Windows), which
// would make a latency>0 assertion race the machine instead of the code.
func TestProbeRoundReachable(t *testing.T) {
	res := runProbeRoundWith("zj-ct-v4.ip.zstaticcdn.com:80", func(network, addr string, timeout time.Duration) (net.Conn, error) {
		time.Sleep(2 * time.Millisecond)
		conn, _ := net.Pipe()
		return conn, nil
	})
	if !res.done {
		t.Fatal("round should be marked done")
	}
	if res.lossPct != 0 {
		t.Fatalf("loss = %v, want 0", res.lossPct)
	}
	if res.latencyMs <= 0 || res.latencyMs > 500 {
		t.Fatalf("latency = %v, want > 0 (each dial slept 2ms)", res.latencyMs)
	}
}

// The production wiring (net.DialTimeout against a real listener) records zero
// loss. The measured latency is not asserted here — a loopback connect is
// faster than some host clocks can resolve; the latency math is covered
// deterministically by TestProbeRoundReachable above.
func TestProbeRoundRealListener(t *testing.T) {
	res := runProbeRound(listenerAddr(t))
	if !res.done || res.lossPct != 0 {
		t.Fatalf("got %+v, want done with loss 0", res)
	}
}

// Failed attempts count as loss while successful ones still feed the latency
// median.
func TestProbeRoundPartialLoss(t *testing.T) {
	var attempts int
	res := runProbeRoundWith("example.com:80", func(network, addr string, timeout time.Duration) (net.Conn, error) {
		attempts++
		if attempts%2 == 0 {
			return nil, errors.New("connection refused")
		}
		conn, _ := net.Pipe()
		time.Sleep(time.Millisecond)
		return conn, nil
	})
	if res.lossPct != 50 {
		t.Fatalf("loss = %v, want 50", res.lossPct)
	}
	if res.latencyMs <= 0 {
		t.Fatalf("latency = %v, want > 0", res.latencyMs)
	}
}

// An unreachable target (connection refused) yields full loss and no
// latency data (0).
func TestProbeRoundUnreachable(t *testing.T) {
	// Grab a port, then close the listener so nothing accepts any more.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	res := runProbeRound(addr)
	if res.lossPct != 100 || res.latencyMs != 0 {
		t.Fatalf("got latency=%v loss=%v, want 0/100", res.latencyMs, res.lossPct)
	}
}

// Latency is the median of the successful attempts (mean of the two middle
// values for an even count).
func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{5}, 5},
		{[]float64{1, 3}, 2},
		{[]float64{3, 1, 2}, 2},
		{[]float64{10, 1, 2, 3}, 2.5},
		{[]float64{}, 0},
	}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Fatalf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// normalizeProbeAddr fills in the default port and skips empty hosts.
func TestNormalizeProbeAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"zj-ct-v4.ip.zstaticcdn.com", "zj-ct-v4.ip.zstaticcdn.com:80"},
		{"zj-ct-v4.ip.zstaticcdn.com:80", "zj-ct-v4.ip.zstaticcdn.com:80"},
		{"1.2.3.4:443", "1.2.3.4:443"},
		{"  example.com  ", "example.com:80"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := normalizeProbeAddr(c.in); got != c.want {
			t.Fatalf("normalizeProbeAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// SetTargets starts one loop per carrier, publishes results, reconfigures
// single targets without disturbing the others, and stops everything when
// the target set empties out.
func TestProberLifecycle(t *testing.T) {
	addrA := listenerAddr(t)
	addrB := listenerAddr(t)

	p := newProber()
	if len(p.latest()) != 0 {
		t.Fatal("fresh prober should have no data")
	}

	p.SetTargets(map[string]string{"telecom": addrA, "unicom": addrB})
	waitProbeResults(t, p, 2)

	got := p.latest()
	for _, carrier := range []string{"telecom", "unicom"} {
		r, ok := got[carrier]
		if !ok {
			t.Fatalf("carrier %q missing from %+v", carrier, got)
		}
		if !r.done || r.lossPct != 0 {
			t.Fatalf("carrier %q: %+v, want done with loss 0", carrier, r)
		}
	}

	// Reconfigure only one carrier; the untouched one must keep its loop
	// (its result survives).
	p.SetTargets(map[string]string{"telecom": addrA, "unicom": ""})
	waitProbeResults(t, p, 1)
	got = p.latest()
	if _, ok := got["unicom"]; ok {
		t.Fatal("disabled carrier should stop reporting")
	}
	if r, ok := got["telecom"]; !ok || r.lossPct != 0 {
		t.Fatalf("untouched carrier should keep probing: %+v", got)
	}

	// Clearing everything stops probing and resets results.
	p.SetTargets(nil)
	if len(p.latest()) != 0 {
		t.Fatalf("clearing targets should reset results, got %+v", p.latest())
	}
	// Re-setting the same (nil) map must be a no-op, not a panic.
	p.SetTargets(nil)
}

// waitProbeResults polls latest() until it holds n carrier results.
func waitProbeResults(t *testing.T, p *prober, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := len(p.latest())
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d probe results, have %d: %+v", n, got, p.latest())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
