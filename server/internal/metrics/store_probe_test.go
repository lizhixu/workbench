package metrics

import (
	"encoding/json"
	"log/slog"
	"math"
	"reflect"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// AddSample copies the per-carrier probe results (skipping nil entries) into
// the stored point; samples without probe data keep NetProbe nil.
func TestAddSampleProbeResults(t *testing.T) {
	s := newTestStore(t)
	s.AddSample("h1", &agentpb.MetricsSample{
		Ts: 1700000000,
		ProbeResults: map[string]*agentpb.ProbeResult{
			"telecom": {LatencyMs: 12.5, LossPct: 0},
			"unicom":  {LatencyMs: 0, LossPct: 100},
			"mobile":  nil, // defensive: never expected from the agent
		},
	})
	s.AddSample("h1", &agentpb.MetricsSample{Ts: 1700000015})

	pts := s.QueryHistory("h1", 1700000000-1, 1700000015+1, 15)
	if len(pts) != 2 {
		t.Fatalf("got %d points, want 2", len(pts))
	}
	want := map[string]ProbePoint{
		"telecom": {LatencyMs: 12.5, LossPct: 0},
		"unicom":  {LatencyMs: 0, LossPct: 100},
	}
	if !reflect.DeepEqual(pts[0].NetProbe, want) {
		t.Fatalf("probe results = %v, want %v", pts[0].NetProbe, want)
	}
	if pts[1].NetProbe != nil {
		t.Fatalf("sample without probe data should have nil NetProbe, got %v", pts[1].NetProbe)
	}
	// AddSample persists asynchronously; let the writer finish so the temp
	// dir can be cleaned up without racing it.
	time.Sleep(100 * time.Millisecond)
}

// Old JSONL records without net_probe still deserialize (NetProbe nil).
func TestPointBackwardCompatibleJSON(t *testing.T) {
	line := `{"ts":1700000000,"cpu_usage":1.5,"mem_usage":42,"net_latency_ms":9}`
	var pt Point
	if err := json.Unmarshal([]byte(line), &pt); err != nil {
		t.Fatalf("unmarshal old record: %v", err)
	}
	if pt.NetProbe != nil {
		t.Fatalf("old record should have nil NetProbe, got %v", pt.NetProbe)
	}
	if pt.CPUUsage != 1.5 {
		t.Fatalf("cpu_usage = %v, want 1.5", pt.CPUUsage)
	}
}

// QueryHistory downsamples probe readings per carrier: latency averages only
// over samples that reported one (> 0), loss over every sample carrying an
// entry for that carrier, and carriers missing from a bucket stay missing.
func TestQueryHistoryProbeDownsample(t *testing.T) {
	s := newTestStore(t)
	const base = int64(1700000000) // divisible by the 40s step below
	const n = 80

	for i := 0; i < n; i++ {
		probe := map[string]ProbePoint{
			"telecom": {LatencyMs: 0, LossPct: 25}, // every other sample has no RTT
			"unicom":  {LatencyMs: 20, LossPct: 100},
		}
		if i%2 == 0 {
			probe["telecom"] = ProbePoint{LatencyMs: 10, LossPct: 25}
		}
		if i < 40 { // mobile disappears in the second bucket
			probe["mobile"] = ProbePoint{LatencyMs: 5, LossPct: 50}
		}
		s.appendToFile("h1", Point{Timestamp: base + int64(i), NetProbe: probe})
	}

	pts := s.QueryHistory("h1", base-1, base+n, 40)
	if len(pts) != 2 {
		t.Fatalf("got %d buckets, want 2", len(pts))
	}

	for i, want := range []map[string]ProbePoint{
		{
			"telecom": {LatencyMs: 10, LossPct: 25},
			"unicom":  {LatencyMs: 20, LossPct: 100},
			"mobile":  {LatencyMs: 5, LossPct: 50},
		},
		{
			"telecom": {LatencyMs: 10, LossPct: 25},
			"unicom":  {LatencyMs: 20, LossPct: 100},
		},
	} {
		got := pts[i].NetProbe
		if len(got) != len(want) {
			t.Fatalf("bucket %d: carriers %v, want %v", i, got, want)
		}
		for carrier, wp := range want {
			gp, ok := got[carrier]
			if !ok {
				t.Fatalf("bucket %d: carrier %q missing", i, carrier)
			}
			if math.Abs(gp.LatencyMs-wp.LatencyMs) > 1e-9 || math.Abs(gp.LossPct-wp.LossPct) > 1e-9 {
				t.Fatalf("bucket %d carrier %q = %+v, want %+v", i, carrier, gp, wp)
			}
		}
	}
}

// Points without any probe reading (older agents, probing off) downsample to
// a nil NetProbe instead of an empty map.
func TestQueryHistoryProbeAbsent(t *testing.T) {
	s := newTestStore(t)
	const base = int64(1700000000)
	for i := 0; i < 80; i++ {
		s.appendToFile("h1", Point{Timestamp: base + int64(i), CPUUsage: float64(i)})
	}
	pts := s.QueryHistory("h1", base-1, base+80, 40)
	if len(pts) != 2 {
		t.Fatalf("got %d buckets, want 2", len(pts))
	}
	for i, pt := range pts {
		if pt.NetProbe != nil {
			t.Fatalf("bucket %d: NetProbe = %v, want nil", i, pt.NetProbe)
		}
	}
}
