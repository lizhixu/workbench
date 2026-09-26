// Package record implements terminal session recording in asciinema cast v2
// format. It wraps a shell session's PTY output and writes a .cast file
// containing the full output timeline for later replay.
package record

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Recorder writes asciinema cast v2 output to a file.
//
// Cast v2 format:
//   line 1: {"version":2,"width":W,"height":H,"timestamp":T,"env":{"SHELL":...}}
//   line N: [elapsed_sec, "o", "output_data"]
type Recorder struct {
	mu       sync.Mutex
	file     *os.File
	writer   *bufio.Writer
	start    time.Time
	closed   bool
}

// New creates a Recorder writing to path. Header is written immediately.
// The file is created with 0600 permissions: recordings may contain
// passwords or other sensitive terminal output.
func New(path string, cols, rows int) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create cast file: %w", err)
	}
	// Tighten permissions on files created by older versions (0644).
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm() != 0o600 {
		_ = f.Chmod(0o600)
	}
	w := bufio.NewWriter(f)
	r := &Recorder{file: f, writer: w, start: time.Now()}

	header := map[string]any{
		"version":  2,
		"width":    cols,
		"height":   rows,
		"timestamp": time.Now().Unix(),
		"env": map[string]string{
			"SHELL": os.Getenv("SHELL"),
			"TERM":  "xterm-256color",
		},
	}
	hb, _ := json.Marshal(header)
	w.Write(hb)
	w.WriteByte('\n')
	w.Flush()

	return r, nil
}

// WriteOutput records a chunk of PTY output.
func (r *Recorder) WriteOutput(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	elapsed := time.Since(r.start).Seconds()
	entry := []any{elapsed, "o", string(data)}
	eb, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	r.writer.Write(eb)
	r.writer.WriteByte('\n')
	return r.writer.Flush()
}

// Close finalizes the recording and emits an EOF timing event so the cast file's
// total duration reflects the true session lifetime, preventing premature replay cutoff.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true

	// Record the final session close timestamp so asciinema-player knows the
	// complete timeline span, matching the session's recorded duration.
	elapsed := time.Since(r.start).Seconds()
	if elapsed > 0 {
		endEntry := []any{elapsed, "o", ""}
		if eb, err := json.Marshal(endEntry); err == nil {
			r.writer.Write(eb)
			r.writer.WriteByte('\n')
		}
	}

	r.writer.Flush()
	return r.file.Close()
}

// Copy wraps an io.Reader, writing output to both the Recorder and returning
// the data to the caller. It runs until the reader returns EOF or error.
func (r *Recorder) Copy(rd io.Reader, out func([]byte)) {
	buf := make([]byte, 8192)
	for {
		n, err := rd.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			_ = r.WriteOutput(data)
			if out != nil {
				out(data)
			}
		}
		if err != nil {
			return
		}
	}
}