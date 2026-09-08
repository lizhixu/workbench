// Package snapshots implements host directory / Docker volume / database
// hot-backup for managed hosts (阶段 3). Backup jobs describe WHAT to archive;
// the engine executes them through the agent gRPC channel and archives land
// on the managed host itself under /var/backups/watchman with a retention
// policy. Metadata mirrors the control-plane backup module's safety design:
// restoring takes a safety copy of the current state first.
package snapshots

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

// JobKind describes what a backup job archives.
type JobKind string

const (
	// KindDir packs a host directory tree (tar.gz).
	KindDir JobKind = "dir"
	// KindVolume packs a named Docker volume's data directory.
	KindVolume JobKind = "volume"
	// KindDatabase hot-dumps a database running in a container
	// (mysql / postgres / redis / mongo).
	KindDatabase JobKind = "database"
)

// Schedule describes when a job runs. Cron is a 5-field expression; an empty
// cron with Manual=true means the job only runs when triggered by hand.
type Schedule struct {
	Cron   string `json:"cron,omitempty"`
	Manual bool   `json:"manual"`
}

// Job is one backup definition bound to a host.
type Job struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	HostID string  `json:"host_id"`
	Kind   JobKind `json:"kind"`
	// Target: dir path, volume name, or container name (database).
	Target string `json:"target"`
	// DBType for KindDatabase: mysql | postgres | redis | mongo.
	DBType string `json:"db_type,omitempty"`
	// DBName limits the dump to one database (mysql/postgres/mongo).
	DBName string `json:"db_name,omitempty"`
	// StorageTarget: "default" | "local" | "s3_xxxx"
	StorageTarget string `json:"storage_target,omitempty"`
	// Retention is how many archives to keep per job on the host.
	Retention int      `json:"retention"`
	Schedule  Schedule `json:"schedule"`
	// Enabled toggles cron pickup; manual runs ignore it.
	Enabled   bool      `json:"enabled"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Archive is one completed backup on a managed host or S3 bucket.
type Archive struct {
	ID            string    `json:"id"` // job id + timestamp, also the file stem
	JobID         string    `json:"job_id"`
	HostName      string    `json:"host_id"`
	Kind          JobKind   `json:"kind"`
	Target        string    `json:"target"`
	StorageTarget string    `json:"storage_target,omitempty"` // "local" | "s3_xxxx"
	StoragePath   string    `json:"storage_path,omitempty"`   // S3 key or host path
	Size          int64     `json:"size"`                     // bytes
	SHA256        string    `json:"sha256,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Store persists backup jobs, archive metadata and S3 storage targets.
type Store struct {
	mu        sync.RWMutex
	dir       string
	log       *slog.Logger
	jobs      map[string]*Job
	archives  map[string][]*Archive // job_id -> archives, newest first
	s3Targets map[string]*S3Target
}

const (
	jobsFile          = "snapshots.json"
	archivesFile      = "snapshot_archives.jsonl"
	s3TargetsFile     = "s3_targets.json"
	maxArchiveHistory = 500 // per job, metadata records only
)

// NewStore loads or initializes the snapshot store under dataDir.
func NewStore(dataDir string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		dir:       dataDir,
		log:       log,
		jobs:      make(map[string]*Job),
		archives:  make(map[string][]*Archive),
		s3Targets: make(map[string]*S3Target),
	}
	if err := s.loadJobs(); err != nil {
		return nil, err
	}
	if err := s.loadS3Targets(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadJobs() error {
	b, err := os.ReadFile(filepath.Join(s.dir, jobsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", jobsFile, err)
	}
	var list []*Job
	if err := json.Unmarshal(b, &list); err != nil {
		s.log.Warn("snapshots.json corrupted, starting empty", "err", err)
		return nil
	}
	for _, j := range list {
		s.jobs[j.ID] = j
	}
	return nil
}

func (s *Store) saveJobsLocked() error {
	list := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, fmt.Sprintf("%s.tmp.%d", jobsFile, time.Now().UnixNano()))
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, jobsFile))
}

// ListJobs returns all jobs ordered by creation time.
func (s *Store) ListJobs() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return list
}

// GetJob returns a copy of one job.
func (s *Store) GetJob(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// PutJob inserts or replaces one job.
func (s *Store) PutJob(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[j.ID] = j
	return s.saveJobsLocked()
}

// UpdateJob mutates one job under the write lock and persists.
func (s *Store) UpdateJob(id string, mutate func(*Job) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	if err := mutate(j); err != nil {
		return err
	}
	return s.saveJobsLocked()
}

// DeleteJob removes one job. Its archives on the host stay untouched until
// pruned by retention or removed via the archives API.
func (s *Store) DeleteJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("job %s not found", id)
	}
	delete(s.jobs, id)
	return s.saveJobsLocked()
}

