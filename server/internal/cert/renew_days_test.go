package cert

import "testing"

// renewBeforeDaysOrDefault drives renewExpiring's threshold; the provider is
// injected by the server entrypoint from the settings store.
func TestRenewBeforeDaysProvider(t *testing.T) {
	h, err := NewHub(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.renewBeforeDaysOrDefault(); got != 30 {
		t.Fatalf("default = %d, want 30", got)
	}

	h.SetRenewBeforeDaysProvider(func() int { return 45 })
	if got := h.renewBeforeDaysOrDefault(); got != 45 {
		t.Fatalf("custom = %d, want 45", got)
	}

	// Out-of-range provider values fall back to the default instead of
	// producing a nonsense threshold.
	h.SetRenewBeforeDaysProvider(func() int { return 0 })
	if got := h.renewBeforeDaysOrDefault(); got != 30 {
		t.Fatalf("zero provider = %d, want 30", got)
	}
	h.SetRenewBeforeDaysProvider(func() int { return 365 })
	if got := h.renewBeforeDaysOrDefault(); got != 30 {
		t.Fatalf("oversized provider = %d, want 30", got)
	}

	// Nil restores the default.
	h.SetRenewBeforeDaysProvider(nil)
	if got := h.renewBeforeDaysOrDefault(); got != 30 {
		t.Fatalf("nil provider = %d, want 30", got)
	}
}
