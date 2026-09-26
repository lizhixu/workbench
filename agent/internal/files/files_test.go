package files

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

// capturingSender records messages the manager sends back to the server.
type capturingSender struct {
	mu   sync.Mutex
	msgs []*agentpb.AgentMessage
}

func (s *capturingSender) Send(msg *agentpb.AgentMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
	return true
}

func (s *capturingSender) acks() []*agentpb.Ack {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*agentpb.Ack
	for _, m := range s.msgs {
		if a := m.GetAck(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// runWithDeadline fails the test if fn does not return in time, which is how a
// lock-reentrancy regression shows up: the handler blocks forever.
func runWithDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not return within 3s (file manager is deadlocked)", what)
	}
}

// A completed chunked write must ack without deadlocking. handleWrite holds mu
// for the whole step and then acks its own result, so the ack path must not
// take mu again.
func TestChunkedWriteAcksWithoutDeadlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uploaded.txt")
	payload := []byte("watchman upload payload 上传内容")
	sum := sha256.Sum256(payload)
	expected := hex.EncodeToString(sum[:])

	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	runWithDeadline(t, "initial write op", func() {
		m.Handle(&agentpb.FileOp{
			OpId: "op1", Op: "write", Path: path,
			Overwrite: true, TotalSize: int64(len(payload)), Sha256: expected,
		})
	})

	runWithDeadline(t, "final chunk", func() {
		m.Handle(&agentpb.FileOp{
			OpId: "op1", Op: "write", Path: path,
			ChunkSeq: 0, Data: payload, Overwrite: true, TotalSize: int64(len(payload)),
		})
	})

	acks := sender.acks()
	if len(acks) != 1 {
		t.Fatalf("want exactly 1 ack for a completed upload, got %d", len(acks))
	}
	if !acks[0].GetOk() {
		t.Fatalf("upload ack reported failure: %s", acks[0].GetError())
	}
	if acks[0].GetSha256() != expected {
		t.Errorf("ack digest = %s, want %s", acks[0].GetSha256(), expected)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("written content = %q, want %q", got, payload)
	}
}

// A rejected chunk (wrong sequence) must also ack and must leave the manager
// usable for the next operation.
func TestWriteErrorAckKeepsManagerUsable(t *testing.T) {
	dir := t.TempDir()
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	runWithDeadline(t, "chunk without a session", func() {
		m.Handle(&agentpb.FileOp{OpId: "missing", Op: "write", Data: []byte("x"), ChunkSeq: 3})
	})

	runWithDeadline(t, "mkdir after failed write", func() {
		m.Handle(&agentpb.FileOp{OpId: "op2", Op: "mkdir", Path: filepath.Join(dir, "sub")})
	})

	acks := sender.acks()
	if len(acks) != 2 {
		t.Fatalf("want 2 acks (failed write + mkdir), got %d", len(acks))
	}
	if acks[0].GetOk() {
		t.Error("chunk without a write session should be rejected")
	}
	if !acks[1].GetOk() {
		t.Errorf("mkdir after a failed write should still succeed, got %s", acks[1].GetError())
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); err != nil {
		t.Errorf("mkdir did not create the directory: %v", err)
	}
}

// HandleAsync must preserve arrival order for a multi-chunk upload. Dispatching
// each message on its own goroutine loses that order and the write fails with a
// missing session or a sequence mismatch.
func TestHandleAsyncKeepsChunkOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ordered.bin")

	payload := make([]byte, 0, 12*1024)
	for i := 0; i < 12*1024; i++ {
		payload = append(payload, byte('a'+i%26))
	}
	sum := sha256.Sum256(payload)
	expected := hex.EncodeToString(sum[:])

	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	m.HandleAsync(&agentpb.FileOp{
		OpId: "up", Op: "write", Path: path,
		Overwrite: true, TotalSize: int64(len(payload)), Sha256: expected,
	})
	const chunk = 1024
	var seq uint32
	for off := 0; off < len(payload); off += chunk {
		end := off + chunk
		if end > len(payload) {
			end = len(payload)
		}
		m.HandleAsync(&agentpb.FileOp{
			OpId: "up", Op: "write", Path: path,
			ChunkSeq: seq, Data: payload[off:end], TotalSize: int64(len(payload)),
		})
		seq++
	}

	deadline := time.Now().Add(5 * time.Second)
	var acks []*agentpb.Ack
	for time.Now().Before(deadline) {
		acks = sender.acks()
		if len(acks) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(acks) != 1 {
		t.Fatalf("want exactly 1 ack for a %d-chunk upload, got %d: %+v", seq, len(acks), acks)
	}
	if !acks[0].GetOk() {
		t.Fatalf("chunked upload failed: %s", acks[0].GetError())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if len(got) != len(payload) || string(got) != string(payload) {
		t.Errorf("uploaded content differs: got %d bytes, want %d", len(got), len(payload))
	}
}

// A digest mismatch must discard the file rather than leave corrupt content.
func TestWriteDigestMismatchDiscardsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.bin")
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	payload := []byte("actual bytes")
	runWithDeadline(t, "initial write op", func() {
		m.Handle(&agentpb.FileOp{
			OpId: "op3", Op: "write", Path: path, Overwrite: true,
			TotalSize: int64(len(payload)),
			Sha256:    hex.EncodeToString(sha256.New().Sum(nil)), // digest of empty input
		})
	})
	runWithDeadline(t, "mismatching chunk", func() {
		m.Handle(&agentpb.FileOp{
			OpId: "op3", Op: "write", Path: path,
			ChunkSeq: 0, Data: payload, TotalSize: int64(len(payload)),
		})
	})

	acks := sender.acks()
	if len(acks) != 1 || acks[0].GetOk() {
		t.Fatalf("want one failing ack for a digest mismatch, got %+v", acks)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("corrupt file should have been removed, stat err = %v", err)
	}
}
