package api

import (
	"testing"
	"time"
)

func TestMonthCycleStart(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name     string
		resetDay int
		now      time.Time
		want     time.Time
	}{
		{"reset day 1", 1, time.Date(2026, 11, 18, 12, 0, 0, 0, utc), time.Date(2026, 11, 1, 0, 0, 0, 0, utc)},
		{"reset day already passed", 15, time.Date(2026, 11, 18, 12, 0, 0, 0, utc), time.Date(2026, 11, 15, 0, 0, 0, 0, utc)},
		{"reset day today", 18, time.Date(2026, 11, 18, 12, 0, 0, 0, utc), time.Date(2026, 11, 18, 0, 0, 0, 0, utc)},
		{"reset day not yet this month", 20, time.Date(2026, 11, 18, 12, 0, 0, 0, utc), time.Date(2026, 10, 20, 0, 0, 0, 0, utc)},
		{"january rolls back to december", 20, time.Date(2026, 1, 5, 12, 0, 0, 0, utc), time.Date(2025, 12, 20, 0, 0, 0, 0, utc)},
		{"reset day clamped to 28", 31, time.Date(2026, 3, 5, 12, 0, 0, 0, utc), time.Date(2026, 2, 28, 0, 0, 0, 0, utc)},
		{"reset day clamped to 1", 0, time.Date(2026, 11, 18, 12, 0, 0, 0, utc), time.Date(2026, 11, 1, 0, 0, 0, 0, utc)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := monthCycleStart(tc.resetDay, tc.now); !got.Equal(tc.want) {
				t.Fatalf("monthCycleStart(%d, %v) = %v, want %v", tc.resetDay, tc.now, got, tc.want)
			}
		})
	}
}
