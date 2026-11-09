package apps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// ---- 缺陷 1：裸 tar 恢复的去层判定 ----

// restoreStripComponents 只在布局无歧义时才去层：GNU tar 对
// --strip-components 超出成员深度的成员是静默丢弃（exit 0），所以平铺、
// 混杂、单个顶层文件或扫描不完整的归档一律不去层。
func TestRestoreStripComponents(t *testing.T) {
	scanTar := func(t *testing.T, entries map[string]string) *tarScanResult {
		t.Helper()
		raw := writeTestTar(t, entries)
		path := filepath.Join(t.TempDir(), "scan.tar")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := scanUploadTar(path)
		if err != nil {
			t.Fatalf("scanUploadTar: %v", err)
		}
		return res
	}
	cand := func(spec, host, container string) volumeScanCandidate {
		return volumeScanCandidate{Spec: spec, HostPath: host, ContainerPath: container}
	}
	pbData := cand("/opt/data:/app/pb_data", "/opt/data", "/app/pb_data")
	data := cand("/opt/data:/data", "/opt/data", "/data")

	cases := []struct {
		name string
		res  *tarScanResult
		c    volumeScanCandidate
		want int
	}{
		{"docker-cp 形态去一层", scanTar(t, map[string]string{"pb_data/data.db": "x", "pb_data/sub/x.txt": "x"}), pbData, 1},
		{"宿主目录打包去一层", scanTar(t, map[string]string{"data/data.db": "x"}), data, 1},
		{"从父目录打包按容器路径深度去层", scanTar(t, map[string]string{"app/pb_data/data.db": "x"}), pbData, 2},
		{"父目录打包但混有同层文件不去层", scanTar(t, map[string]string{"app/pb_data/data.db": "x", "app/other.txt": "x"}), pbData, 0},
		{"平铺不去层", scanTar(t, map[string]string{"data.db": "x", "auxiliary.db": "x"}), pbData, 0},
		{"多顶层不去层", scanTar(t, map[string]string{"a/keep": "x", "b/keep": "x"}), pbData, 0},
		{"单个顶层文件绝不去层", scanTar(t, map[string]string{"data.db": "x"}), pbData, 0},
		{"顶层目录与卷基名无关不去层", scanTar(t, map[string]string{"backup/data.db": "x"}), pbData, 0},
		{"带前导点的顶层目录按规范化后基名去一层", scanTar(t, map[string]string{".data/data.db": "x"}), data, 1},
		{"快照清单不去层", mustScanSnapshot(t), pbData, 0},
		{"扫描不完整不去层", &tarScanResult{complete: false, topDirs: []string{"pb_data"}, topHasChild: map[string]bool{"pb_data": true}}, pbData, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := restoreStripComponents(tc.res, tc.c); got != tc.want {
				t.Errorf("restoreStripComponents = %d, want %d (topDirs=%v complete=%v manifest=%v)",
					got, tc.want, tc.res.topDirs, tc.res.complete, tc.res.manifest != nil)
			}
		})
	}
}

func mustScanSnapshot(t *testing.T) *tarScanResult {
	t.Helper()
	raw := writeTestSnapshotTar(t, &SnapshotManifest{
		Version: "1", AppID: "app1", ArchiveID: "vbk_m",
		Volumes: []VolumeItemArchive{{VolumeSpec: "/opt/data:/data", HostPath: "/opt/data", ContainerPath: "/data"}},
	})
	path := filepath.Join(t.TempDir(), "snap.tar.gz")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := scanUploadTar(path)
	if err != nil {
		t.Fatalf("scanUploadTar(snapshot): %v", err)
	}
	if res.manifest == nil {
		t.Fatalf("snapshot tar not recognised as a manifest")
	}
	return res
}

// ---- 缺陷 1：恢复脚本真实解包落点 ----

