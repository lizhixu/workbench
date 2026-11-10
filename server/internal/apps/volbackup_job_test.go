package apps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
	"watchman/server/internal/snapshots"
)

// Exercise the real snapshot handlers and both engines together: a Compose
// application has no Application.Volumes even though its containers have data.
func TestComposeVolumeBackupJobLifecycle(t *testing.T) {
	for _, source := range []string{"raw_compose", "git"} {
		t.Run(source, func(t *testing.T) {
			e, store, vols := newVolumeTestEngine(t)
			reg := rpc.NewRegistry(t.TempDir(), nil)
			registerMigrateTestAgent(t, reg, "h1", "linux")
			e.reg = reg
			app := &Application{ID: "app1", Name: "Compose", HostID: "h1", SourceType: source, BuildType: "compose"}
			if err := store.PutApp(app); err != nil {
				t.Fatal(err)
			}

			const dataSpec = "watchman-app1_data:/data"
			const cacheSpec = "/srv/cache:/cache"
			discovery := "NAMED_VOL|watchman-app1_data|/var/lib/docker/volumes/watchman-app1_data/_data\n" +
				"CONTAINER_MOUNT|web|volume|watchman-app1_data|/var/lib/docker/volumes/watchman-app1_data/_data|/data|true\n" +
				"CONTAINER_MOUNT|worker|volume|watchman-app1_data|/var/lib/docker/volumes/watchman-app1_data/_data|/data|true\n" +
				"CONTAINER_MOUNT|web|bind||/srv/cache|/cache|true\n" +
				"CONTAINER_MOUNT|web|bind||/var/run/docker.sock|/var/run/docker.sock|true\n"
			backups, uploads := 0, 0
			prev := execOnAgent
			execOnAgent = func(_ *rpc.Hub, id, command string, _ int32) (*agentpb.ExecResult, error) {
				switch {
				case strings.HasPrefix(id, "voldisc-"):
					if !strings.Contains(command, composeProjectName(app.ID)) {
						t.Fatalf("discovery must be scoped to the app project: %s", command)
					}
					return &agentpb.ExecResult{Stdout: []byte(discovery)}, nil
				case strings.HasPrefix(id, "volbk-"):
					backups++
					return &agentpb.ExecResult{Stdout: []byte("WATCHMAN_SIZE=128\nWATCHMAN_SHA256=" + strings.Repeat("a", 64) + "\n")}, nil
				case strings.HasPrefix(id, "volup-"):
					uploads++
					return &agentpb.ExecResult{}, nil
				case strings.HasPrefix(command, "rm -f "):
					return &agentpb.ExecResult{}, nil
				default:
					return nil, fmt.Errorf("unexpected agent command %s: %s", id, command)
				}
			}
			t.Cleanup(func() { execOnAgent = prev })

			target, err := e.ResolveAppVolume(app.ID)
			if err != nil || target == nil {
				t.Fatalf("resolve Compose mounts: target=%+v err=%v", target, err)
			}
			if !reflect.DeepEqual(target.Volumes, []string{dataSpec, cacheSpec}) {
				t.Fatalf("want unique business mounts, got %v", target.Volumes)
			}

			jobStore, err := snapshots.NewStore(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			s3, _ := e.s3.GetDefaultS3Target()
			if err := jobStore.PutS3Target(s3); err != nil {
				t.Fatal(err)
			}
			jobEngine := snapshots.NewEngine(reg, jobStore, nil)
			jobEngine.SetAppVolumeHost(e)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			snapshots.NewHandlers(reg, jobStore, jobEngine).Register(router.Group(""), router.Group(""))
			request := func(method, url, body string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequest(method, url, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				return w
			}
			create := func(spec string) *snapshots.Job {
				t.Helper()
				body := fmt.Sprintf(`{"name":"Compose backup","kind":"app_volume","app_id":"app1","target":%q,"storage_target":"default","retention":7}`, spec)
				w := request(http.MethodPost, "/backups/jobs", body)
				if w.Code != http.StatusOK {
					t.Fatalf("create %s: status=%d body=%s", spec, w.Code, w.Body.String())
				}
				var resp struct {
					Data *snapshots.Job `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Data == nil {
					t.Fatalf("decode job: %v, body=%s", err, w.Body.String())
				}
				if resp.Data.HostID != app.HostID || resp.Data.AppID != app.ID {
					t.Fatalf("job binding = %+v", resp.Data)
				}
				return resp.Data
			}

			all := create("ALL")
			subset := create(dataSpec)
			w := request(http.MethodPut, "/backups/jobs/"+subset.ID, `{"name":"Renamed"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("partial update: status=%d body=%s", w.Code, w.Body.String())
			}
			for _, job := range []*snapshots.Job{all, subset} {
				if _, err := jobEngine.Run(job); err != nil {
					t.Fatalf("run %s: %v", job.Target, err)
				}
				got, ok := jobStore.GetJob(job.ID)
				if !ok || got.LastRun.IsZero() || got.LastError != "" {
					t.Fatalf("job outcome not persisted: %+v", got)
				}
			}
			if backups != 2 || uploads != 2 || len(vols.ListArchives(app.ID)) != 2 {
				t.Fatalf("want two completed backups/uploads: backups=%d uploads=%d archives=%d", backups, uploads, len(vols.ListArchives(app.ID)))
			}
			for _, job := range []*snapshots.Job{all, subset} {
				archives, total := jobEngine.AppVolumeArchives(job, 0, 20)
				if total != 1 || len(archives) != 1 || archives[0].Status != VolumeBackupSuccess {
					t.Fatalf("%s history must contain its own successful snapshot: total=%d archives=%+v", job.Target, total, archives)
				}
				arch, _ := vols.GetArchive(app.ID, archives[0].ID)
				wantItems := 1
				if job.Target == "ALL" {
					wantItems = 3 // data is shared by web and worker
				}
				if len(arch.Volumes) != wantItems || arch.SHA256 == "" || arch.Size != 128 {
					t.Fatalf("incorrect snapshot: %+v", arch)
				}
			}

			w = request(http.MethodPost, "/backups/jobs", `{"name":"Bad","kind":"app_volume","app_id":"app1","target":"/etc:/etc","storage_target":"default"}`)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "只能选择") {
				t.Fatalf("outside mount must be rejected: %d %s", w.Code, w.Body.String())
			}
			// A plan must revalidate its selection after a deployment removes a volume.
			discovery = "CONTAINER_MOUNT|web|bind||/srv/cache|/cache|true\n"
			if _, err := jobEngine.Run(subset); err == nil || !strings.Contains(err.Error(), dataSpec) {
				t.Fatalf("removed mount must fail by name: %v", err)
			}
			failed, _ := jobStore.GetJob(subset.ID)
			if failed.LastError == "" || backups != 2 || e.VolumeBusy(app.ID) {
				t.Fatalf("rejected run must record failure without backup or leaked slot: %+v", failed)
			}
		})
	}
}

func TestResolveAppVolumeSourcesAndDiscoveryErrors(t *testing.T) {
	e, store, _ := newVolumeTestEngine(t)
	for _, tc := range []struct {
		name string
		app  Application
		want []string
		err  string
	}{
		{name: "single container", app: Application{SourceType: "image", Volumes: []string{" /srv/data:/data ", "/srv/data:/data", ""}}, want: []string{"/srv/data:/data"}},
		{name: "Compose static fallback", app: Application{SourceType: "raw_compose", ComposeContent: "services:\n  web:\n    volumes:\n      - ./data:/data\n"}, want: []string{"./data:/data"}},
		{name: "empty Compose", app: Application{SourceType: "raw_compose", ComposeContent: "services: {}"}, err: "未配置任何挂载卷"},
		{name: "Git Compose offline", app: Application{SourceType: "git", BuildType: "compose"}, err: "Agent 不在线"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.app.ID, tc.app.HostID = "app1", "h1"
			if err := store.PutApp(&tc.app); err != nil {
				t.Fatal(err)
			}
			target, err := e.ResolveAppVolume(tc.app.ID)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want %q, got %v", tc.err, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(target.Volumes, tc.want) {
				t.Fatalf("target=%+v err=%v, want %v", target, err, tc.want)
			}
		})
	}

	reg := rpc.NewRegistry(t.TempDir(), nil)
	registerMigrateTestAgent(t, reg, "h1", "linux")
	e.reg = reg
	prev := execOnAgent
	execOnAgent = func(_ *rpc.Hub, _, _ string, _ int32) (*agentpb.ExecResult, error) {
		return nil, fmt.Errorf("discovery unavailable")
	}
	t.Cleanup(func() { execOnAgent = prev })
	if _, err := e.ResolveAppVolume("app1"); err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
		t.Fatalf("discovery errors must not become an empty-volume error: %v", err)
	}
}
