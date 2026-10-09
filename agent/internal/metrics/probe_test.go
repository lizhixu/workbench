package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A reachable target yields a positive latency and zero loss.
func TestProbeRoundReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := runProbeRound(&http.Client{Timeout: probeTimeout}, srv.URL)
	if !res.done {
		t.Fatal("round should be marked done")
	}
	if res.lossPct != 0 {
		t.Fatalf("loss = %v, want 0", res.lossPct)
	}
	if res.latencyMs <= 0 {
		t.Fatalf("latency = %v, want > 0", res.latencyMs)
	}
}

// HTTP 5xx counts as failure: full loss and no latency data (0).
func TestProbeRoundServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	res := runProbeRound(&http.Client{Timeout: probeTimeout}, srv.URL)
	if res.lossPct != 100 {
		t.Fatalf("loss = %v, want 100", res.lossPct)
	}
	if res.latencyMs != 0 {
		t.Fatalf("latency = %v, want 0 (no data)", res.latencyMs)
	}
}

// An unreachable target (connection refused) yields full loss.
func TestProbeRoundUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens any more

	res := runProbeRound(&http.Client{Timeout: probeTimeout}, url)
	if res.lossPct != 100 || res.latencyMs != 0 {
		t.Fatalf("got latency=%v loss=%v, want 0/100", res.latencyMs, res.lossPct)
	}
}

// setURL starts probing, publishes a result, and clearing the URL resets.
func TestProberLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newProber()
	if p.latest().done {
		t.Fatal("fresh prober should have no data")
	}
	p.setURL(srv.URL)
	deadline := time.Now().Add(5 * time.Second)
	for !p.latest().done && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := p.latest()
	if !got.done || got.lossPct != 0 || got.latencyMs <= 0 {
		t.Fatalf("after round: %+v, want done with latency>0 loss=0", got)
	}

	p.setURL("")
	if p.latest().done {
		t.Fatal("clearing the URL should reset the result")
	}
	// Re-setting the same (empty) URL must be a no-op, not a panic.
	p.setURL("")
}
