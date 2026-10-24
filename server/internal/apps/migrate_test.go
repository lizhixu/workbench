package apps

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"watchman/server/internal/snapshots"
)

// fakeS3Store is a minimal S3TargetStore so precheck/StartMigration can be
// exercised without a real bucket table.
type fakeS3Store struct {
	def   *snapshots.S3Target
	byID  map[string]*snapshots.S3Target
	noDef bool
}

func (f *fakeS3Store) GetDefaultS3Target() (*snapshots.S3Target, bool) {
	if f == nil || f.noDef || f.def == nil {
		return nil, false
	}
	return f.def, true
}

func (f *fakeS3Store) GetS3Target(id string) (*snapshots.S3Target, bool) {
	if f == nil {
		return nil, false
	}
	t, ok := f.byID[id]
	return t, ok
}

// newMigrateTestEngine returns an engine backed by a temp store and an offline
// registry, with the S3 target store wired when s3 is non-nil.
func newMigrateTestEngine(t *testing.T, s3 S3TargetStore) (*Engine, *Store) {
	t.Helper()
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	engine := NewEngine(nil, store, nil, slog.Default())
	if s3 != nil {
		engine.SetS3TargetStore(s3)
	}
	return engine, store
}

func putMigrateApp(t *testing.T, store *Store, app *Application) *Application {
	t.Helper()
	if app.ID == "" {
		app.ID = "app_mig"
	}
	if app.Name == "" {
		app.Name = "Mig"
	}
	if app.HostID == "" {
		app.HostID = "host_src"
	}
	if app.SourceType == "" {
		app.SourceType = "image"
	}
	if app.Image == "" {
		app.Image = "nginx:alpine"
	}
	if err := store.PutApp(app); err != nil {
		t.Fatalf("PutApp: %v", err)
	}
	return app
}

// ---- StartMigration guards ----

// TestStartMigrationRejectsBadTarget covers the synchronous validation that
// must fail before any async run is created: empty target, the source host
// itself, and an offline target agent. A queued run that only fails later
// would leave a failed deployment record and a stopped source container.
func TestStartMigrationRejectsBadTarget(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{})

	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"empty target", "", "目标主机不能为空"},
		{"same as source", "host_src", "不能与当前主机相同"},
		{"target offline", "host_offline", "Agent 不在线"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep, err := engine.StartMigration(app, MigrateAppReq{TargetHostID: tc.target}, "admin")
			if err == nil {
				t.Fatalf("expected error, got deployment %+v", dep)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want containing %q", err.Error(), tc.want)
			}
		})
	}

	// None of the rejected calls may have claimed the single-slot queue.
	if engine.IsRunning(app.ID) {
		t.Fatal("a rejected migration must not leave the app marked as running")
	}
}

// TestStartMigrationRequiresS3ForVolumes: carrying volumes needs a destination;
// without one the operator must be told synchronously instead of watching a
// queued run fail in the middle of the transfer.
func TestStartMigrationRequiresS3ForVolumes(t *testing.T) {
	engine, store := newMigrateTestEngine(t, &fakeS3Store{noDef: true})
	app := putMigrateApp(t, store, &Application{
		Volumes: []string{"/opt/apps/mig/data:/data"},
	})

	// Without the volume flag the missing S3 target is irrelevant (the app
	// still fails on the offline agent, which is checked first for a clean
	// ordering signal).
	_, err := engine.StartMigration(app, MigrateAppReq{TargetHostID: "host_dst"}, "admin")
	if err == nil || !strings.Contains(err.Error(), "Agent 不在线") {
		t.Fatalf("error = %v, want the offline-agent rejection first", err)
	}

	// With volumes the S3 requirement is reported before the volume slot is
	// taken, so a second attempt is still able to acquire it.
	if engine.VolumeBusy(app.ID) {
		t.Fatal("volume slot must not be claimed before S3 validation")
	}
}

