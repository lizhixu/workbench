package apps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// The 2026-11-18 mysql incident: the first create deploy timed out on the
// server side (agent exec timeout) while the agent-side `docker run` still
// finished, leaving a -next container holding 100.64.0.7:3306. Every retry
// then died with a bare "port is already allocated" at the docker-run step.
// These tests pin the guards: timeout cleanup, preflight orphan sweep,
// fail-fast foreign port precheck, and stop-first redeploy of a port-bound
// app (stop the live container, then start the replacement; any failure
// after the stop restarts the old container).

func TestParseDockerPsPorts(t *testing.T) {
	out := `hw-next-dep_bf150856adda803b 100.64.0.7:3306->3306/tcp, 33060/tcp
web 0.0.0.0:8080->80/tcp, :::8080->80/tcp
db 127.0.0.1:5432->5432/tcp
stopped
`
	names, bindings := parseDockerPsPorts(out)
	if len(names) != 4 {
		t.Fatalf("names = %v, want 4 entries", names)
	}
	if got := bindings["hw-next-dep_bf150856adda803b"]; len(got) != 1 || got[0].ip != "100.64.0.7" || got[0].port != 3306 {
		t.Errorf("hw-next bindings = %+v, want [{100.64.0.7 3306}]", got)
	}
	if got := bindings["web"]; len(got) != 2 {
		t.Errorf("web bindings = %+v, want 2 entries (0.0.0.0 + ::)", got)
	}
	if got := bindings["db"]; len(got) != 1 || got[0].ip != "127.0.0.1" || got[0].port != 5432 {
		t.Errorf("db bindings = %+v, want [{127.0.0.1 5432}]", got)
	}
	if got := bindings["stopped"]; len(got) != 0 {
		t.Errorf("stopped bindings = %+v, want none", got)
	}
	if _, ok := bindings["missing"]; ok {
		t.Errorf("unexpected bindings for missing container")
	}
}

func TestPortBindingConflicts(t *testing.T) {
	cases := []struct {
		name     string
		newIP    string
		port     int
		held     publishedPort
		conflict bool
	}{
		{"same ip", "100.64.0.7", 3306, publishedPort{"100.64.0.7", 3306}, true},
		{"different specific ips share a port", "100.64.0.7", 3306, publishedPort{"127.0.0.1", 3306}, false},
		{"new wildcard covers holder", "0.0.0.0", 3306, publishedPort{"127.0.0.1", 3306}, true},
		{"holder wildcard covers new", "100.64.0.7", 3306, publishedPort{"0.0.0.0", 3306}, true},
		{"holder ipv6 wildcard covers new", "100.64.0.7", 3306, publishedPort{"::", 3306}, true},
		{"empty new ip means public", "", 3306, publishedPort{"127.0.0.1", 3306}, true},
		{"different port", "100.64.0.7", 3307, publishedPort{"100.64.0.7", 3306}, false},
	}
	for _, tc := range cases {
		if got := portBindingConflicts(tc.newIP, tc.port, tc.held); got != tc.conflict {
			t.Errorf("%s: portBindingConflicts(%q, %d, %+v) = %v, want %v",
				tc.name, tc.newIP, tc.port, tc.held, got, tc.conflict)
		}
	}
}

// fakePreflightAgent scripts `docker ps` output and records every command,
// so tests can prove the sweep runs before `docker run` and that a foreign
// port holder blocks the deploy before any container is touched.
type fakePreflightAgent struct {
	mu         sync.Mutex
	cmds       []string
	psOutput   string
	runErr     error         // non-nil: `docker run` fails with this error (timeout)
	runExit    int32         // non-zero: `docker run` returns this exit code
	runDelay   time.Duration // >0: `docker run` sleeps this long before returning
	stopErr    error         // non-nil: `docker stop` fails with this error
	probeState string        // `docker inspect --format '{{.State.Status}}'` result; "" = "running"
}

func (f *fakePreflightAgent) exec(_ context.Context, hub *rpc.Hub, execID, command string, timeoutSec, stallSec int32, _ *logBuffer) (*agentpb.ExecResult, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, command)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(command, "docker ps -a "):
		return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte(f.psOutput)}, nil
	case strings.HasPrefix(command, "docker stop "):
		if f.stopErr != nil {
			return nil, f.stopErr
		}
		return &agentpb.ExecResult{ExitCode: 0}, nil
	case strings.Contains(command, "docker inspect --format '{{.State.Status}}'"):
		state := f.probeState
		if state == "" {
			state = "running"
		}
		return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte(state + "\n")}, nil
	case strings.Contains(command, "docker run -d "):
		if f.runDelay > 0 {
			time.Sleep(f.runDelay)
		}
		if f.runErr != nil {
			return nil, f.runErr
		}
		if f.runExit != 0 {
			return &agentpb.ExecResult{ExitCode: f.runExit}, nil
		}
		return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte("deadbeef\n")}, nil
	default:
		return &agentpb.ExecResult{ExitCode: 0}, nil
	}
}

