package cert

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type IssueTaskStatus string

const (
	TaskRunning IssueTaskStatus = "running"
	TaskSuccess IssueTaskStatus = "success"
	TaskFailed  IssueTaskStatus = "failed"
)

// IssueTask tracks an ongoing or completed certificate issuance/renewal request.
type IssueTask struct {
	ID        string          `json:"id"`
	CertID    string          `json:"cert_id,omitempty"`
	Domains   []string        `json:"domains"`
	AccountID string          `json:"account_id,omitempty"`
	Challenge string          `json:"challenge,omitempty"`
	Status    IssueTaskStatus `json:"status"` // "running" | "success" | "failed"
	Error     string          `json:"error,omitempty"`
	Log       string          `json:"log,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

const maxRetainedTasks = 30

func (h *Hub) tasksPath() string            { return filepath.Join(h.dir, "tasks.json") }
func (h *Hub) logsDir() string              { return filepath.Join(h.dir, "logs") }
func (h *Hub) certLogFile(id string) string { return filepath.Join(h.logsDir(), id+".log") }

func cloneIssueTask(t *IssueTask) *IssueTask {
	if t == nil {
		return nil
	}
	cp := *t
	cp.Domains = append([]string(nil), t.Domains...)
	return &cp
}

func cloneIssueTasks(src map[string]*IssueTask) map[string]*IssueTask {
	dst := make(map[string]*IssueTask, len(src))
	for id, task := range src {
		dst[id] = cloneIssueTask(task)
	}
	return dst
}

func taskBefore(a, b *IssueTask) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

// retainTasks returns a cloned, pruned snapshot. Every running task is kept;
// only the newest maxRetainedTasks terminal tasks are eligible for pruning.
func retainTasks(src map[string]*IssueTask) map[string]*IssueTask {
	candidate := cloneIssueTasks(src)
	terminal := make([]*IssueTask, 0, len(candidate))
	for _, task := range candidate {
		if task.Status != TaskRunning {
			terminal = append(terminal, task)
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return taskBefore(terminal[i], terminal[j]) })
	for _, task := range terminal[maxInt(0, minInt(len(terminal), maxRetainedTasks)):] {
		delete(candidate, task.ID)
	}
	return candidate
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (h *Hub) loadTasks() error {
	b, err := os.ReadFile(h.tasksPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cert tasks: %w", err)
	}
	var list []*IssueTask
	if err := unmarshalJSON(b, &list); err != nil {
		h.log.Warn("cert tasks corrupted, starting empty", "err", err)
		return nil
	}
	needSave := false
	for _, task := range list {
		if task == nil || task.ID == "" {
			continue
		}
		// A restart must not leave tasks stuck in "running" forever.
		if task.Status == TaskRunning {
			task.Status = TaskFailed
			task.Error = "服务重启，任务已中断"
			task.UpdatedAt = time.Now()
			needSave = true
		}
		h.tasks[task.ID] = cloneIssueTask(task)
	}
	if needSave {
		if err := h.saveTasksLocked(); err != nil {
			h.log.Warn("persist interrupted cert tasks failed", "err", err)
		}
	}
	return nil
}

// persistTasksLocked atomically writes a retained candidate snapshot. The
// caller owns h.mu; h.tasks is changed only after the rename succeeds.
func (h *Hub) persistTasksLocked(src map[string]*IssueTask) (map[string]*IssueTask, error) {
	candidate := retainTasks(src)
	list := make([]*IssueTask, 0, len(candidate))
	for _, task := range candidate {
		list = append(list, task)
	}
	sort.Slice(list, func(i, j int) bool { return taskBefore(list[i], list[j]) })
	b, err := marshalJSON(list)
	if err != nil {
		return nil, err
	}
	tmp := h.tasksPath() + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	written := false
	defer func() {
		if !written {
			_ = os.Remove(tmp)
		}
	}()
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, h.tasksPath()); err != nil {
		return nil, err
	}
	written = true
	return candidate, nil
}

func (h *Hub) saveTasksLocked() error {
	candidate, err := h.persistTasksLocked(h.tasks)
	if err != nil {
		return err
	}
	h.tasks = candidate
	return nil
}

// CreateIssueTaskWithError enqueues a task only after its durable snapshot has
// been written successfully. On failure, neither memory nor disk is changed.
func (h *Hub) CreateIssueTaskWithError(domains []string, accountID, challenge string) (*IssueTask, error) {
	id := "task_" + randomHex(8)
	now := time.Now()
	task := &IssueTask{
		ID:        id,
		Domains:   append([]string(nil), domains...),
		AccountID: accountID,
		Challenge: challenge,
		Status:    TaskRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	candidate := cloneIssueTasks(h.tasks)
	candidate[id] = task
	committed, err := h.persistTasksLocked(candidate)
	if err != nil {
		return nil, err
	}
	h.tasks = committed
	return cloneIssueTask(task), nil
}

// CreateIssueTask is retained for internal/test callers that historically did
// not handle persistence errors. Production request paths use the error form.
func (h *Hub) CreateIssueTask(domains []string, accountID, challenge string) *IssueTask {
	task, err := h.CreateIssueTaskWithError(domains, accountID, challenge)
	if err != nil {
		h.log.Error("create cert issue task failed", "err", err)
		return nil
	}
	return task
}

// UpdateIssueTaskWithError modifies a task in a candidate snapshot and commits
// it only after durable persistence succeeds. A missing task is a no-op.
func (h *Hub) UpdateIssueTaskWithError(id string, updateFn func(*IssueTask)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.tasks[id]; !ok {
		return nil
	}
	candidate := cloneIssueTasks(h.tasks)
	updated := candidate[id]
	updateFn(updated)
	updated.UpdatedAt = time.Now()
	committed, err := h.persistTasksLocked(candidate)
	if err != nil {
		return err
	}
	h.tasks = committed
	return nil
}

// UpdateIssueTask is retained for callers that historically ignored errors.
func (h *Hub) UpdateIssueTask(id string, updateFn func(*IssueTask)) {
	if err := h.UpdateIssueTaskWithError(id, updateFn); err != nil {
		h.log.Error("update cert issue task failed", "id", id, "err", err)
	}
}

// GetTask returns a copy of the issue task. If currently running, overlays live log.
func (h *Hub) GetTask(id string) (*IssueTask, bool) {
	h.mu.RLock()
	t, ok := h.tasks[id]
	if !ok {
		h.mu.RUnlock()
		return nil, false
	}
	cp := cloneIssueTask(t)
	h.mu.RUnlock()

	if cp.Status == TaskRunning {
		if live, ok := h.LiveLog(id); ok {
			cp.Log = live
		}
	}
	return cp, true
}

// ListTasks returns recent issue tasks without full log strings to keep payload small.
func (h *Hub) ListTasks() []*IssueTask {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make([]*IssueTask, 0, len(h.tasks))
	for _, task := range h.tasks {
		cp := cloneIssueTask(task)
		cp.Log = "" // omit large log body for list
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return taskBefore(out[i], out[j]) })
	return out
}

// RegisterLiveLog tracks an in-flight logger under an identifier (task_*, crt_*, mord_*).
func (h *Hub) RegisterLiveLog(id string, l *certLogger) {
	h.liveLogs.Store(id, l)
}

// UnregisterLiveLog removes an in-flight logger.
func (h *Hub) UnregisterLiveLog(id string) {
	h.liveLogs.Delete(id)
}

// LiveLog retrieves the in-flight log content for an active execution.
func (h *Hub) LiveLog(id string) (string, bool) {
	val, ok := h.liveLogs.Load(id)
	if !ok {
		return "", false
	}
	l, ok := val.(*certLogger)
	if !ok || l == nil {
		return "", false
	}
	return l.String(), true
}

// SaveCertLog persists the latest full execution log of a certificate.
func (h *Hub) SaveCertLog(certID, content string) error {
	if err := os.MkdirAll(h.logsDir(), 0700); err != nil {
		return err
	}
	filePath := h.certLogFile(certID)
	tmp := filePath + fmt.Sprintf(".tmp.%d", time.Now().UnixNano())
	written := false
	defer func() {
		if !written {
			_ = os.Remove(tmp)
		}
	}()
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filePath); err != nil {
		return err
	}
	written = true

	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.certs[certID]; ok {
		c.LastLogAt = time.Now()
		if err := h.saveCertsLocked(); err != nil {
			return err
		}
	}
	return nil
}

// GetCertLog returns the log text, updated time, and whether it exists.
// Live in-flight logs take precedence over on-disk logs.
func (h *Hub) GetCertLog(certID string) (string, time.Time, bool) {
	if live, ok := h.LiveLog(certID); ok {
		return live, time.Now(), true
	}
	path := h.certLogFile(certID)
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, false
	}
	return string(b), info.ModTime(), true
}

// DeleteCertLog removes the on-disk log file for a certificate or order.
func (h *Hub) DeleteCertLog(certID string) {
	_ = os.Remove(h.certLogFile(certID))
}