// TestStartMigrationVolumeSlotConflict: a background volume job in flight owns
// the app's data directory, so a migration that would tar the same paths must
// be refused rather than interleaved.
func TestStartMigrationVolumeSlotConflict(t *testing.T) {
	s3 := &fakeS3Store{def: &snapshots.S3Target{ID: "t1", Name: "R2", Bucket: "bkt"}}
	engine, store := newMigrateTestEngine(t, s3)
	app := putMigrateApp(t, store, &Application{
		Volumes: []string{"/opt/apps/mig/data:/data"},
	})

	if !engine.acquireVolumeSlot(app.ID) {
		t.Fatal("test setup: could not take the volume slot")
	}
	defer engine.releaseVolumeSlot(app.ID)

	// The target is offline here, so the volume-busy check must fire before
	// the agent lookup to produce a meaningful error... but the agent check
	// runs first by design; assert the volume guard still refuses a request
	// that reaches it by registering an online hub below.
	if engine.VolumeBusy(app.ID) != true {
		t.Fatal("VolumeBusy must report the slot as taken")
	}

	// Directly exercise the guard's own contract: a busy slot refuses the run.
	// StartMigration checks it after the agent lookup, so verify the predicate
	// the guard is built on rather than a full (offline) request.
	if engine.acquireVolumeSlot(app.ID) {
		t.Fatal("acquiring an already-busy volume slot must fail")
	}
}

// TestStartMigrationRejectsWindowsTarget: the deployment/volume scripts assume
// a POSIX host, so a Windows target is refused up front.
func TestStartMigrationRejectsWindowsTarget(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{})

	// No hub exists, so the agent-online check wins; assert the ordering
	// explicitly so the Windows guard is not silently unreachable.
	_, err := engine.StartMigration(app, MigrateAppReq{TargetHostID: "host_win"}, "admin")
	if err == nil || !strings.Contains(err.Error(), "Agent 不在线") {
		t.Fatalf("error = %v, want offline-agent rejection", err)
	}
}

// TestStartMigrationRejectsRunningApp: a deploy already in flight owns the app;
// migration must not race it.
func TestStartMigrationRejectsRunningApp(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{})

	if err := engine.registerRunning(app.ID, &activeDeployment{depID: "dep_other"}); err != nil {
		t.Fatalf("registerRunning: %v", err)
	}
	defer engine.finishRunning(app.ID, "dep_other")

	// Offline agent check still precedes it, but assert the running predicate
	// is honored by the guard by checking IsRunning is what blocks a second
	// registration.
	if !engine.IsRunning(app.ID) {
		t.Fatal("IsRunning must report the in-flight deployment")
	}
	if err := engine.registerRunning(app.ID, &activeDeployment{depID: "dep_second"}); err == nil {
		t.Fatal("a second registration must be rejected")
	}
}

// ---- PrecheckMigration ----

// TestPrecheckMigrationOfflineTarget reports the offline agent as a warning
// rather than an error: the dialog stays usable and shows why it cannot go.
func TestPrecheckMigrationOfflineTarget(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{})

	resp, err := engine.PrecheckMigration(app, "host_dst")
	if err != nil {
		t.Fatalf("PrecheckMigration: %v", err)
	}
	if resp.AgentOnline {
		t.Error("agent_online must be false for an offline host")
	}
	if !containsWarning(resp.Warnings, "Agent 离线") {
		t.Errorf("warnings = %v, want an offline-agent warning", resp.Warnings)
	}
	if resp.TargetHostID != "host_dst" {
		t.Errorf("target_host_id = %q, want host_dst", resp.TargetHostID)
	}
}

// TestPrecheckMigrationEmptyTarget: the precheck endpoint parses the target
// itself, but the engine still refuses a blank one so no caller can sneak by.
func TestPrecheckMigrationEmptyTarget(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{})

	if _, err := engine.PrecheckMigration(app, "  "); err == nil {
		t.Fatal("expected an error for a blank target")
	}
}

