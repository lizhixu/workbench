package apps

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// fakeUpgradeAgent answers every agent exec with success unless pullFail
// makes `docker pull` fail, and records every command in order so tests can
// prove the upgrade pull step runs before any container is touched. The
// probe's `docker inspect` answer is "running" so the single-container
// health check passes without a real host.
type fakeUpgradeAgent struct {
	mu       sync.Mutex
	cmds     []string
	pullFail bool
}

func (f *fakeUpgradeAgent) exec(hub *rpc.Hub, execID, command string, timeoutSec int32) (*agentpb.ExecResult, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, command)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(command, "docker pull "):
		if f.pullFail {
			return &agentpb.ExecResult{ExitCode: 1, Stderr: []byte("pull access denied")}, nil
		}
		return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte("Pulled")}, nil
	case strings.Contains(command, "docker inspect --format '{{.State.Status}}'"):
		return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte("running\n")}, nil
	default:
		return &agentpb.ExecResult{ExitCode: 0}, nil
	}
}

func (f *fakeUpgradeAgent) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakeUpgradeAgent) indexOf(substr string) int {
	for i, cmd := range f.commands() {
		if strings.Contains(cmd, substr) {
			return i
		}
	}
	return -1
}

func newUpgradeTestEnv(t *testing.T) (*Engine, *Store, *fakeUpgradeAgent) {
	t.Helper()
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	registerMigrateTestAgent(t, reg, "h1", "linux")
	e := NewEngine(reg, store, nil, slog.Default())
	fake := &fakeUpgradeAgent{}
	prev := execOnAgent
	execOnAgent = fake.exec
	t.Cleanup(func() { execOnAgent = prev })
	return e, store, fake
}

func putUpgradeTestApp(t *testing.T, store *Store, app *Application) *Application {
	t.Helper()
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	return app
}

func waitDeploymentTerminal(t *testing.T, store *Store, appID, depID string) *Deployment {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if d, ok := store.GetDeployment(appID, depID); ok && isTerminalDeploymentStatus(d.Status) {
			return d
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("deployment %s did not reach a terminal state in time", depID)
	return nil
}

func TestStartUpgradeImagePullsBeforeRun(t *testing.T) {
	e, store, fake := newUpgradeTestEnv(t)
	app := putUpgradeTestApp(t, store, &Application{
		ID: "app_upg_img", Name: "UpgImg", HostID: "h1",
		SourceType: "image", Image: "nginx:alpine",
	})

	dep, err := e.StartUpgrade(app, "tester")
	if err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}
	if dep.Trigger != TriggerUpgrade {
		t.Errorf("Trigger = %q, want %q", dep.Trigger, TriggerUpgrade)
	}
	got := waitDeploymentTerminal(t, store, app.ID, dep.ID)
	if got.Status != DeploySuccess {
		t.Fatalf("status = %q, error = %q, log = %q", got.Status, got.Error, got.BuildLog)
	}
	pullIdx := fake.indexOf("docker pull")
	runIdx := fake.indexOf("docker run -d")
	if pullIdx < 0 {
		t.Errorf("docker pull was never issued; commands: %v", fake.commands())
	}
	if runIdx < 0 {
		t.Errorf("docker run was never issued; commands: %v", fake.commands())
	}
	if pullIdx >= 0 && runIdx >= 0 && pullIdx > runIdx {
		t.Errorf("pull ran after run (pull=%d, run=%d); the old image would be redeployed", pullIdx, runIdx)
	}
	if !strings.Contains(got.BuildLog, "docker pull") || !strings.Contains(got.BuildLog, "nginx:alpine") {
		t.Errorf("build log does not show the pull step: %q", got.BuildLog)
	}
}

func TestStartUpgradePullFailureKeepsOldContainer(t *testing.T) {
	e, store, fake := newUpgradeTestEnv(t)
	fake.pullFail = true
	app := putUpgradeTestApp(t, store, &Application{
		ID: "app_upg_fail", Name: "UpgFail", HostID: "h1",
		SourceType: "image", Image: "nginx:alpine",
	})

	dep, err := e.StartUpgrade(app, "tester")
	if err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}
	got := waitDeploymentTerminal(t, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "拉取镜像失败") {
		t.Errorf("Error = %q, want it to name the pull failure", got.Error)
	}
	if fake.indexOf("docker run -d") >= 0 {
		t.Errorf("docker run was issued after a failed pull; the rollout must stop before touching containers")
	}
}

func TestStartUpgradeRawComposePullsBeforeUp(t *testing.T) {
	e, store, fake := newUpgradeTestEnv(t)
	app := putUpgradeTestApp(t, store, &Application{
		ID: "app_upg_cmp", Name: "UpgCmp", HostID: "h1",
		SourceType:     "raw_compose",
		ComposeContent: "services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"8080:80\"\n",
	})

	dep, err := e.StartUpgrade(app, "tester")
	if err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}
	got := waitDeploymentTerminal(t, store, app.ID, dep.ID)
	if got.Status != DeploySuccess {
		t.Fatalf("status = %q, error = %q, log = %q", got.Status, got.Error, got.BuildLog)
	}
	pullIdx := fake.indexOf("docker compose")
	// The first compose command must be the pull, not the up.
	if pullIdx < 0 || !strings.Contains(fake.commands()[pullIdx], " pull") {
		t.Errorf("first compose command is not a pull; commands: %v", fake.commands())
	}
	upIdx := fake.indexOf("up -d --remove-orphans")
	if upIdx < 0 {
		t.Errorf("compose up was never issued; commands: %v", fake.commands())
	}
	if pullIdx >= 0 && upIdx >= 0 && pullIdx > upIdx {
		t.Errorf("pull ran after up (pull=%d, up=%d)", pullIdx, upIdx)
	}
}

func TestStartUpgradeRejectsWhileRunning(t *testing.T) {
	e, store, _ := newUpgradeTestEnv(t)
	app := putUpgradeTestApp(t, store, &Application{
		ID: "app_upg_busy", Name: "UpgBusy", HostID: "h1",
		SourceType: "image", Image: "nginx:alpine",
	})

	dep, err := e.StartUpgrade(app, "tester")
	if err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}
	if _, err := e.StartUpgrade(app, "tester"); err == nil {
		t.Errorf("second StartUpgrade while one is running must fail")
	} else if !strings.Contains(err.Error(), "already running") {
		t.Errorf("second StartUpgrade error = %q, want already-running", err.Error())
	}
	waitDeploymentTerminal(t, store, app.ID, dep.ID)
}

func TestUpgradeAppHTTPHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, store, nil, slog.Default())
	handlers := NewAppHandlers(nil, store, engine)

	r := gin.New()
	rg := r.Group("")
	handlers.RegisterAppRoutes(rg, rg, rg)

	// Unknown app -> 404.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/apps/nonexistent/upgrade", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown app, got %d", w.Code)
	}

	app := &Application{
		ID: "app_upg_http", Name: "UpgHTTP", HostID: "host_1",
		SourceType: "image", Image: "nginx:alpine",
	}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/apps/app_upg_http/upgrade", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"trigger":"upgrade"`) {
		t.Errorf("response does not carry trigger=upgrade: %s", w.Body.String())
	}
}
