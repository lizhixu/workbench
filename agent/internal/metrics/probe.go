package metrics

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Network quality probe: periodically issues a few lightweight HTTP(S)
// requests to a panel-configured target and derives latency + packet loss.
// It runs on its own goroutine so metrics collection never blocks on the
// network; collect() only reads the latest completed round.
//
// Result semantics (MetricsSample.net_latency_ms / net_loss_pct):
//   - latency = mean round-trip time of the successful attempts (ms);
//     0 when no attempt succeeded or no round has completed (no data).
//   - loss = share of failed attempts in the round (percent 0..100);
//     an attempt fails on transport error or HTTP status >= 500.
const (
	probeAttempts   = 4
	probeTimeout    = 3 * time.Second
	probeInterval   = 30 * time.Second
	probeAttemptGap = 200 * time.Millisecond
	probeBodyCap    = 64 << 10
)

type probeResult struct {
	latencyMs float64
	lossPct   float64
	done      bool // at least one round completed since (re)configuration
}

type prober struct {
	mu     sync.Mutex
	client *http.Client
	url    string
	result probeResult
	cancel context.CancelFunc
}

func newProber() *prober {
	return &prober{client: &http.Client{Timeout: probeTimeout}}
}

// setURL reconfigures the probe target. Empty stops probing (old server or
// probing disabled); a changed URL restarts the round loop. Safe to call
// on every (re)connect.
func (p *prober) setURL(url string) {
	p.mu.Lock()
	if url == p.url {
		p.mu.Unlock()
		return
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.url = url
	p.result = probeResult{}
	if url == "" {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()
	go p.loop(ctx, url)
}

func (p *prober) loop(ctx context.Context, url string) {
	p.runOnce(url)
	t := time.NewTicker(probeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.runOnce(url)
		}
	}
}

func (p *prober) runOnce(url string) {
	res := runProbeRound(p.client, url)
	p.mu.Lock()
	// Only publish if this round still belongs to the current target.
	if p.url == url {
		p.result = res
	}
	p.mu.Unlock()
}

// latest returns the most recent round. done=false means no usable data
// (probing off, or the first round has not finished yet).
func (p *prober) latest() probeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}

// runProbeRound performs one round of attempts against url and reduces it
// to a latency/loss pair. Exposed for tests.
func runProbeRound(client *http.Client, url string) probeResult {
	res := probeResult{done: true}
	var latSum float64
	var okCount int
	for i := 0; i < probeAttempts; i++ {
		if i > 0 {
			time.Sleep(probeAttemptGap)
		}
		start := time.Now()
		if probeAttempt(client, url) {
			latSum += float64(time.Since(start).Microseconds()) / 1000.0
			okCount++
		}
	}
	if okCount > 0 {
		res.latencyMs = latSum / float64(okCount)
	}
	res.lossPct = float64(probeAttempts-okCount) / float64(probeAttempts) * 100
	return res
}

func probeAttempt(client *http.Client, url string) bool {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Watchman-Agent/probe")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Drain a capped prefix so the measured time covers a real transfer
	// (and the connection stays reusable) without downloading big pages.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyCap))
	return resp.StatusCode < 500
}
