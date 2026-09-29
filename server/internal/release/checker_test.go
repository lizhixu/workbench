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

func TestPickLatestRelease(t *testing.T) {
	mk := func(tag string, draft bool) GitHubRelease {
		return GitHubRelease{TagName: tag, Draft: draft}
	}
	// Mirrors the real 2026-09-29 API response: NOT sorted by creation date.
	unordered := []GitHubRelease{
		mk("v0.1.0-beta.9", false),
		mk("v0.1.0-beta.8", false),
		mk("v0.1.0-beta.7", false),
		mk("v0.1.0-beta.11", false),
		mk("v0.1.0-beta.10", false),
	}
	got := pickLatestRelease(unordered)
	if got == nil || got.TagName != "v0.1.0-beta.11" {
		t.Fatalf("pickLatestRelease(unordered) = %v, want v0.1.0-beta.11", got)
	}

	// Drafts are skipped even when newest.
	withDraft := append([]GitHubRelease{mk("v0.1.0-beta.12", true)}, unordered...)
	got = pickLatestRelease(withDraft)
	if got == nil || got.TagName != "v0.1.0-beta.11" {
		t.Fatalf("pickLatestRelease(withDraft) = %v, want v0.1.0-beta.11", got)
	}

	// A stable release beats a newer-looking pre-release of an older core.
	mixed := []GitHubRelease{mk("v0.1.0-beta.99", false), mk("v0.1.0", false)}
	got = pickLatestRelease(mixed)
	if got == nil || got.TagName != "v0.1.0" {
		t.Fatalf("pickLatestRelease(mixed) = %v, want v0.1.0", got)
	}

	if pickLatestRelease(nil) != nil {
		t.Fatal("pickLatestRelease(nil) should be nil")
	}
	if pickLatestRelease([]GitHubRelease{mk("v0.1.0-beta.1", true)}) != nil {
		t.Fatal("pickLatestRelease(all drafts) should be nil")
	}
}
