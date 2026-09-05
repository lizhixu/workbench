package rpc

import (
	"log/slog"
	"testing"
	"time"
)

// TestListAgentsStableOrder verifies ListAgents returns a deterministic order.
// Go map iteration is randomized, so without an explicit sort the host list
// reshuffles on every refresh — the bug this guards against.
func TestListAgentsStableOrder(t *testing.T) {
	r := NewRegistry("", slog.Default())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Insert out of order; registration time should drive the result order.
	r.agents["zzz"] = &Agent{ID: "zzz", Hostname: "c", Registered: base.Add(2 * time.Minute)}
	r.agents["aaa"] = &Agent{ID: "aaa", Hostname: "a", Registered: base}
	r.agents["mmm"] = &Agent{ID: "mmm", Hostname: "b", Registered: base.Add(1 * time.Minute)}

	var first []string
	for _, a := range r.ListAgents() {
		first = append(first, a.ID)
	}
	want := []string{"aaa", "mmm", "zzz"}
	for i, id := range want {
		if first[i] != id {
			t.Fatalf("order[%d] = %q, want %q (full: %v)", i, first[i], id, first)
		}
	}

	// Repeated calls must return the identical order (map randomization must not
	// leak through).
	for iter := 0; iter < 50; iter++ {
		got := r.ListAgents()
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("iteration %d order changed: %q != %q", iter, got[i].ID, want[i])
			}
		}
	}
}

// TestListAgentsTiebreakByID checks agents with an identical (or zero)
// registration timestamp fall back to a stable ID sort rather than random map
// order.
func TestListAgentsTiebreakByID(t *testing.T) {
	r := NewRegistry("", slog.Default())
	// All zero timestamps => ID is the only tiebreaker.
	for _, id := range []string{"delta", "alpha", "charlie", "bravo"} {
		r.agents[id] = &Agent{ID: id, Hostname: id}
	}
	want := []string{"alpha", "bravo", "charlie", "delta"}
	for iter := 0; iter < 30; iter++ {
		got := r.ListAgents()
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("iteration %d: order[%d] = %q, want %q", iter, i, got[i].ID, want[i])
			}
		}
	}
}
