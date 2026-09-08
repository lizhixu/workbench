package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecorderWritesHeaderAndCloseTimestamp(t *testing.T) {
	dir := t.TempDir()
	castFile := filepath.Join(dir, "test.cast")

	rec, err := New(castFile, 80, 24)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	if err := rec.WriteOutput([]byte("hello world")); err != nil {
		t.Fatalf("WriteOutput failed: %v", err)
	}

	// Sleep slightly so close timestamp is measurably later
	time.Sleep(50 * time.Millisecond)

	if err := rec.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	data, err := os.ReadFile(castFile)
	if err != nil {
		t.Fatalf("read cast failed: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines (header, output, close), got %d: %v", len(lines), lines)
	}

	// Line 1: Header
	var header map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("header unmarshal: %v", err)
	}
	if header["version"].(float64) != 2 {
		t.Errorf("version = %v, want 2", header["version"])
	}

	// Line 2: Output
	var outLine []any
	if err := json.Unmarshal([]byte(lines[1]), &outLine); err != nil {
		t.Fatalf("output unmarshal: %v", err)
	}
	if outLine[1].(string) != "o" || outLine[2].(string) != "hello world" {
		t.Errorf("output line content mismatch: %v", outLine)
	}

	// Line 3: Close EOF timestamp event
	var endLine []any
	if err := json.Unmarshal([]byte(lines[2]), &endLine); err != nil {
		t.Fatalf("endLine unmarshal: %v", err)
	}
	if endLine[1].(string) != "o" || endLine[2].(string) != "" {
		t.Errorf("endLine event mismatch: %v", endLine)
	}
	endElapsed := endLine[0].(float64)
	startElapsed := outLine[0].(float64)
	if endElapsed <= startElapsed {
		t.Errorf("endElapsed (%f) should be > startElapsed (%f)", endElapsed, startElapsed)
	}
}
