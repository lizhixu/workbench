// Package scan implements the server-side scan job store and REST handlers.
// It dispatches ScanRequest messages to agents, accumulates ScanProgress
// responses, and persists findings for later querying and AI report analysis.
package scan

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ScanJob represents a completed or in-progress security scan.
type ScanJob struct {
	ID            string    `json:"id"`
	HostID        string    `json:"host_id"`
	Hostname      string    `json:"hostname"`
	Type          string    `json:"type"` // baseline / intrusion / vuln
	Status        string    `json:"status"` // running / completed / failed
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
	FindingsCount int       `json:"findings_count"`
	FindingsJSON  []byte    `json:"findings_json,omitempty"` // raw findings array
	Progress      float64   `json:"progress"`
	Error         string    `json:"error,omitempty"`
}

// Store persists scan jobs to disk.
type Store struct {
	mu      sync.Mutex
	dataDir string
	log     *slog.Logger
	jobs    map[string]*ScanJob // keyed by job ID
	order   []string            // job IDs in creation order (for listing)
}

// NewStore creates a scan store, loading existing jobs from disk.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		dataDir: dataDir,
		log:     log,
		jobs:    make(map[string]*ScanJob),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	path := filepath.Join(s.dataDir, "scans.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var jobs []*ScanJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return fmt.Errorf("unmarshal scans.json: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range jobs {
		s.jobs[j.ID] = j
		s.order = append(s.order, j.ID)
	}
	// Cap at 500 jobs.
	if len(s.order) > 500 {
		excess := len(s.order) - 500
		for _, id := range s.order[:excess] {
			delete(s.jobs, id)
		}
		s.order = s.order[excess:]
	}
	s.log.Info("scan store loaded", "jobs", len(s.jobs))
	return nil
}

func (s *Store) persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// persistLocked writes the store to disk; caller must hold s.mu.
func (s *Store) persistLocked() error {
	jobs := make([]*ScanJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	// Sort by started_at descending for readability.
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].StartedAt.After(jobs[j].StartedAt)
	})
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dataDir, "scans.json")
	return os.WriteFile(path, data, 0o600)
}

// CreateJob registers a new scan job.
func (s *Store) CreateJob(id, hostID, hostname, scanType string) *ScanJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := &ScanJob{
		ID:        id,
		HostID:    hostID,
		Hostname:  hostname,
		Type:      scanType,
		Status:    "running",
		StartedAt: time.Now(),
		Progress:  0,
	}
	s.jobs[id] = job
	s.order = append(s.order, id)
	// Cap stored jobs to avoid unbounded growth.
	if len(s.order) > 500 {
		drop := s.order[0]
		delete(s.jobs, drop)
		s.order = s.order[1:]
	}
	_ = s.persistLocked()
	return job
}

// UpdateProgress updates a running scan job with progress data.
func (s *Store) UpdateProgress(id string, progress float64, done bool, findingsJSON []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return
	}
	job.Progress = progress
	if done {
		job.Status = "completed"
		job.FinishedAt = time.Now()
		job.FindingsJSON = findingsJSON
		// Count findings.
		var findings []map[string]any
		if json.Unmarshal(findingsJSON, &findings) == nil {
			job.FindingsCount = len(findings)
		}
	}
	_ = s.persistLocked()
}

// FailJob marks a scan job as failed.
func (s *Store) FailJob(id, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return
	}
	job.Status = "failed"
	job.FinishedAt = time.Now()
	job.Error = errMsg
	_ = s.persistLocked()
}

// GetJob returns a scan job by ID.
func (s *Store) GetJob(id string) *ScanJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j, ok := s.jobs[id]; ok {
		// Return a copy to avoid races.
		cp := *j
		return &cp
	}
	return nil
}

// ListJobs returns all scan jobs, optionally filtered.
func (s *Store) ListJobs(hostID, scanType, status string) []*ScanJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*ScanJob
	for _, j := range s.jobs {
		if hostID != "" && j.HostID != hostID {
			continue
		}
		if scanType != "" && j.Type != scanType {
			continue
		}
		if status != "" && j.Status != status {
			continue
		}
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// LatestScanForHost returns the most recent completed scan for a host.
func (s *Store) LatestScanForHost(hostID string) *ScanJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *ScanJob
	for _, j := range s.jobs {
		if j.HostID != hostID {
			continue
		}
		if best == nil || j.StartedAt.After(best.StartedAt) {
			cp := *j
			best = &cp
		}
	}
	return best
}