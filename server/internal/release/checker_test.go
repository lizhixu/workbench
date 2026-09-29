package release

import (
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		v1   string
		v2   string
		want int
	}{
		{"v0.1.0-beta.6", "v0.1.0-beta.7", -1},
		{"v0.1.0-beta.7", "v0.1.0-beta.6", 1},
		{"v0.1.0-beta.7", "v0.1.0-beta.7", 0},
		{"0.1.0-beta.7", "v0.1.0-beta.7", 0},
		{"0.1.0-dev", "v0.1.0-beta.7", -1},
		{"v0.1.0-beta.7", "0.1.0-dev", 1},
		{"", "v0.1.0-beta.7", -1},
		{"v0.1.0-beta.7", "v0.1.0", -1}, // pre-release is older than standard release
		{"v0.1.0", "v0.1.0-beta.7", 1},
		{"v0.1.0", "v0.1.1", -1},
		{"v0.2.0", "v0.1.9", 1},
		{"v1.0.0-rc.1", "v1.0.0-rc.2", -1},
		{"v1.0.0-beta.10", "v1.0.0-beta.9", 1}, // numeric comparison 10 > 9
	}

	for _, tt := range tests {
		got := CompareVersions(tt.v1, tt.v2)
		if got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.v1, tt.v2, got, tt.want)
		}
	}
}
