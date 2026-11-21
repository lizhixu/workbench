package network

import (
	"sync"
	"time"
)

// Async install/join tasks.
//
// Installing or joining the mesh runs a remote exec that can take minutes
// (install: up to 300s server-side). Holding one HTTP request open for that
// long is fragile: any proxy on the path (Cloudflare cuts proxied requests
// at 100s) or browser timeout kills the request while the server keeps
// running, so the UI reports a timeout for a task that later succeeds.
// install/join therefore run asynchronously: POST validates and returns
// 202 + task_id immediately, the exec runs in a goroutine, and the frontend
// polls GET /network/nodes/:id/tasks/:taskId until the task is terminal.
//
// Task records live in memory only. The durable outcome (installed /
// online / mesh IP) is written to NodeStatus, which is persisted, so a
// lost task record (server restart, TTL expiry) is recoverable by probing
// the node again with POST .../check.

type TaskStatus string

const (
	// TaskRunning means the remote exec is still in flight.
	TaskRunning TaskStatus = "running"
	// TaskSuccess means the exec finished with exit code 0.
	TaskSuccess TaskStatus = "success"
	// TaskFailed means the exec errored or exited non-zero.
	TaskFailed TaskStatus = "failed"
)

// TaskKind identifies what a task does.
type TaskKind string

const (
	TaskKindInstall TaskKind = "install"
	TaskKindJoin    TaskKind = "join"
)

// NetworkTask is one async install/join operation. It is the polling
// payload of GET /network/nodes/:id/tasks/:taskId.
type NetworkTask struct {
	ID         string     `json:"task_id"`
	HostID     string     `json:"host_id"`
	Kind       TaskKind   `json:"kind"`
	Status     TaskStatus `json:"status"`
	Message    string     `json:"message"`
	Detail     string     `json:"detail,omitempty"`
	IP         string     `json:"ip,omitempty"`
	PreviousIP string     `json:"previous_ip,omitempty"`
	IPChanged  bool       `json:"ip_changed,omitempty"`
	DurationMs int64      `json:"duration_ms,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// taskResultTTL bounds how long a finished task stays pollable. The
// frontend polls every 2s and stops at the terminal state, so 10 minutes
// is ample headroom for a slow client; after that the task reads as 404
// and the caller falls back to probing the node.
const taskResultTTL = 10 * time.Minute

// maxTasks bounds the registry. Finished tasks are evicted oldest-first
// once the cap is hit; running tasks are never evicted.
const maxTasks = 200

// clipDetail caps stored exec output so one verbose installer cannot
// bloat the registry.
const clipDetailLen = 2000

// TaskStore is the in-memory registry of async network tasks. It also
// enforces the single-flight guard: at most one install/join runs per
// host at a time (install and join share the guard, because the installer
// restarts tailscaled while a join would be racing it).
type TaskStore struct {
	mu     sync.Mutex
	tasks  map[string]*NetworkTask
	active map[string]string // host_id -> task_id of the running op
}

// NewTaskStore creates an empty task registry.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks:  make(map[string]*NetworkTask),
		active: make(map[string]string),
	}
}

// Start creates a task for the host, or returns the already-running task
// with ok=false when the host already has an in-flight op. The caller
// answers that case with 409 + the running task_id so the client can poll
// the existing task instead of starting a second installer.
func (s *TaskStore) Start(hostID string, kind TaskKind) (*NetworkTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.active[hostID]; ok {
		if t, exists := s.tasks[id]; exists {
			return t, false
		}
		// Stale guard entry (task record already evicted): drop it.
		delete(s.active, hostID)
	}
	s.evictLocked()
	task := &NetworkTask{
		ID:        "ntk-" + randomToken(8),
		HostID:    hostID,
		Kind:      kind,
		Status:    TaskRunning,
		Message:   "任务执行中",
		CreatedAt: time.Now(),
	}
	s.tasks[task.ID] = task
	s.active[hostID] = task.ID
	return task, true
}

// Get returns a copy of the task. Finished tasks older than taskResultTTL
// are dropped and read as not found.
func (s *TaskStore) Get(taskID string) (*NetworkTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return nil, false
	}
	if t.Status != TaskRunning && t.FinishedAt != nil && time.Since(*t.FinishedAt) > taskResultTTL {
		delete(s.tasks, taskID)
		return nil, false
	}
	cp := *t
	return &cp, true
}

// SetJoinResult records the mesh IP outcome of a join. Call it before
// Finish; it holds the store lock, so a polling Get never races the
// runner mutating the task.
func (s *TaskStore) SetJoinResult(task *NetworkTask, ip, prevIP string, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task == nil {
		return
	}
	task.IP = ip
	task.PreviousIP = prevIP
	task.IPChanged = changed
}

// Finish marks the task terminal, records its outcome, and releases the
// host's single-flight guard. Detail is clipped to bound memory.
func (s *TaskStore) Finish(task *NetworkTask, status TaskStatus, message, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if task == nil {
		return
	}
	now := time.Now()
	task.Status = status
	task.Message = message
	task.Detail = clipDetail(detail)
	task.DurationMs = now.Sub(task.CreatedAt).Milliseconds()
	task.FinishedAt = &now
	if id, ok := s.active[task.HostID]; ok && id == task.ID {
		delete(s.active, task.HostID)
	}
}

// evictLocked drops the oldest finished tasks when the registry is over
// the cap. Callers must hold s.mu.
func (s *TaskStore) evictLocked() {
	if len(s.tasks) < maxTasks {
		return
	}
	var oldest *NetworkTask
	for _, t := range s.tasks {
		if t.Status == TaskRunning || t.FinishedAt == nil {
			continue
		}
		if oldest == nil || t.FinishedAt.Before(*oldest.FinishedAt) {
			oldest = t
		}
	}
	if oldest != nil {
		delete(s.tasks, oldest.ID)
	}
}

func clipDetail(s string) string {
	if len(s) <= clipDetailLen {
		return s
	}
	return s[:clipDetailLen] + "…（已截断）"
}
