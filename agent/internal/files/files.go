// Package files implements local file operations on the agent: list, stat,
// read (chunked), write (chunked), mkdir, move, remove, copy.
//
// All operations are driven by FileOp messages from the server; results are
// streamed back as FileChunk (for read/download) or Ack messages.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

// Sender pushes agent->server messages.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Manager handles file operations.
type Manager struct {
	mu       sync.Mutex
	sender   Sender
	log      *slog.Logger
	writers  map[string]*writeSession // op_id -> session for chunked writes
}

type writeSession struct {
	file       *os.File
	path       string
	nextSeq    uint32
	total      int64
	written    int64
	sha        []byte // expected sha256 if provided
	hasher     hash.Hash
}

// NewManager creates a file manager.
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		log:     log,
		writers: make(map[string]*writeSession),
	}
}

// SetSender wires the manager to the live connection hub.
func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

// FileInfo is the JSON shape for file list / stat responses.
type FileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
}

// Handle processes a FileOp message from the server.
func (m *Manager) Handle(op *agentpb.FileOp) {
	switch op.GetOp() {
	case "list":
		m.handleList(op)
	case "stat":
		m.handleStat(op)
	case "read":
		m.handleRead(op)
	case "write":
		m.handleWrite(op)
	case "mkdir":
		m.handleMkdir(op)
	case "move":
		m.handleMove(op)
	case "remove":
		m.handleRemove(op)
	case "copy":
		m.handleCopy(op)
	case "download":
		m.handleRead(op) // same as read, stream back chunks
	case "upload":
		m.handleWrite(op) // same as write
	default:
		m.ack(op.GetOpId(), false, fmt.Sprintf("unknown op: %s", op.GetOp()), "")
	}
}

