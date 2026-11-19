package files

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func (s *capturingSender) chunks() []*agentpb.FileChunk {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*agentpb.FileChunk
	for _, m := range s.msgs {
		if c := m.GetFileChunk(); c != nil {
			out = append(out, c)
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

// readTarGz returns the entries of a .tar.gz archive: regular-file contents
// by name plus the full name list (directories included).
func readTarGz(t *testing.T, archive string) (map[string]string, []string) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	files := map[string]string{}
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read tar entry %s: %v", hdr.Name, err)
			}
			files[hdr.Name] = string(b)
		}
	}
	return files, names
}

// compressResult runs one compress op and returns the archive path from the
// single JSON chunk, or fails the test on an ack error.
func compressResult(t *testing.T, m *Manager, sender *capturingSender, opID, path string) (string, int64) {
	t.Helper()
	runWithDeadline(t, "compress op", func() {
		m.Handle(&agentpb.FileOp{OpId: opID, Op: "compress", Path: path})
	})
	if acks := sender.acks(); len(acks) != 0 {
		t.Fatalf("compress acked instead of chunking: %+v", acks)
	}
	chunks := sender.chunks()
	if len(chunks) != 1 {
		t.Fatalf("want exactly 1 result chunk, got %d", len(chunks))
	}
	if !chunks[0].GetEof() {
		t.Fatalf("result chunk must be the final one")
	}
	var res struct {
		Archive string `json:"archive"`
		Size    int64  `json:"size"`
	}
	if err := json.Unmarshal(chunks[0].GetData(), &res); err != nil {
		t.Fatalf("result chunk is not JSON: %v", err)
	}
	if res.Archive == "" || res.Size <= 0 {
		t.Fatalf("bad compress result: %+v", res)
	}
	return res.Archive, res.Size
}

func TestHandleChmodAppliesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	runWithDeadline(t, "chmod op", func() {
		m.Handle(&agentpb.FileOp{OpId: "c1", Op: "chmod", Path: path, Mode: 0600})
	})
	acks := sender.acks()
	if len(acks) != 1 || !acks[0].GetOk() {
		t.Fatalf("chmod should ack ok, got %+v", acks)
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0600 {
		t.Errorf("mode after chmod = %o, want 600", st.Mode().Perm())
	}
}

func TestHandleChmodRejectsBadModeAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	runWithDeadline(t, "chmod with mode above 07777", func() {
		m.Handle(&agentpb.FileOp{OpId: "c2", Op: "chmod", Path: filepath.Join(dir, "x"), Mode: 010000})
	})
	runWithDeadline(t, "chmod on a missing file", func() {
		m.Handle(&agentpb.FileOp{OpId: "c3", Op: "chmod", Path: filepath.Join(dir, "nope"), Mode: 0644})
	})
	acks := sender.acks()
	if len(acks) != 2 {
		t.Fatalf("want 2 failing acks, got %+v", acks)
	}
	for i, a := range acks {
		if a.GetOk() {
			t.Errorf("ack %d should have failed", i)
		}
	}
}

func TestHandleCompressSingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	archive, size := compressResult(t, m, sender, "z1", path)
	if archive != filepath.Join(dir, "a.txt.tar.gz") {
		t.Errorf("archive = %s, want %s", archive, filepath.Join(dir, "a.txt.tar.gz"))
	}
	if st, err := os.Stat(archive); err != nil || st.Size() != size {
		t.Errorf("archive stat size = %v, want %d", st, size)
	}
	files, _ := readTarGz(t, archive)
	if got, ok := files["a.txt"]; !ok || got != "hello" {
		t.Errorf("archive content = %v, want a.txt=hello", files)
	}
}

func TestHandleCompressDirectory(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "f.txt"), []byte("f"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "sub", "g.txt"), []byte("g"), 0644); err != nil {
		t.Fatal(err)
	}
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	archive, _ := compressResult(t, m, sender, "z2", data)
	files, names := readTarGz(t, archive)
	if files["data/f.txt"] != "f" || files["data/sub/g.txt"] != "g" {
		t.Errorf("archive files = %v", files)
	}
	want := map[string]bool{"data/": true, "data/f.txt": true, "data/sub/": true, "data/sub/g.txt": true}
	for _, n := range names {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Errorf("missing archive entries: %v (have %v)", want, names)
	}
}

func TestHandleCompressNameCollisionAppendsSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	// A stale archive already occupies the natural name.
	if err := os.WriteFile(filepath.Join(dir, "a.txt.tar.gz"), []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	archive, _ := compressResult(t, m, sender, "z3", path)
	if archive != filepath.Join(dir, "a.txt-1.tar.gz") {
		t.Errorf("archive = %s, want a.txt-1.tar.gz", archive)
	}
}

func TestHandleCompressRefusesRootAndMissingSource(t *testing.T) {
	dir := t.TempDir()
	sender := &capturingSender{}
	m := NewManager(nil)
	m.SetSender(sender)

	runWithDeadline(t, "compress /", func() {
		m.Handle(&agentpb.FileOp{OpId: "z4", Op: "compress", Path: "/"})
	})
	runWithDeadline(t, "compress a missing path", func() {
		m.Handle(&agentpb.FileOp{OpId: "z5", Op: "compress", Path: filepath.Join(dir, "nope")})
	})
	acks := sender.acks()
	if len(acks) != 2 {
		t.Fatalf("want 2 failing acks, got %+v", acks)
	}
	for i, a := range acks {
		if a.GetOk() {
			t.Errorf("ack %d should have failed", i)
		}
	}
	if acks[0].GetError() == "" || !strings.Contains(acks[0].GetError(), "根目录") {
		t.Errorf("root compress error should mention 根目录, got %q", acks[0].GetError())
	}
}