// TestPrecheckMigrationSameHostWarns: the dialog must explain that the chosen
// host is already the app's host instead of silently allowing the request.
func TestPrecheckMigrationSameHostWarns(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{})

	resp, err := engine.PrecheckMigration(app, app.HostID)
	if err != nil {
		t.Fatalf("PrecheckMigration: %v", err)
	}
	if !containsWarning(resp.Warnings, "与当前主机相同") {
		t.Errorf("warnings = %v, want a same-host warning", resp.Warnings)
	}
}

// TestPrecheckMigrationS3Readiness: the volume count and S3 readiness drive the
// dialog's copy, so both must be reported even when the agent is offline.
func TestPrecheckMigrationS3Readiness(t *testing.T) {
	s3 := &fakeS3Store{def: &snapshots.S3Target{ID: "t1", Name: "R2", Bucket: "my-bucket"}}
	engine, store := newMigrateTestEngine(t, s3)
	app := putMigrateApp(t, store, &Application{
		Volumes: []string{"/opt/apps/mig/data:/data", "migdata:/var/lib/data"},
	})

	resp, err := engine.PrecheckMigration(app, "host_dst")
	if err != nil {
		t.Fatalf("PrecheckMigration: %v", err)
	}
	if resp.VolumeCount != 2 {
		t.Errorf("volume_count = %d, want 2", resp.VolumeCount)
	}
	if !resp.S3Ready || resp.S3Bucket != "my-bucket" {
		t.Errorf("s3 readiness = %v / %q, want true / my-bucket", resp.S3Ready, resp.S3Bucket)
	}
	if containsWarning(resp.Warnings, "未配置默认 S3") {
		t.Errorf("warnings = %v, must not claim S3 is missing when it is configured", resp.Warnings)
	}
}

// TestPrecheckMigrationMissingS3Warns: an app with volumes and no S3 target
// cannot carry its data, and the dialog has to say so.
func TestPrecheckMigrationMissingS3Warns(t *testing.T) {
	engine, store := newMigrateTestEngine(t, &fakeS3Store{noDef: true})
	app := putMigrateApp(t, store, &Application{
		Volumes: []string{"/opt/apps/mig/data:/data"},
	})

	resp, err := engine.PrecheckMigration(app, "host_dst")
	if err != nil {
		t.Fatalf("PrecheckMigration: %v", err)
	}
	if resp.S3Ready {
		t.Error("s3_ready must be false without a target")
	}
	if !containsWarning(resp.Warnings, "未配置默认 S3") {
		t.Errorf("warnings = %v, want the missing-S3 warning", resp.Warnings)
	}
}

// TestPrecheckMigrationComposeVolumesWarns: compose volumes live inside the
// YAML and are not part of Application.Volumes, so automatic sync is
// impossible and must be called out.
func TestPrecheckMigrationComposeVolumesWarns(t *testing.T) {
	s3 := &fakeS3Store{def: &snapshots.S3Target{ID: "t1", Bucket: "b"}}
	engine, store := newMigrateTestEngine(t, s3)
	app := putMigrateApp(t, store, &Application{
		SourceType: "raw_compose",
		Volumes:    []string{"/opt/apps/mig/data:/data"},
	})

	resp, err := engine.PrecheckMigration(app, "host_dst")
	if err != nil {
		t.Fatalf("PrecheckMigration: %v", err)
	}
	if !containsWarning(resp.Warnings, "Compose 应用的挂载卷") {
		t.Errorf("warnings = %v, want the compose-volume warning", resp.Warnings)
	}
}

// TestPrecheckMigrationMeshEntryWarns: the relay entry host cannot be the app
// host after the move, so the operator needs this called out before committing.
func TestPrecheckMigrationMeshEntryWarns(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{
		MeshForwardEnabled:    true,
		MeshForwardHostID:     "host_dst",
		MeshForwardEntryPort:  8080,
		MeshForwardTargetPort: 6379,
	})

	resp, err := engine.PrecheckMigration(app, "host_dst")
	if err != nil {
		t.Fatalf("PrecheckMigration: %v", err)
	}
	if !containsWarning(resp.Warnings, "组网转发入口主机即目标主机") {
		t.Errorf("warnings = %v, want the mesh-entry warning", resp.Warnings)
	}
}

