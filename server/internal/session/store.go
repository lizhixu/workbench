// Package session implements terminal session auditing for the control server.
//
// Every time an operator opens a Web terminal, a Session record is created
// capturing who/when/which-host. When the terminal closes, the end time and
// duration are recorded. A record still live when the server (re)starts is
// closed at load time: the PTY, the hub and the grace timer all live only in
// memory and cannot survive a restart, so such a record can never become live
// again. The agent writes an asciinema cast v2 file locally
// for each session; the server fetches it on demand via the agent's file-read
// channel and caches it under dataDir/records so the audit page can replay it
// even after the agent goes offline.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Session is an auditable terminal session record.
type Session struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Hostname  string    `json:"hostname"`
	Operator  string    `json:"operator"` // username of the console user
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Duration  int64     `json:"duration_sec"` // seconds, 0 while live
	Live      bool      `json:"live"`
	CastPath  string    `json:"cast_path"` // relative path under records dir
}

// Store persists session metadata to a JSON file and recording files to disk.
type Store struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	filePath  string
	recordDir string
}

// NewStore loads (or creates) the session store.
func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "watchman")
	}
	recordDir := filepath.Join(dataDir, "records")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		sessions:  map[string]*Session{},
		filePath:  filepath.Join(dataDir, "sessions.json"),
		recordDir: recordDir,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// RecordDir returns the directory where cast files are cached.
func (s *Store) RecordDir() string { return s.recordDir }

func (s *Store) load() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []*Session
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	swept := false
	s.mu.Lock()
	for _, sess := range list {
		if sess.Live {
			// A live record belongs to a previous process run: the PTY, the
			// hub and the grace timer are all in-memory and died with it, so
			// no session can still be live after a restart. Close it here
			// instead of leaving a "0s 进行中" row forever. The real end time
			// is unknown, so the record is closed at its start time (duration
			// stays 0) rather than inventing an end time that would inflate
			// the duration.
			sess.Live = false
			sess.EndedAt = sess.StartedAt
			sess.Duration = 0
			swept = true
		}
		s.sessions[sess.ID] = sess
	}
	s.mu.Unlock()
	if swept {
		_ = s.persist()
	}
	return nil
}

func (s *Store) persist() error {
	s.mu.RLock()
	list := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		return list[i].StartedAt.After(list[j].StartedAt)
	})
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.filePath, data, 0o600)
}

// Start records a new terminal session.
// Start records a new terminal session. It is create-if-absent: the record is
// created on the first WS attach, and a later attach (reconnect, second
// viewer) must not reset StartedAt or wipe the ended state. In particular it
// must not resurrect a record an admin deleted mid-session.
func (s *Store) Start(id, agentID, hostname, operator string) {
	s.mu.Lock()
	if _, ok := s.sessions[id]; !ok {
		s.sessions[id] = &Session{
			ID:        id,
			AgentID:   agentID,
			Hostname:  hostname,
			Operator:  operator,
			StartedAt: time.Now(),
			Live:      true,
			CastPath:  id + ".cast",
		}
	}
	s.mu.Unlock()
	_ = s.persist()
}

// End marks a session as closed. It is idempotent: the record is ended exactly
// once, at the first call. Later calls are no-ops, so a PTY exit and the later
// grace-period teardown cannot move EndedAt forward and inflate the duration.
func (s *Store) End(id string) {
	s.mu.Lock()
	if sess, ok := s.sessions[id]; ok && sess.Live {
		sess.EndedAt = time.Now()
		sess.Duration = int64(sess.EndedAt.Sub(sess.StartedAt).Seconds())
		sess.Live = false
	}
	s.mu.Unlock()
	_ = s.persist()
}

// List returns all sessions sorted by start time (newest first).
func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// Get returns a single session by ID.
func (s *Store) Get(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// SaveRecording writes the cast file content to disk.
func (s *Store) SaveRecording(id string, data []byte) error {
	path := filepath.Join(s.recordDir, id+".cast")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write cast: %w", err)
	}
	return nil
}

// RecordingPath returns the local path to a cached cast file, or "" if absent.
func (s *Store) RecordingPath(id string) string {
	path := filepath.Join(s.recordDir, id+".cast")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// Delete removes a session and its recording.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	_ = os.Remove(filepath.Join(s.recordDir, id+".cast"))
	return s.persist()
}