// TestVolumeMultiRestoreScriptRealUnpack runs the generated restore script
// through a real sh instead of only asserting its text: the bare-tar branch
// used to unpack docker-cp-shaped archives one level too deep, and a string
// assertion cannot catch a wrong --strip-components value.
func TestVolumeMultiRestoreScriptRealUnpack(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh in this environment")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar in this environment")
	}
	cases := []struct {
		name    string
		entries map[string]string
		strip   int
		want    []string // files expected at the dest root
		notWant []string // paths that must NOT appear nested below the root
	}{
		{"docker-cp 形态 strip 1", map[string]string{"pb_data/data.db": "db", "pb_data/sub/x.txt": "x"}, 1,
			[]string{"data.db", "sub/x.txt"}, []string{"pb_data/data.db"}},
		{"父目录形态 strip 2", map[string]string{"app/pb_data/data.db": "db"}, 2,
			[]string{"data.db"}, []string{"app/pb_data/data.db"}},
		{"平铺 strip 0", map[string]string{"data.db": "db", "auxiliary.db": "aux"}, 0,
			[]string{"data.db", "auxiliary.db"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archPath := filepath.Join(dir, "vbk.tar")
			if err := os.WriteFile(archPath, writeTestTar(t, tc.entries), 0o644); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(dir, "dest")
			if err := os.MkdirAll(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			// A pre-existing file proves the clean wipe runs before the unpack.
			if err := os.WriteFile(filepath.Join(dest, "stale.txt"), []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			script := volumeMultiRestoreScript(archPath, filepath.Join(dir, "safety"), true,
				[]RestoreTarget{{Index: 0, DestDir: dest, Spec: "/opt/data:/data", Strip: tc.strip}})
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("restore script failed: %v\n%s", err, out)
			}
			for _, want := range tc.want {
				if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
					t.Errorf("want %s at the dest root: %v", want, err)
				}
			}
			for _, nw := range tc.notWant {
				if _, err := os.Stat(filepath.Join(dest, nw)); err == nil {
					t.Errorf("unexpected nested %s at the dest", nw)
				}
			}
			if _, err := os.Stat(filepath.Join(dest, "stale.txt")); err == nil {
				t.Errorf("clean wipe did not remove the stale file")
			}
			// The safety copy holds the pre-restore state.
			if _, err := os.Stat(filepath.Join(dir, "safety", "pre-restore-0.tar.gz")); err != nil {
				t.Errorf("safety copy missing: %v", err)
			}
		})
	}
}

// ---- 缺陷 2：恢复停着的应用 ----

// fakeRestoreAgent stubs the agent round-trip for restore tests: container
// state comes from an in-memory flag, and restore/cleanup scripts run for
// real through the local sh so unpack placement is genuinely exercised. It
// fails loudly on any command it does not recognise, so a new agent call in
// the restore path cannot slip past the test unnoticed.
type fakeRestoreAgent struct {
	running  atomic.Bool
	sawStart atomic.Bool
	sawStop  atomic.Bool
}

func (f *fakeRestoreAgent) exec(_ *rpc.Hub, _ string, command string, _ int32) (*agentpb.ExecResult, error) {
	switch {
	case strings.HasPrefix(command, "sha256sum "):
		// volumeSHA256Script: sha256sum '<path>' | awk '{print $1}'
		path := strings.TrimSpace(strings.TrimPrefix(command, "sha256sum "))
		path = strings.TrimSuffix(path, " | awk '{print $1}'")
		path = strings.Trim(path, "'")
		raw, err := os.ReadFile(path)
		if err != nil {
			return &agentpb.ExecResult{ExitCode: 1, Stderr: []byte(err.Error())}, nil
		}
		sum := sha256.Sum256(raw)
		return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte(hex.EncodeToString(sum[:]) + "\n")}, nil
	case strings.Contains(command, "docker inspect --format"):
		if f.running.Load() {
			return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte("RUNNING\n")}, nil
		}
		return &agentpb.ExecResult{ExitCode: 0, Stdout: []byte("STOPPED\n")}, nil
	case strings.HasPrefix(command, "docker stop "):
		f.sawStop.Store(true)
		f.running.Store(false)
		return &agentpb.ExecResult{ExitCode: 0}, nil
	case strings.HasPrefix(command, "docker start "):
		f.sawStart.Store(true)
		f.running.Store(true)
		return &agentpb.ExecResult{ExitCode: 0}, nil
	case command == "true":
		return &agentpb.ExecResult{ExitCode: 0}, nil
	case strings.HasPrefix(command, "rm -f "):
		// volumeRemoveScript (staging cleanup): run for real; missing
		// files are a no-op.
		return runLocalScript(command)
	case strings.Contains(command, "IS_SNAPSHOT"):
		// The multi-restore script: run for real so placement, the safety
		// copy and the clean wipe are genuinely exercised.
		return runLocalScript(command)
	default:
		return nil, fmt.Errorf("fakeRestoreAgent: unexpected command %q", command)
	}
}

