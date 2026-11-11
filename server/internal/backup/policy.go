package backup

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watchman/server/internal/snapshots"
)

type S3Targets interface {
	GetS3Target(string) (*snapshots.S3Target, bool)
	GetDefaultS3Target() (*snapshots.S3Target, bool)
}

type Policy struct {
	Enabled   bool      `json:"enabled"`
	Time      string    `json:"time"`
	Retention int       `json:"retention"`
	TargetID  string    `json:"target_id"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

func (s *Store) policyPath() string { return filepath.Join(s.dataDir, "control-backup-policy.json") }

func (s *Store) SetS3Targets(targets S3Targets) { s.s3 = targets }
func (s *Store) Policy() Policy                 { s.mu.Lock(); defer s.mu.Unlock(); return s.policy }
func (s *Store) SavePolicy(p Policy) error {
	if _, err := time.Parse("15:04", p.Time); err != nil {
		return fmt.Errorf("每日时间须为 HH:mm")
	}
	if p.Retention < 1 || p.Retention > 100 {
		return fmt.Errorf("自动备份保留份数须为 1~100")
	}
	p.TargetID = strings.TrimSpace(p.TargetID)
	if _, err := s.resolveTarget(p.TargetID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p.LastRun = s.policy.LastRun
	p.LastError = s.policy.LastError
	if err := atomicJSON(s.policyPath(), p); err != nil {
		return err
	}
	s.policy = p
	return nil
}

func (s *Store) recordSchedule(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy.LastRun = time.Now()
	s.policy.LastError = ""
	if cause != nil {
		s.policy.LastError = cause.Error()
	}
	if err := atomicJSON(s.policyPath(), s.policy); err != nil {
		s.log.Error("save backup schedule result", "err", err)
	}
}

func (s *Store) StartScheduler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.runDue(now.In(time.Local))
			}
		}
	}()
}

func (s *Store) runDue(now time.Time) {
	p := s.Policy()
	if !p.Enabled || now.Format("15:04") != p.Time || (!p.LastRun.IsZero() && now.Sub(p.LastRun) < time.Minute) {
		return
	}
	// Reserve the schedule minute before dispatch, including failed attempts.
	s.recordSchedule(nil)
	if _, err := s.StartBackup("system", "定时控制端备份", p.TargetID, true); err != nil {
		s.recordSchedule(err)
	}
}

func (s *Store) resolveTarget(id string) (*snapshots.S3Target, error) {
	if id == "" || id == "local" {
		return nil, nil
	}
	if s.s3 == nil {
		return nil, fmt.Errorf("S3 存储未启用")
	}
	var target *snapshots.S3Target
	var ok bool
	if id == "default" {
		target, ok = s.s3.GetDefaultS3Target()
	} else {
		target, ok = s.s3.GetS3Target(id)
	}
	if !ok || target == nil {
		return nil, fmt.Errorf("所选 S3 存储目标不存在，请重新选择")
	}
	return target, nil
}

// uploadProgress streams the archive while refreshing the task's uploaded
// byte count about once per second, so a long S3 copy does not sit at a fixed
// progress value for the whole upload.
type uploadProgress struct {
	store *Store
	r     io.Reader
	files int
	read  int64
	last  time.Time
}

func (u *uploadProgress) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	u.read += int64(n)
	if time.Since(u.last) >= time.Second {
		u.tick()
	}
	return n, err
}

func (u *uploadProgress) tick() {
	u.last = time.Now()
	u.store.progress("uploading_s3", u.files, u.read)
}

func (s *Store) copyRemote(m Meta, id string) (Meta, error) {
	target, err := s.resolveTarget(id)
	if err != nil || target == nil {
		return m, err
	}
	client := snapshots.NewS3Client(target)
	key := client.ObjectKey("control-plane/" + m.Name)
	u, err := client.PresignPut(key, 2*time.Hour)
	if err != nil {
		return m, err
	}
	m.RemoteTarget = target.ID
	m.RemoteName = target.Name
	m.RemoteKey = key
	f, err := os.Open(filepath.Join(s.backupDir, m.Name))
	if err != nil {
		return m, err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	up := &uploadProgress{store: s, r: f, files: m.Files}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, up)
	if err != nil {
		return m, err
	}
	req.ContentLength = m.Size
	up.tick()
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err == nil {
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode/100 != 2 {
			err = fmt.Errorf("S3 上传返回 HTTP %d", resp.StatusCode)
		}
	}
	if err != nil {
		m.RemoteError = "S3 副本上传失败，本地归档已保留"
		return m, fmt.Errorf("%s", m.RemoteError)
	}
	return m, nil
}

func (s *Store) updateMeta(m Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.index {
		if s.index[i].Name == m.Name {
			s.index[i] = m
			return s.save()
		}
	}
	return fmt.Errorf("归档索引不存在")
}

// deleteRemote is best-effort for permanent conditions: when the recorded S3
// target no longer exists, retrying can never succeed, so deletion proceeds
// locally and the caller surfaces the returned warning. A failed DeleteObject
// may still be transient, so it stays blocking and keeps the record for retry.
func (s *Store) deleteRemote(name string) (string, error) {
	for _, m := range s.List() {
		if m.Name != name || m.RemoteTarget == "" || m.RemoteKey == "" {
			continue
		}
		target, err := s.resolveTarget(m.RemoteTarget)
		if err != nil {
			s.log.Warn("S3 target of remote copy is gone; skipping remote cleanup", "name", name, "target", m.RemoteTarget)
			return "S3 目标已被删除，远程副本无法清理，可在对象存储中手动删除", nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := snapshots.NewS3Client(target).DeleteObject(ctx, m.RemoteKey); err != nil {
			return "", fmt.Errorf("S3 副本删除失败，已保留归档记录")
		}
	}
	return "", nil
}

func (s *Store) pruneScheduled(keep int) error {
	count := 0
	for _, m := range s.List() {
		if m.Kind != "scheduled" {
			continue
		}
		count++
		if count > keep {
			if _, err := s.delete(m.Name); err != nil {
				return err
			}
		}
	}
	return nil
}
