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
	// KindAppVolume archives one declared volume of an application managed by
	// the app center. The app engine owns the semantics (declared-volume
	// allowlist, S3-only archives, SHA-verified restore with container
	// stop/start); this module owns the job, schedule and unified history.
	KindAppVolume JobKind = "app_volume"
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
	// AppID binds kind=app_volume jobs to an app-center application. HostID is
	// derived from the app at creation time.
	AppID string `json:"app_id,omitempty"`
	// Target: dir path, volume name, container name (database), or the
	// application's volume spec "src:dst" (app_volume).
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
	ID            string  `json:"id"` // job id + timestamp, also the file stem
	JobID         string  `json:"job_id"`
	HostName      string  `json:"host_id"`
	Kind          JobKind `json:"kind"`
	Target        string  `json:"target"`
	StorageTarget string  `json:"storage_target,omitempty"` // "local" | "s3_xxxx"
	// StorageTargetName/Status/Error/Note/LastRestore* are populated for
	// app_volume archives only: those have a lifecycle (running -> success /
	// failed) and a frozen S3 target name, while the other kinds only ever
	// record a successful final state.
	StorageTargetName string     `json:"storage_target_name,omitempty"`
	StoragePath       string     `json:"storage_path,omitempty"` // S3 key or host path
	Size              int64      `json:"size"`                   // bytes
	SHA256            string     `json:"sha256,omitempty"`
	Status            string     `json:"status,omitempty"`
	Error             string     `json:"error,omitempty"`
	Note              string     `json:"note,omitempty"`
	LastRestoreAt     *time.Time `json:"last_restore_at,omitempty"`
	LastRestoreStatus string     `json:"last_restore_status,omitempty"`
	LastRestoreError  string     `json:"last_restore_error,omitempty"`
	// Source is populated for app_volume archives only: "upload" marks a
	// record created by an upload-restore (no S3 object, panel temp file).
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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

func cloneJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	cp := *j
	return &cp
}

func cloneJobs(jobs map[string]*Job) map[string]*Job {
	out := make(map[string]*Job, len(jobs))
	for id, j := range jobs {
		out[id] = cloneJob(j)
	}
	return out
}

func (s *Store) saveJobsSnapshotLocked(jobs map[string]*Job) error {
	list := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
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

func (s *Store) saveJobsLocked() error {
	return s.saveJobsSnapshotLocked(s.jobs)
}

// ListJobs returns all jobs ordered by creation time.
// Each returned job is a copy so callers cannot mutate the live store without
// going through an explicit update method.
func (s *Store) ListJobs() []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, cloneJob(j))
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
	return cloneJob(j), true
}

// PutJob inserts or replaces one job. The candidate snapshot is persisted
// before becoming the live map, so a failed write cannot split memory from disk.
func (s *Store) PutJob(j *Job) error {
	if j == nil {
		return fmt.Errorf("job is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneJobs(s.jobs)
	candidate[j.ID] = cloneJob(j)
	if err := s.saveJobsSnapshotLocked(candidate); err != nil {
		return err
	}
	s.jobs = candidate
	return nil
}

// UpdateJob mutates a candidate job under the write lock and commits it only
// after the candidate snapshot has been persisted.
func (s *Store) UpdateJob(id string, mutate func(*Job) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("job %s not found", id)
	}
	candidate := cloneJobs(s.jobs)
	if err := mutate(candidate[id]); err != nil {
		return err
	}
	if err := s.saveJobsSnapshotLocked(candidate); err != nil {
		return err
	}
	s.jobs = candidate
	return nil
}

// DeleteJob removes one job. Its archives on the host stay untouched until
// pruned by retention or removed via the archives API.
func (s *Store) DeleteJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return fmt.Errorf("job %s not found", id)
	}
	candidate := cloneJobs(s.jobs)
	delete(candidate, id)
	if err := s.saveJobsSnapshotLocked(candidate); err != nil {
		return err
	}
	s.jobs = candidate
	return nil
}

// DeleteJobsByApp removes the app_volume jobs bound to one application and
// returns both the number removed and any persistence error. A failed save
// leaves the live map untouched.
func (s *Store) DeleteJobsByApp(appID string) (int, error) {
	if appID == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneJobs(s.jobs)
	n := 0
	for id, j := range candidate {
		if j.Kind == KindAppVolume && j.AppID == appID {
			delete(candidate, id)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.saveJobsSnapshotLocked(candidate); err != nil {
		return 0, err
	}
	s.jobs = candidate
	return n, nil
}

// UpdateAppVolumeJobHost updates the derived host binding for every app_volume
// job of an application. It uses the same candidate-snapshot transaction as
// the other job mutations.
func (s *Store) UpdateAppVolumeJobHost(appID, hostID string) error {
	if appID == "" || hostID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := cloneJobs(s.jobs)
	changed := false
	for _, j := range candidate {
		if j.Kind == KindAppVolume && j.AppID == appID && j.HostID != hostID {
			j.HostID = hostID
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := s.saveJobsSnapshotLocked(candidate); err != nil {
		return err
	}
	s.jobs = candidate
	return nil
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