// AddArchive records a completed archive (JSONL append).
func (s *Store) AddArchive(a *Archive) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, archivesFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	list := append(s.archives[a.JobID], a)
	if len(list) > maxArchiveHistory {
		list = list[:maxArchiveHistory]
	}
	s.archives[a.JobID] = list
	return nil
}

// ListArchives returns archives for one job, newest first, paged.
func (s *Store) ListArchives(jobID string, offset, pageSize int) ([]*Archive, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.archives[jobID]
	total := len(list)
	if offset >= total {
		return nil, total
	}
	end := total
	if pageSize > 0 && offset+pageSize < total {
		end = offset + pageSize
	}
	out := make([]*Archive, 0, end-offset)
	for _, a := range list[offset:end] {
		cp := *a
		out = append(out, &cp)
	}
	return out, total
}

// DeleteArchive removes one archive's metadata record.
func (s *Store) DeleteArchive(jobID, archiveID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.archives[jobID]
	for i, a := range list {
		if a.ID == archiveID {
			s.archives[jobID] = append(list[:i], list[i+1:]...)
			return s.rewriteArchivesLocked()
		}
	}
	return fmt.Errorf("archive %s not found", archiveID)
}

// rewriteArchivesLocked rewrites the JSONL after a deletion.
func (s *Store) rewriteArchivesLocked() error {
	var buf []byte
	ids := make([]string, 0, len(s.archives))
	for id := range s.archives {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, a := range s.archives[id] {
			b, err := json.Marshal(a)
			if err != nil {
				return err
			}
			buf = append(buf, b...)
			buf = append(buf, '\n')
		}
	}
	tmp := filepath.Join(s.dir, fmt.Sprintf("%s.tmp.%d", archivesFile, time.Now().UnixNano()))
	if err := os.WriteFile(tmp, buf, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, archivesFile))
}

// ---- S3 Storage Targets ----

func (s *Store) s3TargetsPath() string {
	return filepath.Join(s.dir, s3TargetsFile)
}

func (s *Store) loadS3Targets() error {
	b, err := os.ReadFile(s.s3TargetsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", s3TargetsFile, err)
	}
	var list []*S3Target
	if err := json.Unmarshal(b, &list); err != nil {
		s.log.Warn("s3_targets.json corrupted, starting empty", "err", err)
		return nil
	}
	for _, t := range list {
		s.s3Targets[t.ID] = t
	}
	return nil
}

func (s *Store) saveS3TargetsLocked() error {
	list := make([]*S3Target, 0, len(s.s3Targets))
	for _, t := range s.s3Targets {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, fmt.Sprintf("%s.tmp.%d", s3TargetsFile, time.Now().UnixNano()))
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.s3TargetsPath())
}

// ListS3Targets returns all S3 targets with SecretKey masked.
func (s *Store) ListS3Targets() []*S3Target {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*S3Target, 0, len(s.s3Targets))
	for _, t := range s.s3Targets {
		list = append(list, t.Redacted())
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	return list
}

// GetS3Target returns one S3 target with full credentials for server-side S3 operations.
func (s *Store) GetS3Target(id string) (*S3Target, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.s3Targets[id]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// GetDefaultS3Target returns the target marked as default.
func (s *Store) GetDefaultS3Target() (*S3Target, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.s3Targets {
		if t.IsDefault {
			cp := *t
			return &cp, true
		}
	}
	return nil, false
}

// PutS3Target creates or updates an S3 storage target.
func (s *Store) PutS3Target(t *S3Target) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		t.ID = "s3_" + randomHex(8)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	// If marked default, clear default from others
	if t.IsDefault {
		for _, existing := range s.s3Targets {
			existing.IsDefault = false
		}
	}
	s.s3Targets[t.ID] = t
	return s.saveS3TargetsLocked()
}

// DeleteS3Target removes an S3 storage target.
func (s *Store) DeleteS3Target(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.s3Targets[id]; !ok {
		return fmt.Errorf("s3 target %s not found", id)
	}
	delete(s.s3Targets, id)
	return s.saveS3TargetsLocked()
}

func randomHex(n int) string {
	return fmt.Sprintf("%x", time.Now().UnixNano())[:n]
}