func (f *fakePreflightAgent) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func (f *fakePreflightAgent) indexOf(substr string) int {
	for i, cmd := range f.commands() {
		if strings.Contains(cmd, substr) {
			return i
		}
	}
	return -1
}

// indexOfExact matches one whole command. The docker-run command embeds a
// `docker rm -f <next>` prefix of its own, so a substring search cannot
// tell the sweep/cleanup apart from the run step.
func (f *fakePreflightAgent) indexOfExact(want string) int {
	for i, cmd := range f.commands() {
		if cmd == want {
			return i
		}
	}
	return -1
}

func newPreflightTestEnv(t *testing.T, psOutput string, runErr error) (*Engine, *Store, *fakePreflightAgent) {
	t.Helper()
	e, store, _ := newUpgradeTestEnv(t)
	fake := &fakePreflightAgent{psOutput: psOutput, runErr: runErr}
	prev := execOnAgent
	execOnAgent = fake.exec
	t.Cleanup(func() { execOnAgent = prev })
	return e, store, fake
}

func putPreflightTestApp(t *testing.T, store *Store) *Application {
	t.Helper()
	app := &Application{
		ID: "app_preflight", Name: "Preflight", HostID: "h1",
		SourceType: "image", Image: "mysql:8.0", ContainerName: "hw",
		Ports: []PortMapping{{Host: 3306, Container: 3306}},
	}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestDeployPreflightSweepsStaleNext(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw-next-dep_bf150856adda803b 100.64.0.7:3306->3306/tcp, 33060/tcp\n", nil)
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeploySuccess {
		t.Fatalf("status = %q, error = %q, log = %q", got.Status, got.Error, got.BuildLog)
	}
	sweepIdx := fake.indexOfExact("docker rm -f 'hw-next-dep_bf150856adda803b'")
	runIdx := fake.indexOf("docker run -d ")
	if sweepIdx < 0 {
		t.Fatalf("stale -next container was not removed; commands: %v", fake.commands())
	}
	if runIdx < 0 || sweepIdx > runIdx {
		t.Fatalf("sweep (%d) must run before docker run (%d); commands: %v", sweepIdx, runIdx, fake.commands())
	}
	if !strings.Contains(got.BuildLog, "已清理上次失败残留的容器 hw-next-dep_bf150856adda803b") {
		t.Errorf("build log %q: missing sweep note", got.BuildLog)
	}
}

func TestDeployPreflightBlocksForeignPortHolder(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"other-mysql 0.0.0.0:3306->3306/tcp\n", nil)
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "端口 3306 已被容器 other-mysql 占用") {
		t.Errorf("error = %q, want foreign-holder message", got.Error)
	}
	if fake.indexOf("docker run -d ") >= 0 {
		t.Errorf("docker run must not start when the port is held; commands: %v", fake.commands())
	}
}

// A redeploy of a port-bound app must stop the live container first and
// then start the replacement (brief downtime), instead of failing fast
// with "port is already allocated" or telling the operator to stop it by
// hand. This is the case that was previously
// TestDeployPreflightBlocksOwnRunningContainer.
func TestDeployStopFirstRedeploysPortBoundApp(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw 100.64.0.7:3306->3306/tcp\n", nil)
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeploySuccess {
		t.Fatalf("status = %q, error = %q, log = %q", got.Status, got.Error, got.BuildLog)
	}
	stopIdx := fake.indexOf("docker stop -t 30 'hw'")
	runIdx := fake.indexOf("docker run -d ")
	if stopIdx < 0 {
		t.Fatalf("live container was not stopped first; commands: %v", fake.commands())
	}
	if runIdx < 0 || stopIdx > runIdx {
		t.Fatalf("stop (%d) must run before docker run (%d); commands: %v", stopIdx, runIdx, fake.commands())
	}
	if !strings.Contains(got.BuildLog, "先停止旧容器再启动新容器") {
		t.Errorf("build log %q: missing stop-first note", got.BuildLog)
	}
	if fake.indexOf("docker start 'hw'") >= 0 {
		t.Errorf("successful deploy must not restart the old container; commands: %v", fake.commands())
	}
}

// When the old container cannot be stopped, the deploy aborts before any
// replacement is started: the port stays held by the live container and
// the app keeps serving the old version.
func TestDeployStopFirstStopFailure(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw 100.64.0.7:3306->3306/tcp\n", nil)
	fake.stopErr = errors.New("agent exec timeout")
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "停止旧容器失败") {
		t.Errorf("error = %q, want stop-failure message", got.Error)
	}
	if fake.indexOf("docker run -d ") >= 0 {
		t.Errorf("docker run must not start when the old container is still running; commands: %v", fake.commands())
	}
}

