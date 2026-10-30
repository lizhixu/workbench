package cert

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIssueTasksLifecycleAndCompaction(t *testing.T) {
	dataDir := t.TempDir()
	hub, err := NewHub(dataDir, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	// 1. Create task
	task := hub.CreateIssueTask([]string{"example.com"}, "acc_1", "http-01")
	if task == nil || task.ID == "" {
		t.Fatalf("expected non-nil task with ID")
	}
	if task.Status != TaskRunning {
		t.Fatalf("expected status %s, got %s", TaskRunning, task.Status)
	}

	// Verify it can be retrieved
	got, ok := hub.GetTask(task.ID)
	if !ok || got.ID != task.ID {
		t.Fatalf("expected task %s, got %v", task.ID, got)
	}

	// 2. Update task
	hub.UpdateIssueTask(task.ID, func(tk *IssueTask) {
		tk.Status = TaskSuccess
		tk.CertID = "crt_test123"
		tk.Log = "[2026-10-30 12:00:00] [完成入库] 证书入库成功"
	})
	oldRunning, err := hub.CreateIssueTaskWithError([]string{"old-running.example.com"}, "acc_1", "http-01")
	if err != nil {
		t.Fatalf("create old running task: %v", err)
	}

	gotUpdated, ok := hub.GetTask(task.ID)
	if !ok || gotUpdated.Status != TaskSuccess || gotUpdated.CertID != "crt_test123" {
		t.Fatalf("update failed: got %v", gotUpdated)
	}

	// 3. Test cap & compaction (> maxRetainedTasks = 30 terminal tasks).
	for i := 0; i < 35; i++ {
		terminal := hub.CreateIssueTask([]string{fmt.Sprintf("sub%d.example.com", i)}, "acc_1", "http-01")
		if terminal == nil {
			t.Fatalf("create terminal task %d returned nil", i)
		}
		if err := hub.UpdateIssueTaskWithError(terminal.ID, func(tk *IssueTask) {
			tk.Status = TaskSuccess
		}); err != nil {
			t.Fatalf("finish task %d: %v", i, err)
		}
	}

	tasks := hub.ListTasks()
	terminalCount := 0
	for _, item := range tasks {
		if item.Status != TaskRunning {
			terminalCount++
		}
	}
	if terminalCount != maxRetainedTasks {
		t.Fatalf("expected %d terminal tasks, got %d", maxRetainedTasks, terminalCount)
	}
	if _, ok := hub.GetTask(oldRunning.ID); !ok {
		t.Fatal("old running task should survive terminal task compaction")
	}

	// 4. Persistence round-trip & restart compaction
	// Create another running task to test restart compaction
	runningTask := hub.CreateIssueTask([]string{"running.example.com"}, "acc_1", "http-01")

	// Create a new Hub instance from the same directory to simulate service restart
	hubRestart, err := NewHub(dataDir, nil)
	if err != nil {
		t.Fatalf("NewHub restart: %v", err)
	}

	recovered, ok := hubRestart.GetTask(runningTask.ID)
	if !ok {
		t.Fatalf("expected recovered task %s", runningTask.ID)
	}
	if recovered.Status != TaskFailed {
		t.Errorf("expected interrupted task status %s, got %s", TaskFailed, recovered.Status)
	}
	if !strings.Contains(recovered.Error, "服务重启，任务已中断") {
		t.Errorf("expected interrupted error message, got %q", recovered.Error)
	}
}

func TestIssueTaskPersistenceFailureLeavesStateUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	hub, err := NewHub(dataDir, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	initial, err := hub.CreateIssueTaskWithError([]string{"initial.example.com"}, "acc", "http-01")
	if err != nil {
		t.Fatalf("create initial task: %v", err)
	}
	certDir := hub.dir
	path := hub.tasksPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tasks: %v", err)
	}

	blocker := filepath.Join(dataDir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocker"), 0600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	hub.dir = blocker
	if task, err := hub.CreateIssueTaskWithError([]string{"failed-create.example.com"}, "acc", "http-01"); err == nil || task != nil {
		t.Fatalf("expected create persistence failure, task=%v err=%v", task, err)
	}
	hub.dir = certDir
	if _, ok := hub.GetTask("failed-create"); ok {
		t.Fatal("failed create must not add an in-memory task")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed create changed tasks file: err=%v", err)
	}

	hub.dir = blocker
	if err := hub.UpdateIssueTaskWithError(initial.ID, func(tk *IssueTask) {
		tk.Status = TaskFailed
		tk.Error = "must not commit"
	}); err == nil {
		t.Fatal("expected update persistence failure")
	}
	hub.dir = certDir
	updated, ok := hub.GetTask(initial.ID)
	if !ok || updated.Status != TaskRunning || updated.Error != "" {
		t.Fatalf("failed update changed in-memory task: %+v", updated)
	}
	after, err = os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed update changed tasks file: err=%v", err)
	}
}

func TestCertLogPersistenceAndEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dataDir := t.TempDir()
	hub, err := NewHub(dataDir, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	handlers := NewHandlers(hub, nil, nil)
	r := gin.New()
	rg := r.Group("/api/v1")
	handlers.Register(rg, rg)

	certID := "crt_test_log_999"

	// 1. Initially no log -> fallback structured info
	req := httptest.NewRequest(http.MethodGet, "/api/v1/certs/"+certID+"/log", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// Cert doesn't exist yet -> 404
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent cert, got %d: %s", w.Code, w.Body.String())
	}

	// Save log on disk
	logContent := "[2026-10-30 14:00:00] [准备账户] 账户就绪\n[2026-10-30 14:00:05] [完成入库] 证书保存成功"
	if err := hub.SaveCertLog(certID, logContent); err != nil {
		t.Fatalf("SaveCertLog: %v", err)
	}

	// Check on-disk file
	expectedPath := filepath.Join(dataDir, "certs", "logs", certID+".log")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected log file at %s: %v", expectedPath, err)
	}

	// Query /certs/:id/log -> should return on-disk log
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/certs/"+certID+"/log", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			ID      string `json:"id"`
			Running bool   `json:"running"`
			Log     string `json:"log"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Data.Running {
		t.Errorf("expected running=false for on-disk log")
	}
	if !strings.Contains(resp.Data.Log, "证书保存成功") {
		t.Errorf("expected log to contain '证书保存成功', got %q", resp.Data.Log)
	}

	// 2. Register live log -> should take precedence over on-disk log
	liveLogger := newCertLogger(certID, nil)
	liveLogger.Log("CA验证", "正在进行中的实时日志...")
	hub.RegisterLiveLog(certID, liveLogger)

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/certs/"+certID+"/log", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Data.Running {
		t.Errorf("expected running=true for live log")
	}
	if !strings.Contains(resp.Data.Log, "正在进行中的实时日志") {
		t.Errorf("expected live log content, got %q", resp.Data.Log)
	}

	// Unregister live log
	hub.UnregisterLiveLog(certID)

	// 3. Test /certs/tasks endpoints
	task := hub.CreateIssueTask([]string{"task.example.com"}, "acc_1", "http-01")
	hub.UpdateIssueTask(task.ID, func(t *IssueTask) {
		t.Log = "[2026-10-30 14:05:00] [准备账户] 任务测试日志"
	})

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/certs/tasks/"+task.ID, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for task, got %d: %s", w.Code, w.Body.String())
	}

	var taskResp struct {
		Data IssueTask `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &taskResp); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if taskResp.Data.ID != task.ID || !strings.Contains(taskResp.Data.Log, "任务测试日志") {
		t.Errorf("unexpected task response: %v", taskResp.Data)
	}

	// Delete cert log
	hub.DeleteCertLog(certID)
	if _, err := os.Stat(expectedPath); !os.IsNotExist(err) {
		t.Errorf("expected log file deleted, err = %v", err)
	}
}
