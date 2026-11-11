package backup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Task struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"` // running | ready | success | failed | cancelled
	Phase      string         `json:"phase"`
	Files      int            `json:"files"`
	Bytes      int64          `json:"bytes"`
	Error      string         `json:"error,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at,omitempty"`
	Archive    *Meta          `json:"archive,omitempty"`
	Result     *RestoreResult `json:"result,omitempty"`
}

func (s *Store) taskPath() string      { return filepath.Join(s.backupDir, "task.json") }
func (s *Store) saveTaskLocked() error { return atomicJSON(s.taskPath(), s.task) }

func (s *Store) loadOperations() error {
	s.policy = Policy{Time: "03:00", Retention: 7}
	for _, file := range []struct {
		name   string
		target any
	}{{s.policyPath(), &s.policy}, {s.taskPath(), &s.task}} {
		raw, err := os.ReadFile(file.name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, file.target); err != nil {
			return fmt.Errorf("备份状态文件 %s 无效: %w", file.name, err)
		}
	}
	if s.task.Status == "running" && !s.hasPending() {
		s.task.Status = "failed"
		s.task.Error = "服务重启，任务已中断"
		s.task.FinishedAt = time.Now()
		return s.saveTaskLocked()
	}
	return nil
}

func (s *Store) Status() Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.task
	if t.Result != nil {
		r := *t.Result
		t.Result = &r
	}
	if t.Archive != nil {
		a := *t.Archive
		t.Archive = &a
	}
	return t
}

func (s *Store) progress(phase string, files int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task.Status != "running" && phase != "applying" {
		return
	}
	s.task.Phase = phase
	s.task.Files = files
	s.task.Bytes = bytes
}

func (s *Store) startTask(kind string) (Task, error) {
	if s.dataDir == "" {
		return Task{}, ErrDisabled
	}
	if !s.opMu.TryLock() {
		return Task{}, ErrBusy
	}
	if s.hasPending() {
		s.opMu.Unlock()
		return Task{}, ErrPending
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task = Task{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Kind: kind, Status: "running", Phase: "queued", StartedAt: time.Now()}
	if err := s.saveTaskLocked(); err != nil {
		s.opMu.Unlock()
		return Task{}, err
	}
	return s.task, nil
}

func (s *Store) endTask(meta *Meta, result *RestoreResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.task.Archive = meta
	s.task.Result = result
	s.task.FinishedAt = time.Now()
	s.task.Phase = "complete"
	s.task.Status = "success"
	if result != nil && result.Pending {
		s.task.Status = "ready"
		s.task.Phase = "awaiting_restart"
	}
	if err != nil {
		s.task.Status = "failed"
		s.task.Error = err.Error()
	}
	if saveErr := s.saveTaskLocked(); saveErr != nil {
		s.log.Error("save backup task", "err", saveErr)
	}
}

func (s *Store) StartBackup(actor, note, target string, scheduled bool) (Task, error) {
	if _, err := s.resolveTarget(target); err != nil {
		return Task{}, err
	}
	task, err := s.startTask("backup")
	if err != nil {
		return Task{}, err
	}
	go func() {
		defer s.opMu.Unlock()
		kind := "manual"
		if scheduled {
			kind = "scheduled"
		}
		m, err := s.create(actor, note, kind)
		if err == nil {
			m, err = s.copyRemote(m, target)
			if saveErr := s.updateMeta(m); err == nil {
				err = saveErr
			}
			if err == nil && scheduled {
				err = s.pruneScheduled(s.Policy().Retention)
			}
		}
		if scheduled {
			s.recordSchedule(err)
		}
		var meta *Meta
		if m.Name != "" {
			meta = &m
		}
		s.endTask(meta, nil, err)
	}()
	return task, nil
}

func (s *Store) StartRestoreFile(name, actor, passphrase string) (Task, error) {
	if _, err := s.Path(name); err != nil {
		return Task{}, err
	}
	task, err := s.startTask("restore")
	if err != nil {
		return Task{}, err
	}
	go func() {
		defer s.opMu.Unlock()
		res, err := s.prepareFile(name, actor, passphrase)
		s.endTask(nil, &res, err)
	}()
	return task, nil
}

// StartRestoreUpload consumes the request before returning. Parsing/decompression
// continues in the background from a bounded private file, never Request.Body.
func (s *Store) StartRestoreUpload(r io.Reader, actor, passphrase string) (Task, error) {
	task, err := s.startTask("restore")
	if err != nil {
		return Task{}, err
	}
	f, err := os.CreateTemp(s.backupDir, ".upload-*")
	if err != nil {
		s.endTask(nil, nil, err)
		s.opMu.Unlock()
		return Task{}, err
	}
	name := f.Name()
	n, err := io.Copy(f, io.LimitReader(r, MaxArchiveBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if n > MaxArchiveBytes {
		err = fmt.Errorf("上传归档超过 65 GiB 上限")
	}
	if err != nil {
		os.Remove(name)
		s.endTask(nil, nil, err)
		s.opMu.Unlock()
		return Task{}, err
	}
	go func() {
		defer s.opMu.Unlock()
		defer os.Remove(name)
		f, err := os.Open(name)
		if err != nil {
			s.endTask(nil, nil, err)
			return
		}
		defer f.Close()
		res, err := s.prepare(f, actor, "upload", "", passphrase)
		s.endTask(nil, &res, err)
	}()
	return task, nil
}