// A failure after the stop must bring the old container back: the deploy
// fails, but the app keeps serving the previous version instead of
// staying down with a stopped container.
func TestDeployStopFirstRunFailureRollsBack(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw 100.64.0.7:3306->3306/tcp\n", errors.New("agent exec timeout"))
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "旧容器已重新启动") {
		t.Errorf("error = %q, want rollback note", got.Error)
	}
	startIdx := fake.indexOf("docker start 'hw'")
	runIdx := fake.indexOf("docker run -d ")
	if startIdx < 0 {
		t.Fatalf("old container was not restarted after the failed run; commands: %v", fake.commands())
	}
	if runIdx < 0 || startIdx < runIdx {
		t.Fatalf("restart (%d) must run after docker run (%d); commands: %v", startIdx, runIdx, fake.commands())
	}
}

// TestDeployStopFirstRunExitFailureRemovesNextBeforeRestart pins the same
// order for the docker-run non-zero-exit branch as the probe branch: the
// replacement must be removed BEFORE the old container is restarted, or
// `docker start` dies with "port is already allocated" and the app stays
// down. (The transport-error branch already did it in this order; the
// exit-code branch used to restart first and remove the -next async.)
func TestDeployStopFirstRunExitFailureRemovesNextBeforeRestart(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw 100.64.0.7:3306->3306/tcp\n", nil)
	fake.runExit = 125
	prevRm := removeContainerFn
	removeContainerFn = func(_ *Engine, _ *rpc.Hub, name string) {
		fake.mu.Lock()
		fake.cmds = append(fake.cmds, "docker rm -f '"+name+"'")
		fake.mu.Unlock()
	}
	t.Cleanup(func() { removeContainerFn = prevRm })
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "启动新容器失败 (exit 125)") || !strings.Contains(got.Error, "旧容器已重新启动") {
		t.Errorf("error = %q, want run-failure message with rollback note", got.Error)
	}
	want := fmt.Sprintf("docker rm -f 'hw-next-%s'", dep.ID)
	rmIdx := fake.indexOfExact(want)
	startIdx := fake.indexOf("docker start 'hw'")
	if rmIdx < 0 {
		t.Fatalf("removal %q not issued; commands: %v", want, fake.commands())
	}
	if startIdx < 0 {
		t.Fatalf("old container was not restarted after the failed run; commands: %v", fake.commands())
	}
	if rmIdx > startIdx {
		t.Fatalf("removal (%d) must run before restart (%d); commands: %v", rmIdx, startIdx, fake.commands())
	}
}

func TestDeployStopFirstProbeFailureRollsBack(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw 100.64.0.7:3306->3306/tcp\n", nil)
	fake.probeState = "exited"
	// Route removeContainer through a recording stub (same pattern as
	// the cancel-order test): in this env the real one is a no-op.
	prevRm := removeContainerFn
	removeContainerFn = func(_ *Engine, _ *rpc.Hub, name string) {
		fake.mu.Lock()
		fake.cmds = append(fake.cmds, "docker rm -f '"+name+"'")
		fake.mu.Unlock()
	}
	t.Cleanup(func() { removeContainerFn = prevRm })
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "健康检查未通过") || !strings.Contains(got.Error, "旧容器已重新启动") {
		t.Errorf("error = %q, want probe-failure message with rollback note", got.Error)
	}
	// The replacement must be removed BEFORE the old container is
	// restarted: it holds the host port, so a restart first dies with
	// "port is already allocated" and the app stays down.
	want := fmt.Sprintf("docker rm -f 'hw-next-%s'", dep.ID)
	rmIdx := fake.indexOfExact(want)
	startIdx := fake.indexOf("docker start 'hw'")
	if rmIdx < 0 {
		t.Fatalf("removal %q not issued; commands: %v", want, fake.commands())
	}
	if startIdx < 0 {
		t.Fatalf("old container was not restarted after the failed probe; commands: %v", fake.commands())
	}
	if rmIdx > startIdx {
		t.Fatalf("removal (%d) must run before restart (%d); commands: %v", rmIdx, startIdx, fake.commands())
	}
}

