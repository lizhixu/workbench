package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeCastRecordingAppendsClosingFrame(t *testing.T) {
	raw := []byte(`{"version":2,"width":80,"height":24,"timestamp":1700000000}
[0.1,"o","prompt $ "]
[5.2,"o","ls\r\n"]
`)

	// Session actually lasted 1500 seconds (25 minutes)
	normalized := normalizeCastRecording(raw, 1500)

	lines := strings.Split(strings.TrimSpace(string(normalized)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (header + 2 outputs + 1 closing frame), got %d:\n%s", len(lines), string(normalized))
	}

	// 1. Header has duration set
	var header map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header["duration"].(float64) != 1500 {
		t.Errorf("header duration = %v, want 1500", header["duration"])
	}

	// 2. Final line is at 1500s
	var endEvt []any
	if err := json.Unmarshal([]byte(lines[3]), &endEvt); err != nil {
		t.Fatalf("unmarshal end frame: %v", err)
	}
	if endEvt[0].(float64) != 1500 {
		t.Errorf("end frame timestamp = %v, want 1500", endEvt[0])
	}
	if endEvt[1].(string) != "o" || endEvt[2].(string) != "" {
		t.Errorf("end frame content mismatch: %v", endEvt)
	}
}

func TestNormalizeCastRecordingAlreadyExtended(t *testing.T) {
	// If cast already reaches or exceeds session duration, do not append redundant frame
	raw := []byte(`{"version":2,"width":80,"height":24,"timestamp":1700000000}
[0.1,"o","prompt $ "]
[1500.0,"o",""]
`)
	normalized := normalizeCastRecording(raw, 1500)
	lines := strings.Split(strings.TrimSpace(string(normalized)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines without duplicate closing frame, got %d", len(lines))
	}
}