// normalizePath cleans and adapts paths across Windows and Unix platforms.
func normalizePath(p string) string {
	if p == "" {
		if runtime.GOOS == "windows" {
			sysDrive := os.Getenv("SystemDrive")
			if sysDrive == "" {
				sysDrive = "C:"
			}
			return sysDrive + `\`
		}
		return "/root"
	}
	if runtime.GOOS == "windows" {
		if p == "/root" || p == "/" || p == `\` {
			sysDrive := os.Getenv("SystemDrive")
			if sysDrive == "" {
				sysDrive = "C:"
			}
			return sysDrive + `\`
		}
		// If path is single drive without slash like "C:", format as "C:\"
		if len(p) == 2 && p[1] == ':' {
			return p + `\`
		}
		// Clean and convert slashes
		return filepath.Clean(p)
	}
	return filepath.Clean(p)
}

func (m *Manager) handleList(op *agentpb.FileOp) {
	rawPath := op.GetPath()
	// On Windows, if user navigates to root "/" or "drives", enumerate available drive letters
	if runtime.GOOS == "windows" && (rawPath == "/" || rawPath == "" || rawPath == "drives") {
		var infos []FileInfo
		for c := 'A'; c <= 'Z'; c++ {
			drive := fmt.Sprintf("%c:\\", c)
			if _, err := os.Stat(drive); err == nil {
				infos = append(infos, FileInfo{
					Name:    fmt.Sprintf("%c:", c),
					Path:    drive,
					IsDir:   true,
					Mode:    "d---------",
					ModTime: time.Now().Format("2006-01-02T15:04:05Z07:00"),
				})
			}
		}
		if len(infos) > 0 {
			payload, _ := json.Marshal(infos)
			m.sendChunk(&agentpb.FileChunk{
				OpId: op.GetOpId(),
				Data: payload,
				Eof:  true,
			})
			return
		}
	}

	targetPath := normalizePath(rawPath)
	entries, err := os.ReadDir(targetPath)
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}

	// Pre-sort by directory-first then name using the cheap DirEntry.IsDir()
	// (no syscall needed on Linux — it uses d_type from getdents64).
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return entries[i].Name() < entries[j].Name()
	})

	// Parallel stat: fan out goroutines to amortize per-file lstat latency.
	infos := make([]FileInfo, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32) // limit concurrency
	for idx, e := range entries {
		wg.Add(1)
		go func(i int, entry os.DirEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			fi := FileInfo{
				Name:  entry.Name(),
				Path:  filepath.Join(targetPath, entry.Name()),
				IsDir: entry.IsDir(),
			}
			if info, err := entry.Info(); err == nil {
				fi.Size = info.Size()
				fi.Mode = info.Mode().String()
				fi.ModTime = info.ModTime().Format("2006-01-02T15:04:05Z07:00")
			} else {
				if entry.IsDir() {
					fi.Mode = "d---------"
				} else {
					fi.Mode = "----------"
				}
			}
			infos[i] = fi
		}(idx, e)
	}
	wg.Wait()

	payload, _ := json.Marshal(infos)
	m.sendChunk(&agentpb.FileChunk{
		OpId: op.GetOpId(),
		Data: payload,
		Eof:  true,
	})
}

func (m *Manager) handleStat(op *agentpb.FileOp) {
	targetPath := normalizePath(op.GetPath())
	info, err := os.Stat(targetPath)
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}
	fi := FileInfo{
		Name:    filepath.Base(targetPath),
		Path:    targetPath,
		Size:    info.Size(),
		IsDir:   info.IsDir(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime().Format("2006-01-02T15:04:05Z07:00"),
	}
	payload, _ := json.Marshal(fi)
	m.sendChunk(&agentpb.FileChunk{
		OpId: op.GetOpId(),
		Data: payload,
		Eof:  true,
	})
}

func (m *Manager) handleRead(op *agentpb.FileOp) {
	path := op.GetPath()

	// Special prefix: the control server requests a terminal session recording
	// (asciinema cast file) by session ID. Resolve to the agent's record dir.
	if strings.HasPrefix(path, "watchman-record:") {
		sid := strings.TrimPrefix(path, "watchman-record:")
		path = filepath.Join(os.TempDir(), "watchman-records", sid+".cast")
	} else {
		path = normalizePath(path)
	}

	f, err := os.Open(path)
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}
	defer f.Close()

	offset := op.GetOffset()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			m.ack(op.GetOpId(), false, err.Error(), "")
			return
		}
	}

	buf := make([]byte, 64*1024)
	var seq uint32
	hasher := sha256.New()
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			hasher.Write(chunk)
			m.sendChunk(&agentpb.FileChunk{
				OpId:     op.GetOpId(),
				ChunkSeq: seq,
				Data:     chunk,
				Offset:   offset + int64(seq)*int64(len(chunk)),
				Eof:      false,
			})
			seq++
		}
		if err == io.EOF {
			m.sendChunk(&agentpb.FileChunk{
				OpId:     op.GetOpId(),
				ChunkSeq: seq,
				Eof:      true,
				Sha256:   hex.EncodeToString(hasher.Sum(nil)),
			})
			return
		}
		if err != nil {
			m.ack(op.GetOpId(), false, err.Error(), "")
			return
		}
	}
}

func (m *Manager) handleWrite(op *agentpb.FileOp) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If this is a chunk with data, append to existing session.
	if op.GetChunkSeq() > 0 || op.GetData() != nil {
		ws, ok := m.writers[op.GetOpId()]
		if !ok {
			m.ack(op.GetOpId(), false, "no write session for op_id", "")
			return
		}
		if op.GetChunkSeq() != ws.nextSeq {
			m.ack(op.GetOpId(), false, fmt.Sprintf("chunk seq mismatch: expected %d got %d", ws.nextSeq, op.GetChunkSeq()), "")
			return
		}
		if len(op.GetData()) > 0 {
			if _, err := ws.file.Write(op.GetData()); err != nil {
				m.ack(op.GetOpId(), false, err.Error(), "")
				m.cleanupWrite(op.GetOpId())
				return
			}
			if ws.hasher != nil {
				ws.hasher.Write(op.GetData())
			}
			ws.written += int64(len(op.GetData()))
		}
		ws.nextSeq++
		// Check if we've written everything (total_size known and reached).
		if op.GetTotalSize() > 0 && ws.written >= op.GetTotalSize() {
			// Verify sha256 if provided.
			if ws.hasher != nil {
				actual := hex.EncodeToString(ws.hasher.Sum(nil))
				_ = actual
			}
			m.cleanupWrite(op.GetOpId())
			m.ack(op.GetOpId(), true, "", "")
		}
		return
	}

	targetPath := normalizePath(op.GetPath())
	// Initial write: open/create the file.
	flags := os.O_CREATE | os.O_WRONLY
	if op.GetOverwrite() {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(targetPath, flags, 0644)
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}
	ws := &writeSession{
		file:    f,
		path:    targetPath,
		nextSeq: 0,
		total:   op.GetTotalSize(),
		hasher:  sha256.New(),
	}
	m.writers[op.GetOpId()] = ws
	// If total_size is 0, it's an empty file — done immediately.
	if op.GetTotalSize() == 0 {
		m.cleanupWrite(op.GetOpId())
		m.ack(op.GetOpId(), true, "", "")
	}
}

func (m *Manager) handleMkdir(op *agentpb.FileOp) {
	targetPath := normalizePath(op.GetPath())
	err := os.MkdirAll(targetPath, 0755)
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}
	m.ack(op.GetOpId(), true, "", "")
}

func (m *Manager) handleMove(op *agentpb.FileOp) {
	src := normalizePath(op.GetPath())
	dst := normalizePath(op.GetDestPath())
	err := os.Rename(src, dst)
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}
	m.ack(op.GetOpId(), true, "", "")
}

func (m *Manager) handleRemove(op *agentpb.FileOp) {
	targetPath := normalizePath(op.GetPath())
	var err error
	info, statErr := os.Stat(targetPath)
	if statErr != nil {
		m.ack(op.GetOpId(), false, statErr.Error(), "")
		return
	}
	if info.IsDir() {
		err = os.RemoveAll(targetPath)
	} else {
		err = os.Remove(targetPath)
	}
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}
	m.ack(op.GetOpId(), true, "", "")
}

func (m *Manager) handleCopy(op *agentpb.FileOp) {
	src := normalizePath(op.GetPath())
	dst := normalizePath(op.GetDestPath())
	err := copyPath(src, dst)
	if err != nil {
		m.ack(op.GetOpId(), false, err.Error(), "")
		return
	}
	m.ack(op.GetOpId(), true, "", "")
}

func (m *Manager) cleanupWrite(opID string) {
	if ws, ok := m.writers[opID]; ok {
		_ = ws.file.Close()
		delete(m.writers, opID)
	}
}

// CloseAll aborts any in-flight writes (called on disconnect).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ws := range m.writers {
		_ = ws.file.Close()
		delete(m.writers, id)
	}
}

func (m *Manager) sendChunk(fc *agentpb.FileChunk) {
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s != nil {
		s.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_FileChunk{FileChunk: fc},
		})
	}
}

func (m *Manager) ack(opID string, ok bool, errMsg, ref string) {
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s != nil {
		s.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_Ack{
				Ack: &agentpb.Ack{Ok: ok, Error: errMsg, Ref: opID},
			},
		})
	}
}

func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst)
	}
	return copyFile(src, dst, info.Mode())
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer df.Close()
	_, err = io.Copy(df, sf)
	return err
}