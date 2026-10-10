package metrics

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Network quality probe (三网监控): periodically dials a few lightweight TCP
// endpoints (one per carrier: telecom / unicom / mobile, panel-configured via
// RegisterResponse.probe_targets) and derives latency + packet loss per
// carrier. Every target runs on its own goroutine so metrics collection never
// blocks on the network; collect() only reads the latest completed round.
//
// Result semantics (MetricsSample.probe_results, per carrier):
//   - latency = median round-trip time of the successful attempts (ms);
//     0 when no attempt succeeded.
//   - loss = share of failed attempts in the round (percent 0..100);
//     an attempt fails on dial error or timeout.
const (
	probeAttempts    = 4
	probeTimeout     = 3 * time.Second
	probeInterval    = 30 * time.Second
	probeAttemptGap  = 200 * time.Millisecond
	defaultProbePort = "80"
)

type probeResult struct {
	latencyMs float64
	lossPct   float64
	done      bool // at least one round completed since (re)configuration
}

// targetProber probes a single carrier target on its own goroutine.
type targetProber struct {
	addr   string // normalized "host:port"
	result probeResult
	cancel context.CancelFunc
}

// prober manages one probing loop per carrier target.
type prober struct {
	mu      sync.Mutex
	targets map[string]*targetProber // carrier -> prober (nil = probing off)
}

func newProber() *prober {
	return &prober{}
}

// SetTargets reconfigures the probe targets (carrier -> "host[:port]"). The
// server pushes the host's full effective target set on every (re)connect, so
// any carrier missing from the map (or mapped to an empty value) must stop
// probing: the panel treats an empty target as "do not probe this carrier".
// An empty map stops probing entirely (old server or monitoring disabled); a
// changed target restarts its round loop while untouched targets keep
// running. Safe to call on every (re)connect.
func (p *prober) SetTargets(targets map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Resolve the set of carriers that should be probing after this update.
	keep := make(map[string]string, len(targets))
	for carrier, raw := range targets {
		if addr := normalizeProbeAddr(raw); addr != "" {
			keep[carrier] = addr
		}
	}

	// Stop loops for carriers that are gone or disabled.
	for carrier, tp := range p.targets {
		if _, ok := keep[carrier]; !ok {
			if tp.cancel != nil {
				tp.cancel()
			}
			delete(p.targets, carrier)
		}
	}

	// Start or restart loops for new/changed targets.
	for carrier, addr := range keep {
		if tp, ok := p.targets[carrier]; ok && tp.addr == addr {
			continue // unchanged target keeps its running loop
		}
		if tp, ok := p.targets[carrier]; ok && tp.cancel != nil {
			tp.cancel()
		}
		if p.targets == nil {
			p.targets = make(map[string]*targetProber)
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.targets[carrier] = &targetProber{addr: addr, cancel: cancel}
		go p.loop(ctx, carrier, addr)
	}
}

// normalizeProbeAddr completes a "host[:port]" target with the default port.
// Returns "" for an empty host so disabled carriers are simply skipped.
func normalizeProbeAddr(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return ""
	}
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port == "" {
			// "host:" — an explicit but empty port takes the default too,
			// otherwise every dial would fail on the missing port.
			return net.JoinHostPort(h, defaultProbePort)
		}
		return host
	}
	return net.JoinHostPort(host, defaultProbePort)
}

func (p *prober) loop(ctx context.Context, carrier, addr string) {
	p.runOnce(carrier, addr)
	t := time.NewTicker(probeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.runOnce(carrier, addr)
		}
	}
}

func (p *prober) runOnce(carrier, addr string) {
	res := runProbeRound(addr)
	p.mu.Lock()
	// Only publish if this round still belongs to the carrier's current target.
	if tp, ok := p.targets[carrier]; ok && tp.addr == addr {
		tp.result = res
	}
	p.mu.Unlock()
}

// latest returns the most recent round per carrier. A carrier is absent
// (or done=false) when it has no usable data yet (probing off, or the first
// round has not finished).
func (p *prober) latest() map[string]probeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]probeResult, len(p.targets))
	for carrier, tp := range p.targets {
		if tp.result.done {
			out[carrier] = tp.result
		}
	}
	return out
}

// dialFn dials a TCP endpoint, mirroring net.DialTimeout. Injectable so the
// round logic can be tested without racing the host clock.
type dialFn func(network, addr string, timeout time.Duration) (net.Conn, error)

// runProbeRound performs one round of TCP dial attempts against addr and
// reduces it to a latency/loss pair. Exposed for tests.
func runProbeRound(addr string) probeResult {
	return runProbeRoundWith(addr, net.DialTimeout)
}

// runProbeRoundWith is the round core with an injectable dial: production
// passes net.DialTimeout, tests pass a fake with a controlled delay.
func runProbeRoundWith(addr string, dial dialFn) probeResult {
	res := probeResult{done: true}
	var rtts []float64
	for i := 0; i < probeAttempts; i++ {
		if i > 0 {
			time.Sleep(probeAttemptGap)
		}
		start := time.Now()
		conn, err := dial("tcp", addr, probeTimeout)
		if err != nil {
			continue
		}
		rtts = append(rtts, float64(time.Since(start))/float64(time.Millisecond))
		_ = conn.Close()
	}
	if len(rtts) > 0 {
		res.latencyMs = median(rtts)
	}
	res.lossPct = float64(probeAttempts-len(rtts)) / float64(probeAttempts) * 100
	return res
}

// median returns the median of a non-empty slice (mean of the two middle
// values for even lengths).
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
