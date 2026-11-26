package apps

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
	"watchman/server/internal/vault"
)

func TestBuildGitFetchScript(t *testing.T) {
	// Branch deploy: shallow clone, remote-URL drift cleanup, explicit target fetch.
	s := buildGitFetchScript("/var/lib/watchman/apps/a1/repo", "https://github.com/acme/mono.git", "release/v2", "", "")
	for _, want := range []string{
		"git  clone --depth 1 --branch 'release/v2' 'https://github.com/acme/mono.git' '/var/lib/watchman/apps/a1/repo'",
		"CUR_URL=$(git -C '/var/lib/watchman/apps/a1/repo' remote get-url origin",
		"fetch --depth 1 --force origin 'release/v2':refs/watchman/target",
		"checkout --force refs/watchman/target",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("branch fetch script missing %q:\n%s", want, s)
		}
	}

	// Rollback pin: single-commit shallow fetch with shallow-aware full-fetch fallback.
	pinScript := buildGitFetchScript("/tmp/repo", "https://github.com/acme/mono.git", "main", "deadbeef", "")
	for _, want := range []string{
		"cat-file -e 'deadbeef'^{commit}",
		"fetch --depth 1 origin 'deadbeef'",
		"if [ -f '/tmp/repo'/.git/shallow ]",
		"fetch --unshallow origin",
		"fetch --force origin",
		"checkout --force 'deadbeef'",
	} {
		if !strings.Contains(pinScript, want) {
			t.Errorf("pin fetch script missing %q:\n%s", want, pinScript)
		}
	}
}

func TestGitComposeLocation(t *testing.T) {
	cases := []struct {
		name        string
		ctx         string
		composeFile string
		wantDir     string
		wantFile    string
	}{
		{"root empty", "", "compose.yaml", "/repo", "compose.yaml"},
		{"root dot", ".", "docker-compose.yml", "/repo", "docker-compose.yml"},
		{"subdir relative compose", "apps/web", "compose.yaml", "/repo/apps/web", "compose.yaml"},
		{"subdir strip dup prefix", "apps/web", "apps/web/compose.yaml", "/repo/apps/web", "compose.yaml"},
		{"subdir clean trailing slash", "apps/api/", "apps/api/sub/compose.yml", "/repo/apps/api", "sub/compose.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &Application{BuildContext: tc.ctx, Dockerfile: tc.composeFile}
			gotDir, gotFile := gitComposeLocation("/repo", app)
			if gotDir != tc.wantDir || gotFile != tc.wantFile {
				t.Fatalf("got (%q, %q), want (%q, %q)", gotDir, gotFile, tc.wantDir, gotFile)
			}
		})
	}
}

func TestResolveRegistryHostAndFirstComposeImage(t *testing.T) {
	if got := resolveRegistryHost("nginx:alpine"); got != "" {
		t.Errorf("dockerhub short ref = %q, want empty", got)
	}
	if got := resolveRegistryHost("library/nginx:alpine"); got != "" {
		t.Errorf("dockerhub library ref = %q, want empty", got)
	}
	if got := resolveRegistryHost("ghcr.io/acme/web:v1"); got != "ghcr.io" {
		t.Errorf("ghcr.io ref = %q", got)
	}
	if got := resolveRegistryHost("localhost:5000/acme/web:v1"); got != "localhost:5000" {
		t.Errorf("localhost:5000 ref = %q", got)
	}
	compose := "services:\n  api:\n    image: 'registry.example.com/team/api:2.0'\n"
	if got := firstComposeImage(compose); got != "registry.example.com/team/api:2.0" {
		t.Errorf("firstComposeImage = %q", got)
	}
}

func TestExecRegistryCommandVaultAndRedaction(t *testing.T) {
	store, err := NewStore(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	vStore, err := vault.NewStore(t.TempDir(), "test-pass")
	if err != nil {
		t.Fatal(err)
	}
	secretVal := "ghp_super_secret_registry_token_999"
	cred, err := vStore.Create(&vault.Credential{
		Name: "GHCR", Type: "container_registry",
		Username: "octocat", Secret: secretVal,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := rpc.NewRegistry(t.TempDir(), nil)
	registerMigrateTestAgent(t, reg, "h1", "linux")
	e := NewEngine(reg, store, vStore, slog.Default())

	var seenCmd string
	prev := execOnAgent
	execOnAgent = func(_ context.Context, _ *rpc.Hub, _, command string, _, _ int32, _ *logBuffer) (*agentpb.ExecResult, error) {
		if strings.Contains(command, "docker pull") {
			seenCmd = command
			return &agentpb.ExecResult{
				ExitCode: 1,
				Stderr:   []byte("denied using token " + secretVal + " at ghcr.io"),
			}, nil
		}
		return &agentpb.ExecResult{ExitCode: 0}, nil
	}
	t.Cleanup(func() { execOnAgent = prev })

	app := &Application{
		ID: "app_priv_img", Name: "PrivImg", HostID: "h1",
		SourceType: "image", Image: "ghcr.io/acme/priv:v1",
		RegistryVaultID: cred.ID,
	}
	if err := store.PutApp(app); err != nil {
		t.Fatal(err)
	}

	dep, err := e.StartCIDeploy(app, "ci")
	if err != nil {
		t.Fatalf("StartCIDeploy: %v", err)
	}
	got := waitDeploymentTerminal(t, e, store, app.ID, dep.ID)
	if got.Status != DeployFailed {
		t.Fatalf("expected failed deploy on pull exit 1, got %q", got.Status)
	}
	for _, want := range []string{
		"TMP_DC=$(mktemp -d)",
		"export DOCKER_CONFIG=\"$TMP_DC\"\n",
		"login 'ghcr.io' -u 'octocat' --password-stdin",
		"docker pull 'ghcr.io/acme/priv:v1'",
	} {
		if !strings.Contains(seenCmd, want) {
			t.Errorf("command missing %q:\n%s", want, seenCmd)
		}
	}
	if strings.Contains(got.BuildLog, secretVal) {
		t.Errorf("build log leaked registry secret: %q", got.BuildLog)
	}
	if strings.Contains(got.Error, secretVal) {
		t.Errorf("error leaked registry secret: %q", got.Error)
	}

	// Compound commands (`||` in ensureImagePresent) must also receive DOCKER_CONFIG
	// via exported env var rather than a one-command prefix.
	var inspectPullCmd string
	execOnAgent = func(_ context.Context, _ *rpc.Hub, _, command string, _, _ int32, _ *logBuffer) (*agentpb.ExecResult, error) {
		if strings.Contains(command, "docker image inspect") && strings.Contains(command, "docker pull") {
			inspectPullCmd = command
		}
		return &agentpb.ExecResult{ExitCode: 0}, nil
	}
	e.ensureImagePresent(context.Background(), nil, app, "ghcr.io/acme/priv:v1", &logBuffer{}, func() bool { return false }, func(string, ...any) {})
	if !strings.Contains(inspectPullCmd, "export DOCKER_CONFIG=\"$TMP_DC\"\ndocker image inspect") {
		t.Errorf("ensureImagePresent must export DOCKER_CONFIG before compound command:\n%s", inspectPullCmd)
	}
}
