package uninstall

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

func TestSafeDataDir(t *testing.T) {
	cases := []struct {
		dir  string
		want bool
	}{
		{"/var/lib/watchman", true},
		{"/opt/watchman/data", true},
		{`C:\ProgramData\watchman`, true},
		{"", false},
		{"/", false},
		{"/usr", false},
		{"/var", false},
		{"/opt", false},
		{"/tmp", false},
		{"/home/user/data", false}, // no "watchman" in path
		{"/var/lib/Watchman", true},
	}
	for _, c := range cases {
		if got := safeDataDir(c.dir); got != c.want {
			t.Errorf("safeDataDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}

// TestHandleRemovesDataDir uses a fake exit func and a temp watchman dir
// to verify the data-dir removal path without touching the real system.
func TestHandleRemovesDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "watchman-data")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	exited := make(chan int, 1)
	e := New(slog.Default(), dir)
	e.exitFunc = func(code int) { exited <- code }

	e.Handle(&agentpb.UninstallRequest{RemoveData: true})

	// Handle is async (2s grace delay); wait for the fake exit.
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for uninstall to complete")
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("data dir still exists: %s", dir)
	}
}

// TestHandleKeepsDataDir verifies remove_data=false leaves the dir alone.
func TestHandleKeepsDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "watchman-data")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	exited := make(chan int, 1)
	e := New(slog.Default(), dir)
	e.exitFunc = func(code int) { exited <- code }

	e.Handle(&agentpb.UninstallRequest{RemoveData: false})

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for uninstall to complete")
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data dir should be kept, stat err: %v", err)
	}
}
