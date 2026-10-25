package apps

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Terminal-state guards: once a deployment record reaches success, failure
// or cancelled, no later stage, cancel or success path may rewrite it.
// These tests pin the guard against the three pairwise overwrites the
// review found (probe-cancel -> fail, swap-cancel -> success, and stage
// status writes resurrecting a cancelled record).

func newTerminalTestEngine(t *testing.T) (*Engine, *Store, *Application, *Deployment) {
	t.Helper()
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, store, nil, slog.Default())
	app := &Application{ID: "app_term", Name: "Term", HostID: "h1", SourceType: "image", Image: "nginx:alpine"}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	dep := &Deployment{ID: "dep_term", AppID: app.ID, Status: DeployBuilding, StartedAt: time.Now()}
	if err := store.AddDeployment(dep); err != nil {
		t.Fatalf("AddDeployment: %v", err)
	}
	return engine, store, app, dep
}

func TestTerminalGuardFailKeepsCancelled(t *testing.T) {
	engine, store, app, dep := newTerminalTestEngine(t)
	// Simulate a cancel that already finalized the record (probe-cancel
	// path): fail() afterwards must not overwrite it or drop CancelledBy.
	_ = store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		d.Status = DeployCancelled
		d.CancelledBy = "operator_bob"
		d.Error = "部署已被用户取消"
	})
	engine.fail(app, dep, &logBuffer{}, "健康检查未通过，已保留旧版本容器")
	got, _ := store.GetDeployment(app.ID, dep.ID)
	if got.Status != DeployCancelled {
		t.Errorf("fail() overwrote cancelled record: status = %q", got.Status)
	}
	if got.CancelledBy != "operator_bob" {
		t.Errorf("fail() dropped CancelledBy: %q", got.CancelledBy)
	}
	if got.Error != "部署已被用户取消" {
		t.Errorf("fail() overwrote cancel error: %q", got.Error)
	}
}

func TestTerminalGuardCancelKeepsFailed(t *testing.T) {
	engine, store, app, dep := newTerminalTestEngine(t)
	engine.fail(app, dep, &logBuffer{}, "构建失败")
	engine.cleanupCancelled(app, dep, nil, "", "", &logBuffer{})
	got, _ := store.GetDeployment(app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Errorf("cleanupCancelled overwrote failed record: status = %q", got.Status)
	}
	if got.Error != "构建失败" {
		t.Errorf("cleanupCancelled overwrote fail error: %q", got.Error)
	}
}

func TestSetDeploymentStatusNoResurrection(t *testing.T) {
	engine, store, app, dep := newTerminalTestEngine(t)
	if !engine.setDeploymentStatus(app.ID, dep.ID, DeployDeploying) {
		t.Fatalf("expected deploying transition to run")
	}
	_ = store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		d.Status = DeployCancelled
		d.CancelledBy = "op"
	})
	if engine.setDeploymentStatus(app.ID, dep.ID, DeployBuilding) {
		t.Errorf("status write must not run on a terminal record")
	}
	got, _ := store.GetDeployment(app.ID, dep.ID)
	if got.Status != DeployCancelled {
		t.Errorf("cancelled record resurrected: status = %q", got.Status)
	}
}

func TestFinishDeploySuccessKeepsCancelled(t *testing.T) {
	engine, store, app, dep := newTerminalTestEngine(t)
	_ = store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		d.Status = DeployCancelled
		d.CancelledBy = "op"
	})
	engine.finishDeploySuccess(app, dep, "abc", "watchman-app-app_term:abc", "watchman-app-app_term", &logBuffer{})
	got, _ := store.GetDeployment(app.ID, dep.ID)
	if got.Status != DeployCancelled {
		t.Errorf("finishDeploySuccess overwrote cancelled record: status = %q", got.Status)
	}
}

func TestFinishDeploySuccessDeletedApp(t *testing.T) {
	engine, store, app, dep := newTerminalTestEngine(t)
	if err := store.DeleteApp(app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	engine.finishDeploySuccess(app, dep, "abc", "watchman-app-app_term:abc", "watchman-app-app_term", &logBuffer{})
	got, _ := store.GetDeployment(app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Errorf("expected deleted-app finish to fail, got status %q", got.Status)
	}
	if !strings.Contains(got.Error, "已被删除") {
		t.Errorf("expected deletion note in error, got %q", got.Error)
	}
}

func TestNextContainerNameScoped(t *testing.T) {
	a := nextContainerName("watchman-app-x", "dep_11111111")
	b := nextContainerName("watchman-app-x", "dep_22222222")
	if a == b {
		t.Errorf("two runs must not share a -next name: %q", a)
	}
	if !strings.HasPrefix(a, "watchman-app-x-next-") {
		t.Errorf("unexpected -next name shape: %q", a)
	}
}

func TestParseTarMembers(t *testing.T) {
	out := "./data/app.db\ndata/logs/\n./\n.\n/abs/path\n../escape\na/../b\nplain.txt\n"
	got := parseTarMembers(out)
	want := []string{"data/app.db", "plain.txt"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestTargetContainerCleanupScript(t *testing.T) {
	script := targetContainerCleanupScript("watchman-app-x")
	if !strings.Contains(script, "docker ps -aq --filter") {
		t.Errorf("expected a docker name filter, got: %s", script)
	}
	if !strings.Contains(script, "name=^/watchman-app-x-next") {
		t.Errorf("expected -next prefix filter, got: %s", script)
	}
	// A shell glob would expand against filenames, never container names.
	if strings.Contains(script, "-next*") {
		t.Errorf("must not use a shell glob for container names: %s", script)
	}
}