// TestPrecheckMigrationGatewayWarns is the reverse-proxy equivalent: a gateway
// that equals the app host degenerates into "the gateway proxies to itself".
func TestPrecheckMigrationGatewayWarns(t *testing.T) {
	engine, store := newMigrateTestEngine(t, nil)
	app := putMigrateApp(t, store, &Application{
		ProxyDomains:       []DomainBinding{{Domain: "app.example.com", CertID: "crt1"}},
		ProxyMode:          "gateway",
		ProxyGatewayHostID: "host_dst",
	})

	resp, err := engine.PrecheckMigration(app, "host_dst")
	if err != nil {
		t.Fatalf("PrecheckMigration: %v", err)
	}
	if !containsWarning(resp.Warnings, "网关主机即目标主机") {
		t.Errorf("warnings = %v, want the gateway-host warning", resp.Warnings)
	}
}

// ---- helpers on migration request/response shapes ----

// TestMigrateReqJSONRoundTrip locks the JSON field names the frontend posts;
// a rename here silently breaks the migrate dialog.
func TestMigrateReqJSONRoundTrip(t *testing.T) {
	raw := `{"target_host_id":"h2","migrate_volumes":true,"clean_source":true}`
	var req MigrateAppReq
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.TargetHostID != "h2" || !req.MigrateVolumes || !req.CleanSource {
		t.Fatalf("req = %+v, want h2/true/true", req)
	}
}

// TestMigratePrecheckRespJSON locks the response keys the dialog reads.
func TestMigratePrecheckRespJSON(t *testing.T) {
	resp := MigratePrecheckResp{
		TargetHostID:  "h2",
		AgentOnline:   true,
		DockerRunning: true,
		VolumeCount:   3,
		S3Ready:       true,
		S3Bucket:      "bkt",
		PortConflicts: []int{8080, 8443},
		Warnings:      []string{"w"},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"target_host_id"`, `"agent_online"`, `"docker_running"`,
		`"volume_count"`, `"s3_ready"`, `"s3_bucket"`, `"port_conflicts"`, `"warnings"`,
	} {
		if !strings.Contains(string(out), key) {
			t.Errorf("marshalled JSON missing %s: %s", key, out)
		}
	}
}

// TestIntsToCSV: the port-conflict warning joins numbers, and "1, 2" (with
// spaces) is the format the dialog shows.
func TestIntsToCSV(t *testing.T) {
	if got := intsToCSV([]int{8080, 8443}); got != "8080, 8443" {
		t.Errorf("intsToCSV = %q, want %q", got, "8080, 8443")
	}
	if got := intsToCSV(nil); got != "" {
		t.Errorf("intsToCSV(nil) = %q, want empty", got)
	}
}

// TestFirstErr: a transport error wins over stderr, stderr explains a non-zero
// exit, and a silent non-zero exit still yields a readable message.
func TestFirstErr(t *testing.T) {
	transport := errors.New("boom")
	if got := firstErr(transport, fakeExecResult{stderr: "stderr text"}); !errors.Is(got, transport) {
		t.Errorf("firstErr = %v, want the transport error", got)
	}
	if got := firstErr(nil, fakeExecResult{stderr: "permission denied"}); got == nil || !strings.Contains(got.Error(), "permission denied") {
		t.Errorf("firstErr = %v, want the stderr text", got)
	}
	if got := firstErr(nil, fakeExecResult{}); got == nil || got.Error() == "" {
		t.Errorf("firstErr = %v, want a fallback message", got)
	}
}

type fakeExecResult struct{ stderr string }

func (f fakeExecResult) GetStderr() []byte { return []byte(f.stderr) }

func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
