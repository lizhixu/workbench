package sysinfo

import (
	"sort"
	"testing"
)

func TestParseLast_Sessions(t *testing.T) {
	in := `root     pts/0        61.138.254.239   Sun Aug 23 21:44   still logged in
root     pts/1        61.138.254.239   Thu Aug 20 12:21 - 14:57  (02:35)
root     pts/0        61.138.254.239   Thu Aug 20 12:08 - 12:46  (00:38)
wtmp begins Fri Apr 25 15:13:00 2025
`
	entries := parseLastOutput(in)
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries (3 sessions + wtmp), got %d", len(entries))
	}

	got := entries[0]
	if got.Type != "session" || got.User != "root" || got.Tty != "pts/0" ||
		got.From != "61.138.254.239" || got.Detail != "still logged in" {
		t.Fatalf("session row wrong: %+v", got)
	}
	if got.Started == "" {
		t.Fatalf("session row missing started timestamp: %+v", got)
	}

	if entries[1].Duration != "(02:35)" {
		t.Fatalf("expected duration (02:35), got %q", entries[1].Duration)
	}

	if entries[3].Type != "wtmp" {
		t.Fatalf("expected wtmp sentinel, got %+v", entries[3])
	}
}

func TestParseLast_RebootAndShutdown(t *testing.T) {
	in := `reboot   system boot  6.1.0-10-amd64   Sun Aug 23 21:43   still running
reboot   system boot  6.1.0-10-amd64   Thu Aug 20 21:15   still running
shutdown system down  6.1.0-10-amd64   Thu Aug 20 21:14 - 21:15  (00:01)
`
	entries := parseLastOutput(in)
	if len(entries) != 3 {
		t.Fatalf("expected 3 reboot/shutdown entries, got %d", len(entries))
	}
	if entries[0].Type != "reboot" || entries[0].Kernel != "6.1.0-10-amd64" {
		t.Fatalf("reboot row wrong: %+v", entries[0])
	}
	if entries[2].Type != "shutdown" {
		t.Fatalf("shutdown row wrong: %+v", entries[2])
	}
}

func TestParseLast_EmptyAndMalformed(t *testing.T) {
	if got := parseLastOutput(""); len(got) != 0 {
		t.Fatalf("expected empty output to yield no entries, got %d", len(got))
	}
	if got := parseLastOutput("\n\n\n"); len(got) != 0 {
		t.Fatalf("blank lines should yield no entries, got %d", len(got))
	}
	// A single junk line should not panic and should be skipped gracefully.
	if got := parseLastOutput("?????malformed\n"); len(got) != 0 {
		t.Fatalf("malformed line should be skipped, got %d", len(got))
	}
}

// sortedForCompare sorts entries by (started, user) to make assertions stable
// across platforms where `last` may not produce stable order.
func sortedForCompare(entries []loginEntry) []loginEntry {
	out := append([]loginEntry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Started < out[j].Started
	})
	return out
}