func runLocalScript(command string) (*agentpb.ExecResult, error) {
	cmd := exec.Command("sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}
	return &agentpb.ExecResult{ExitCode: int32(code), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

// newRestoreTestEnv wires an engine whose registry has a fake online agent
// ("h1") plus the stubbed execOnAgent above, so restore paths run end to end
// without a real host. The safety copy still lands under
// /var/backups/watchman/apps/<appID> (a const), so the test skips when that
// scratch space is not writable and removes its own app dir afterwards.
func newRestoreTestEnv(t *testing.T, appID string) (*Engine, *Store, *VolumeStore, *fakeRestoreAgent) {
	t.Helper()
	if err := os.MkdirAll(volumeStagingDir(appID), 0o755); err != nil {
		t.Skipf("volume staging scratch space not writable: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(volumeStagingDir(appID)) })
	store, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	vols, err := NewVolumeStore(store.Dir(), nil)
	if err != nil {
		t.Fatalf("NewVolumeStore: %v", err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	registerMigrateTestAgent(t, reg, "h1", "linux")
	e := NewEngine(reg, store, nil, slog.Default())
	e.SetVolumeStore(vols)
	fake := &fakeRestoreAgent{}
	prev := execOnAgent
	execOnAgent = fake.exec
	t.Cleanup(func() { execOnAgent = prev })
	return e, store, vols, fake
}

// putRestoreTestApp seeds a stopped single-container app whose one volume is
// a bind mount from a temp dir onto /app/pb_data. appID is the app's full ID
// (the same string newRestoreTestEnv was given, so the safety-copy scratch
// dir it manages is the one the restore actually writes to).
func putRestoreTestApp(t *testing.T, store *Store, appID string) (*Application, string) {
	t.Helper()
	dataDir := t.TempDir()
	app := &Application{
		ID: appID, HostID: "h1", Name: "Restore" + appID,
		SourceType: "image", Image: "nginx:alpine",
		ContainerName: "certimate", Volumes: []string{dataDir + ":/app/pb_data"},
	}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}
	return app, dataDir
}

func writeRestoreTestTar(t *testing.T, entries map[string]string) (path string, sha string) {
	t.Helper()
	raw := writeTestTar(t, entries)
	path = filepath.Join(t.TempDir(), "vbk.tar")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return path, hex.EncodeToString(sum[:])
}

func TestRunRestoreUploadedBatchStartsStoppedApp(t *testing.T) {
	e, store, vols, fake := newRestoreTestEnv(t, "app_restore_batch")
	app, dataDir := putRestoreTestApp(t, store, "app_restore_batch")
	spec := app.Volumes[0]
	tarPath, sha := writeRestoreTestTar(t, map[string]string{
		"pb_data/data.db": "restored-db", "pb_data/sub/x.txt": "x",
	})
	arch := &VolumeArchive{
		ID: "vbk_batch1", AppID: app.ID, HostID: "h1",
		VolumeSpec: spec, HostPath: dataDir, ContainerPath: "/app/pb_data",
		StorageTarget: StorageTargetLocal, StoragePath: tarPath,
		SHA256: sha, Status: VolumeBackupUploaded, Source: VolumeArchiveSourceUpload,
		StripComponents: 1, CreatedAt: time.Now(),
	}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}

	// The app is stopped before the restore (the certimate shape): the
	// container state flag starts false and no stop is issued.
	restarted, err := e.runRestoreUploadedBatch(app,
		[]*uploadedRestoreItem{{Arch: arch, ExpectedSHA: sha, Specs: []string{spec}}},
		RestoreVolumeOptions{CleanRestore: true})
	if err != nil {
		t.Fatalf("runRestoreUploadedBatch: %v", err)
	}
	if !restarted {
		t.Errorf("restarted = false, want true: a stopped app must be started after a successful restore")
	}
	if !fake.sawStart.Load() {
		t.Errorf("docker start was never issued for the stopped app")
	}
	if fake.sawStop.Load() {
		t.Errorf("docker stop was issued for an already-stopped app")
	}
	if got, err := os.ReadFile(filepath.Join(dataDir, "data.db")); err != nil || string(got) != "restored-db" {
		t.Errorf("data.db at the dest root: content=%q err=%v (strip 1 must land it there)", got, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "pb_data", "data.db")); err == nil {
		t.Errorf("unexpected nested pb_data/data.db: the wrapper was not stripped")
	}
	rec, ok := vols.GetArchive(app.ID, arch.ID)
	if !ok {
		t.Fatalf("archive record missing after restore")
	}
	if rec.LastRestoreStatus != VolumeRestoreSuccess {
		t.Errorf("LastRestoreStatus = %q, want success (error: %q)", rec.LastRestoreStatus, rec.LastRestoreError)
	}
}

// The guard side of the same fix: when the unpack fails, the stopped app
// must stay stopped (its safety copy still holds the pre-restore data) and
// the record must say failed — not "success with the app still down".
func TestRunRestoreUploadedBatchLeavesStoppedAppOnUnpackFailure(t *testing.T) {
	e, store, vols, fake := newRestoreTestEnv(t, "app_restore_batchneg")
	app, dataDir := putRestoreTestApp(t, store, "app_restore_batchneg")
	spec := app.Volumes[0]
	if err := os.WriteFile(filepath.Join(dataDir, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(t.TempDir(), "vbk.tar")
	if err := os.WriteFile(tarPath, []byte("not a tar at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(tarPath)
	sum := sha256.Sum256(raw)
	arch := &VolumeArchive{
		ID: "vbk_batchneg1", AppID: app.ID, HostID: "h1",
		VolumeSpec: spec, HostPath: dataDir, ContainerPath: "/app/pb_data",
		StorageTarget: StorageTargetLocal, StoragePath: tarPath,
		SHA256: hex.EncodeToString(sum[:]), Status: VolumeBackupUploaded, Source: VolumeArchiveSourceUpload,
		StripComponents: 0, CreatedAt: time.Now(),
	}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}

	restarted, err := e.runRestoreUploadedBatch(app,
		[]*uploadedRestoreItem{{Arch: arch, ExpectedSHA: arch.SHA256, Specs: []string{spec}}},
		RestoreVolumeOptions{CleanRestore: true})
	if err == nil {
		t.Fatalf("runRestoreUploadedBatch with a corrupt tar: want an error, got nil")
	}
	if restarted {
		t.Errorf("restarted = true on a failed unpack, want false")
	}
	if fake.sawStart.Load() {
		t.Errorf("docker start was issued after a failed unpack: the stopped app must stay stopped")
	}
	rec, ok := vols.GetArchive(app.ID, arch.ID)
	if !ok {
		t.Fatalf("archive record missing after failed restore")
	}
	if rec.LastRestoreStatus != VolumeRestoreFailed {
		t.Errorf("LastRestoreStatus = %q, want failed", rec.LastRestoreStatus)
	}
	// The clean wipe ran before the failing extract, but the pre-restore
	// state must survive in the safety copy.
	if _, err := os.Stat(filepath.Join(volumeSafetyPath(app.ID, arch.ID), "pre-restore-0.tar.gz")); err != nil {
		t.Errorf("safety copy missing after failed restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "old.txt")); err == nil {
		t.Errorf("old.txt still in the dest: the clean wipe should have run before the failed extract")
	}
}

// The single-archive restore path shares the same restart rule: a stopped
// app is started after a successful unpack.
func TestRunRestoreCoreStartsStoppedApp(t *testing.T) {
	e, store, vols, fake := newRestoreTestEnv(t, "app_restore_core")
	app, dataDir := putRestoreTestApp(t, store, "app_restore_core")
	spec := app.Volumes[0]
	tarPath, sha := writeRestoreTestTar(t, map[string]string{
		"pb_data/data.db": "restored-db",
	})
	arch := &VolumeArchive{
		ID: "vbk_core1", AppID: app.ID, HostID: "h1",
		VolumeSpec: spec, HostPath: dataDir, ContainerPath: "/app/pb_data",
		StorageTarget: StorageTargetLocal, StoragePath: tarPath,
		SHA256: sha, Status: VolumeBackupSuccess,
		StripComponents: 1, CreatedAt: time.Now(),
	}
	if err := vols.AddArchive(arch); err != nil {
		t.Fatal(err)
	}

	restarted, err := e.runRestoreAppVolume(app, arch, RestoreVolumeOptions{CleanRestore: true})
	if err != nil {
		t.Fatalf("runRestoreAppVolume: %v", err)
	}
	if !restarted {
		t.Errorf("restarted = false, want true")
	}
	if !fake.sawStart.Load() {
		t.Errorf("docker start was never issued for the stopped app")
	}
	if got, err := os.ReadFile(filepath.Join(dataDir, "data.db")); err != nil || string(got) != "restored-db" {
		t.Errorf("data.db at the dest root: content=%q err=%v", got, err)
	}
	rec, ok := vols.GetArchive(app.ID, arch.ID)
	if !ok {
		t.Fatalf("archive record missing after restore")
	}
	if rec.LastRestoreStatus != VolumeRestoreSuccess {
		t.Errorf("LastRestoreStatus = %q, want success (error: %q)", rec.LastRestoreStatus, rec.LastRestoreError)
	}
}
