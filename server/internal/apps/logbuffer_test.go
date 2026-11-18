package apps

import (
	"strings"
	"testing"

	"watchman/proto/agentpb"
)

// TestLogBufferStreamedTailNoDuplication: chunks that arrived live plus the
// final ExecResult must produce each output byte exactly once. The streamed
// prefix is already in the log; WriteTail appends only the remainder.
func TestLogBufferStreamedTailNoDuplication(t *testing.T) {
	lb := &logBuffer{}
	lb.BeginStream()
	lb.WriteChunk("stdout", []byte("line1\n"))
	lb.WriteChunk("stdout", []byte("line2\n"))
	lb.WriteChunk("stderr", []byte("warn\n"))
	lb.WriteTail(&agentpb.ExecResult{
		Stdout: []byte("line1\nline2\n"),
		Stderr: []byte("warn\n"),
	})
	if got := lb.String(); got != "line1\nline2\n[stderr]\nwarn\n" {
		t.Errorf("log = %q, want %q", got, "line1\nline2\n[stderr]\nwarn\n")
	}
}

// TestLogBufferTailWithoutChunks: an old agent that never streams still
// lands its whole output through WriteTail, in the same shape WriteResult
// used to produce.
func TestLogBufferTailWithoutChunks(t *testing.T) {
	lb := &logBuffer{}
	lb.BeginStream()
	lb.WriteTail(&agentpb.ExecResult{
		Stdout: []byte("hello\n"),
		Stderr: []byte("boom\n"),
	})
	if got := lb.String(); got != "hello\n\n[stderr]\nboom\n\n" {
		t.Errorf("log = %q, want %q", got, "hello\n\n[stderr]\nboom\n\n")
	}
}

// TestLogBufferTailOnlyAppendsUnstreamedPart: the last partial batch (and
// any bytes the agent flushed after its final chunk) must still appear,
// once, after the streamed prefix.
func TestLogBufferTailOnlyAppendsUnstreamedPart(t *testing.T) {
	lb := &logBuffer{}
	lb.BeginStream()
	lb.WriteChunk("stdout", []byte("line1\n"))
	lb.WriteTail(&agentpb.ExecResult{Stdout: []byte("line1\nline2\n")})
	if got := lb.String(); got != "line1\nline2\n\n" {
		t.Errorf("log = %q, want %q", got, "line1\nline2\n\n")
	}
}

// TestLogBufferWriteResultResetsCounters: a step that logs its result
// directly (the redacted git fetch) must not inherit the previous step's
// streamed counts — otherwise its output would be swallowed as "already
// streamed".
func TestLogBufferWriteResultResetsCounters(t *testing.T) {
	lb := &logBuffer{}
	lb.BeginStream()
	lb.WriteChunk("stdout", []byte("old-step-output\n"))
	res := &agentpb.ExecResult{Stdout: []byte("new-step-output\n")}
	lb.WriteResult(res, nil, nil)
	if got := lb.String(); !strings.Contains(got, "new-step-output\n") {
		t.Errorf("log = %q, want it to contain the direct WriteResult output", got)
	}
}

// TestLogBufferEmptyChunksAreNoops: an empty chunk must not emit a stray
// "[stderr]" marker or a blank line.
func TestLogBufferEmptyChunksAreNoops(t *testing.T) {
	lb := &logBuffer{}
	lb.BeginStream()
	lb.WriteChunk("stdout", nil)
	lb.WriteChunk("stderr", []byte{})
	lb.WriteTail(&agentpb.ExecResult{})
	if got := lb.String(); got != "" {
		t.Errorf("log = %q, want empty", got)
	}
}