// A cancel landing after the stop must restart the old container too.
// The cancel is issued only once the stop command has been observed, so
// the test does not depend on the pipeline reaching the probe in time;
// the restart itself is then awaited (the record finalizes as cancelled
// before the pipeline goroutine wakes up from the probe sleep).
func TestDeployStopFirstCancelRestartsOld(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw 100.64.0.7:3306->3306/tcp\n", nil)
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for fake.indexOf("docker stop -t 30 'hw'") < 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.indexOf("docker stop -t 30 'hw'") < 0 {
		t.Fatalf("stop step was not reached; commands: %v", fake.commands())
	}
	if !e.CancelDeployment(app.ID, "tester") {
		t.Fatalf("CancelDeployment: no active deployment")
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	deadline = time.Now().Add(10 * time.Second)
	for fake.indexOf("docker start 'hw'") < 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.indexOf("docker start 'hw'") < 0 {
		t.Errorf("old container was not restarted after cancel; commands: %v", fake.commands())
	}
}

func TestDeployRunTimeoutCleansNext(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t, "", errors.New("agent exec timeout"))
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "agent exec timeout") {
		t.Errorf("error = %q, want the timeout cause", got.Error)
	}
	// The cleanup must target this run's -next name, after the failed run.
	want := fmt.Sprintf("docker rm -f 'hw-next-%s'", dep.ID)
	cleanIdx := fake.indexOfExact(want)
	runIdx := fake.indexOf("docker run -d ")
	if cleanIdx < 0 {
		t.Fatalf("timeout cleanup %q not issued; commands: %v", want, fake.commands())
	}
	if runIdx < 0 || cleanIdx < runIdx {
		t.Fatalf("cleanup (%d) must run after docker run (%d); commands: %v", cleanIdx, runIdx, fake.commands())
	}
}

// TestDeployStopFirstCancelRemovesNextBeforeRestart pins the order of the
// cancel path for a port-bound app: the replacement -next container holds
// the host port, so it must be removed BEFORE the old container is
// restarted, or `docker start` dies with "port is already allocated" and
// the app stays down. The run step is slowed so the cancel lands while it
// is still in flight: nextName is only registered after the run returns,
// so CancelDeployment's async removal does not fire and the order below is
// entirely the pipeline's cancelled() closure. With the old order
// (restart first, cleanup later) this test fails deterministically.
func TestDeployStopFirstCancelRemovesNextBeforeRestart(t *testing.T) {
	e, store, fake := newPreflightTestEnv(t,
		"hw 100.64.0.7:3306->3306/tcp\n", nil)
	fake.runDelay = 300 * time.Millisecond
	// removeContainer talks to the agent through hub.Send directly, so
	// the execOnAgent stub cannot see it. Route it through a recording
	// stub in the same command format it would send, so the order below
	// is observable. (In this env the real one is a no-op: the hub has
	// no live stream.)
	prevRm := removeContainerFn
	removeContainerFn = func(_ *Engine, _ *rpc.Hub, name string) {
		fake.mu.Lock()
		fake.cmds = append(fake.cmds, "docker rm -f '"+name+"'")
		fake.mu.Unlock()
	}
	t.Cleanup(func() { removeContainerFn = prevRm })
	app := putPreflightTestApp(t, store)

	dep, err := e.StartDeployment(app, "manual", "tester")
	if err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for fake.indexOf("docker run -d ") < 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.indexOf("docker run -d ") < 0 {
		t.Fatalf("run step was not reached; commands: %v", fake.commands())
	}
	if !e.CancelDeployment(app.ID, "tester") {
		t.Fatalf("CancelDeployment: no active deployment")
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
	// CancelDeployment marks the record cancelled synchronously, but the
	// pipeline only reaches the cancelled() closure once the in-flight
	// run step returns, so wait for the restart before asserting order.
	deadline = time.Now().Add(10 * time.Second)
	for fake.indexOf("docker start 'hw'") < 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// The standalone removal (exact match: the run command itself embeds a
	// `docker rm -f <next>` prefix, so a substring search cannot tell them
	// apart) must come before the restart of the old container.
	want := fmt.Sprintf("docker rm -f 'hw-next-%s'", dep.ID)
	rmIdx := fake.indexOfExact(want)
	startIdx := fake.indexOf("docker start 'hw'")
	if rmIdx < 0 {
		t.Fatalf("removal %q not issued; commands: %v", want, fake.commands())
	}
	if startIdx < 0 {
		t.Fatalf("old container was not restarted; commands: %v", fake.commands())
	}
	if rmIdx > startIdx {
		t.Fatalf("removal (%d) must run before restart (%d); commands: %v", rmIdx, startIdx, fake.commands())
	}
}

// TestRemoveContainerNoStreamReturnsImmediately pins the Send-false
// early return in removeContainer: with no live stream to the agent the
// removal can never be answered, so waiting the full 30s for a result
// that cannot come would only stall the caller (the cancel path calls
// it synchronously). The old code waited the whole 30s here.
func TestRemoveContainerNoStreamReturnsImmediately(t *testing.T) {
	e, _, _ := newPreflightTestEnv(t, "", nil)
	hub := e.reg.Hub("h1")
	if hub == nil {
		t.Fatal("no hub for test agent h1")
	}
	start := time.Now()
	e.removeContainer(hub, "ghost")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("removeContainer took %v with no live stream, want an immediate return", d)
	}
}
