package apps

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
	"watchman/server/internal/vault"
)

type gitTokenProvider interface {
	GetToken() (string, bool)
}

// Engine executes build+rollout deployments on managed hosts through the
// existing agent gRPC channel (ExecRequest / DockerOp), one deployment per
// application at a time (Dokploy-style serial build queue).
type Engine struct {
	reg         *rpc.Registry
	store       *Store
	vault       *vault.Store
	gitProvider gitTokenProvider
	log         *slog.Logger

	mu      sync.Mutex
	running map[string]bool // app_id -> a deployment is executing
}

// SetGitProvider injects a token provider (e.g. GitHub connection) for auto-cloning.
func (e *Engine) SetGitProvider(gp gitTokenProvider) {
	e.gitProvider = gp
}

// NewEngine creates the deployment engine. A nil log falls back to the
// default logger (tests do this).
func NewEngine(reg *rpc.Registry, store *Store, vs *vault.Store, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{reg: reg, store: store, vault: vs, log: log, running: make(map[string]bool)}
}

// StartDeployment enqueues a deployment. It returns immediately; the caller
// polls the deployment status. Returns the created record or nil when a
// build is already running for this application.
func (e *Engine) StartDeployment(app *Application, trigger, startedBy string) (*Deployment, error) {
	e.mu.Lock()
	if e.running[app.ID] {
		e.mu.Unlock()
		return nil, fmt.Errorf("another deployment is already running for this app")
	}
	e.running[app.ID] = true
	e.mu.Unlock()

	dep := &Deployment{
		ID:        "dep_" + randomToken(8),
		AppID:     app.ID,
		Trigger:   trigger,
		Status:    DeployQueued,
		StartedBy: startedBy,
		StartedAt: time.Now(),
	}
	if err := e.store.AddDeployment(dep); err != nil {
		e.finishRunning(app.ID)
		return nil, err
	}

	go e.runWithPin(app, dep, "")
	return dep, nil
}

// StartRollback re-runs the pipeline pinned to a historical commit. The
// recorded commit's image is still referenced by the recorded deployment, so
// rollback rebuilds from the checked-out commit for full auditability.
func (e *Engine) StartRollback(app *Application, commit, startedBy string) (*Deployment, error) {
	e.mu.Lock()
	if e.running[app.ID] {
		e.mu.Unlock()
		return nil, fmt.Errorf("another deployment is already running for this app")
	}
	e.running[app.ID] = true
	e.mu.Unlock()

	dep := &Deployment{
		ID:        "dep_" + randomToken(8),
		AppID:     app.ID,
		Trigger:   "rollback",
		Status:    DeployQueued,
		StartedBy: startedBy,
		StartedAt: time.Now(),
	}
	if err := e.store.AddDeployment(dep); err != nil {
		e.finishRunning(app.ID)
		return nil, err
	}

	go e.runWithPin(app, dep, commit)
	return dep, nil
}

// IsRunning reports whether a deployment is executing for the app.
func (e *Engine) IsRunning(appID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[appID]
}

func (e *Engine) finishRunning(appID string) {
	e.mu.Lock()
	delete(e.running, appID)
	e.mu.Unlock()
}

// runWithPin executes the whole build+rollout pipeline; pin, when non-empty,
// checks out that exact commit after fetch (rollback path). Steps log into a
// bounded buffer that lands on the deployment record so the UI can replay it.
func (e *Engine) runWithPin(app *Application, dep *Deployment, pin string) {
	defer e.finishRunning(app.ID)

	if pin != "" {
		_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.CommitHash = pin })
	}

	logBuf := &logBuffer{}
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		e.fail(app, dep, logBuf, msg)
	}

	e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployBuilding })

	var hub *rpc.Hub
	if e.reg != nil {
		hub = e.reg.Hub(app.HostID)
	}
	if hub == nil {
		fail("目标主机 Agent 不在线")
		return
	}

	// Resolve the git auth credential from the vault, if any. The token is
	// embedded into a https://x-access-token@host URL for clone/fetch only;
	// it never lands in the persistent log (logBuf redacts below).
	authToken, authErr := e.gitToken(app)
	if authErr != nil {
		fail("获取 Git 凭据失败: %v", authErr)
		return
	}

	repoDir := fmt.Sprintf("/var/lib/watchman/apps/%s/repo", app.ID)
	imageTag := fmt.Sprintf("watchman-app-%s", app.ID)
	branch := app.Branch
	if branch == "" {
		branch = "main"
	}
	dockerfile := app.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	context := app.BuildContext
	if context == "" {
		context = "."
	}
	buildTimeout := app.BuildTimeout
	if buildTimeout <= 0 {
		buildTimeout = 900
	}

	// Branch for raw Docker Compose applications (online YAML editor):
	if app.SourceType == "raw_compose" {
		e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployDeploying })
		logBuf.WriteString("$ deploy raw docker compose stack\n")
		composePath := fmt.Sprintf("/var/lib/watchman/apps/%s/compose.yaml", app.ID)
		if err := writeFileOnAgent(hub, composePath, []byte(app.ComposeContent)); err != nil {
			fail("写入 Compose 配置文件失败: %v", err)
			return
		}
		composeCmd := fmt.Sprintf("docker compose -f %s -p watchman-%s up -d --remove-orphans",
			shellQuote(composePath), app.ID)
		logBuf.WriteString("$ " + composeCmd + "\n")
		res, err := execOnAgent(hub, "compose-up-"+randomToken(4), composeCmd, buildTimeout)
		if err != nil {
			fail("Docker Compose 启动失败: %v", err)
			return
		}
		logBuf.WriteResult(res, nil, res.GetStderr())
		if res.GetExitCode() != 0 {
			fail("Docker Compose 启动失败 (exit %d)", res.GetExitCode())
			return
		}
		e.finishDeploySuccess(app, dep, "compose", imageTag, app.ContainerName, logBuf)
		return
	}

	// Step 1: fetch or clone the linked repository, then check out the branch
	// head — or the pinned commit for a rollback.
	fetchURL := app.RepoURL
	if authToken != "" {
		fetchURL = injectToken(app.RepoURL, authToken)
	}
	checkout := "origin/" + branch
	branchLabel := branch
	if pin != "" {
		checkout = pin
		branchLabel = branch + " @ " + pin
	}
	fetchScript := fmt.Sprintf(`if [ -d %[1]s/.git ]; then
  git -C %[1]s remote set-url origin %[2]s 2>/dev/null
  git -C %[1]s fetch --force origin
else
  mkdir -p %[1]s
  git clone --branch %[3]s %[2]s %[1]s
fi
git -C %[1]s checkout --force %[4]s
git -C %[1]s reset --hard %[4]s
git -C %[1]s clean -fd
COMMIT=$(git -C %[1]s rev-parse --short HEAD)
MESSAGE=$(git -C %[1]s log -1 --pretty=%%s)
echo "WATCHMAN_COMMIT=$COMMIT"
echo "WATCHMAN_MESSAGE=$MESSAGE"`,
		shellQuote(repoDir), shellQuote(fetchURL), shellQuote(branch), shellQuote(checkout))
	logBuf.WriteString("$ git fetch/checkout " + app.RepoURL + " (" + branchLabel + ")\n")
	res, err := execOnAgent(hub, "git-env-"+randomToken(4), fetchScript, 300)
	if err != nil {
		fail("代码拉取失败: %v", err)
		return
	}
	logBuf.WriteResult(res, redact(res.GetStdout(), authToken), res.GetStderr())
	if res.GetExitCode() != 0 {
		fail("代码拉取失败 (exit %d)", res.GetExitCode())
		return
	}
	commit, message := parseCommitMeta(res.GetStdout())
	if commit != "" {
		dep.CommitHash = commit
		dep.CommitMessage = message
		e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
			d.CommitHash = commit
			d.CommitMessage = message
		})
	}
	imageRef := imageTag
	if commit != "" {
		imageRef = fmt.Sprintf("%s:%s", imageTag, commit)
	}

	// Step 2 & 3: build and launch.
	if app.BuildType == "compose" {
		e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployDeploying })
		composeFile := app.Dockerfile
		if composeFile == "" || composeFile == "Dockerfile" {
			composeFile = "compose.yaml"
		}
		composeCmd := fmt.Sprintf("docker compose -f %s -p watchman-%s up -d --build --remove-orphans",
			shellQuote(composeFile), app.ID)
		logBuf.WriteString("$ " + composeCmd + "\n")
		res, err = execOnAgent(hub, "compose-up-"+randomToken(4),
			fmt.Sprintf("cd %s && %s", shellQuote(repoDir), composeCmd), buildTimeout)
		if err != nil {
			fail("Docker Compose 部署失败: %v", err)
			return
		}
		logBuf.WriteResult(res, nil, res.GetStderr())
		if res.GetExitCode() != 0 {
			fail("Docker Compose 部署失败 (exit %d)", res.GetExitCode())
			return
		}
		e.finishDeploySuccess(app, dep, commit, imageRef, app.ContainerName, logBuf)
		return
	}

	// Standard Dockerfile build:
	buildCmd := fmt.Sprintf("docker build -t %s -f %s %s", imageRef, dockerfile, context)
	logBuf.WriteString("$ " + buildCmd + "\n")
	res, err = execOnAgent(hub, "docker-build-"+randomToken(4),
		fmt.Sprintf("cd %s && %s", shellQuote(repoDir), buildCmd), buildTimeout)
	if err != nil {
		fail("Docker 构建失败: %v", err)
		return
	}
	logBuf.WriteResult(res, nil, res.GetStderr())
	if res.GetExitCode() != 0 {
		fail("Docker 构建失败 (exit %d)", res.GetExitCode())
		return
	}

	e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployDeploying })

	// Step 3: start the replacement container under a -next name, health
	// probe, then swap. The old container keeps running the whole time so a
	// failure leaves the previous version untouched (auto-rollback-by-design).
	container := app.ContainerName
	if container == "" {
		container = "watchman-app-" + app.ID
	}
	nextName := container + "-next"

	envArgs := e.resolveEnvArgs(app, logBuf)
	var runArgs []string
	for _, pm := range app.Ports {
		runArgs = append(runArgs, "-p", fmt.Sprintf("%d:%d", pm.Host, pm.Container))
	}
	for _, v := range app.Volumes {
		runArgs = append(runArgs, "-v", v)
	}
	runArgs = append(runArgs, "--label", fmt.Sprintf("watchman.app=%s", app.ID))
	runCmd := fmt.Sprintf("docker rm -f %s 2>/dev/null; docker run -d --name %s --restart unless-stopped %s %s %s",
		container, nextName, strings.Join(envArgs, " "), strings.Join(runArgs, " "), imageRef)
	logBuf.WriteString("$ start replacement container\n")
	res, err = execOnAgent(hub, "docker-run-"+randomToken(4), runCmd, 120)
	if err != nil {
		fail("启动新容器失败: %v", err)
		return
	}
	logBuf.WriteResult(res, nil, res.GetStderr())
	if res.GetExitCode() != 0 {
		fail("启动新容器失败 (exit %d)", res.GetExitCode())
		e.removeContainer(hub, nextName)
		return
	}

	// Step 4: health probe (container state; HTTP probe if configured).
	ok := e.probe(hub, app, nextName, logBuf)
	if !ok {
		fail("健康检查未通过，已保留旧版本容器")
		e.removeContainer(hub, nextName)
		return
	}

	// Step 5: swap. Old container is renamed aside (not deleted) so an
	// operator can still inspect it; it is dropped on the next successful
	// deploy of the same app.
	swapCmd := fmt.Sprintf(`docker rm -f %[1]s-prev 2>/dev/null
if docker inspect %[1]s >/dev/null 2>&1; then
  docker rename %[1]s %[1]s-prev && docker stop %[1]s-prev >/dev/null
fi
docker rename %[2]s %[1]s
docker inspect --format '{{.State.Status}}' %[1]s`, container, nextName)
	logBuf.WriteString("$ swap containers\n")
	res, err = execOnAgent(hub, "docker-swap-"+randomToken(4), swapCmd, 60)
	if err != nil {
		fail("容器切换失败: %v", err)
		return
	}
	logBuf.WriteResult(res, nil, res.GetStderr())
	if res.GetExitCode() != 0 {
		fail("容器切换失败 (exit %d)", res.GetExitCode())
		return
	}

	// Step 6: prune old image tags (keep the newest few for rollback).
	pruneCmd := fmt.Sprintf(`docker images %[1]s --format '{{.Tag}}' | grep -v latest | head -n 6 | tail -n +6 | while read t; do docker rmi %[1]s:$$t 2>/dev/null; done`, imageTag)
	if _, err := execOnAgent(hub, "docker-prune-"+randomToken(4), pruneCmd, 60); err != nil {
		e.log.Warn("image prune failed", "err", err, "app", app.ID)
	}

		e.finishDeploySuccess(app, dep, commit, imageTag, container, logBuf)
	}

	func (e *Engine) finishDeploySuccess(app *Application, dep *Deployment, commit, imageRef, container string, logBuf *logBuffer) {
		now := time.Now()
		finalLog := logBuf.String()
		e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
			d.Status = DeploySuccess
			d.FinishedAt = now
			d.DurationMS = now.Sub(d.StartedAt).Milliseconds()
			d.BuildLog = finalLog
		})
		e.store.UpdateApp(app.ID, func(a *Application) error {
			a.CurrentCommit = commit
			if imageRef != "" {
				a.Image = imageRef
			}
			if container != "" {
				a.ContainerName = container
			}
			a.LastDeployAt = now
			return nil
		})
		e.log.Info("deployment succeeded", "app", app.ID, "commit", commit, "deployment", dep.ID)
	}

func (e *Engine) fail(app *Application, dep *Deployment, logBuf *logBuffer, msg string) {
	now := time.Now()
	finalLog := logBuf.String() + "\n[FAILED] " + msg + "\n"
	_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		d.Status = DeployFailed
		d.FinishedAt = now
		d.DurationMS = now.Sub(d.StartedAt).Milliseconds()
		d.Error = msg
		d.BuildLog = finalLog
	})
	e.log.Warn("deployment failed", "app", app.ID, "deployment", dep.ID, "err", msg)
}

// probe waits for the replacement container to become healthy. With a
// configured healthcheck URL it polls until the endpoint answers 2xx; it
// falls back to `docker inspect` state when no URL is set.
func (e *Engine) probe(hub *rpc.Hub, app *Application, container string, logBuf *logBuffer) bool {
	if app.HealthcheckURL != "" {
		logBuf.WriteString("$ health probe " + app.HealthcheckURL + "\n")
		for i := 0; i < 12; i++ {
			time.Sleep(5 * time.Second)
			res, err := execOnAgent(hub, "probe-"+randomToken(4),
				fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --max-time 5 %s", shellQuote(app.HealthcheckURL)), 15)
			if err != nil {
				logBuf.WriteString("probe transport error: " + err.Error() + "\n")
				continue
			}
			code := strings.TrimSpace(string(res.GetStdout()))
			logBuf.WriteString("probe http status: " + code + "\n")
			if strings.HasPrefix(code, "2") {
				return true
			}
		}
		return false
	}
	// No HTTP probe configured: wait a grace period, then require the
	// container to still be running.
	time.Sleep(5 * time.Second)
	res, err := execOnAgent(hub, "probe-"+randomToken(4),
		fmt.Sprintf("docker inspect --format '{{.State.Status}}' %s", container), 15)
	if err != nil {
		logBuf.WriteString("probe transport error: " + err.Error() + "\n")
		return false
	}
	state := strings.TrimSpace(string(res.GetStdout()))
	logBuf.WriteString("container state: " + state + "\n")
	return state == "running"
}

// resolveEnvArgs renders -e flags, resolving {{ vault:<id>:<key> }} refs.
func (e *Engine) resolveEnvArgs(app *Application, logBuf *logBuffer) []string {
	if len(app.EnvVars) == 0 {
		return nil
	}
	keys := make([]string, 0, len(app.EnvVars))
	for k := range app.EnvVars {
		keys = append(keys, k)
	}
	// sort for a stable command string
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	args := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		v := resolveVaultRef(e.vault, app.EnvVars[k])
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	return args
}

func (e *Engine) gitToken(app *Application) (string, error) {
	if app.AuthVaultID != "" && e.vault != nil {
		cred, ok := e.vault.Get(app.AuthVaultID)
		if !ok {
			return "", fmt.Errorf("vault credential %q not found", app.AuthVaultID)
		}
		return cred.Secret, nil
	}
	// If no explicit Vault credential is configured, but this is a GitHub repository
	// and the GitHub Provider is connected, automatically use the GitHub account token.
	if e.gitProvider != nil && strings.Contains(strings.ToLower(app.RepoURL), "github.com") {
		if tok, ok := e.gitProvider.GetToken(); ok && tok != "" {
			return tok, nil
		}
	}
	return "", nil
}

func (e *Engine) removeContainer(hub *rpc.Hub, name string) {
	opID := "app-rm-" + randomToken(4)
	done := make(chan struct{}, 1)
	hub.SetRespHandler(opID, func(*agentpb.AgentMessage) {
		hub.SetRespHandler(opID, nil)
		done <- struct{}{}
	})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     opID,
				Command:    "docker rm -f " + name,
				TimeoutSec: 30,
			},
		},
	})
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		hub.SetRespHandler(opID, nil)
	}
}

// ---- helpers ----------------------------------------------------------

// execOnAgent runs one command on the agent and waits for the result.
func execOnAgent(hub *rpc.Hub, execID string, command string, timeoutSec int32) (*agentpb.ExecResult, error) {
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
		hub.SetRespHandler(execID, nil)
		if msg != nil {
			resultCh <- msg.GetExecResult()
		} else {
			resultCh <- nil
		}
	})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:     execID,
				Command:    command,
				IsScript:   true,
				TimeoutSec: timeoutSec,
			},
		},
	})
	wait := time.Duration(timeoutSec)*time.Second + 30*time.Second
	select {
	case res := <-resultCh:
		if res == nil {
			return nil, fmt.Errorf("agent disconnected")
		}
		return res, nil
	case <-time.After(wait):
		hub.SetRespHandler(execID, nil)
		return nil, fmt.Errorf("agent exec timeout")
	}
}

// logBuffer accumulates build output with a hard bound so one chatty build
// cannot bloat the JSONL history.
type logBuffer struct {
	b strings.Builder
}

const maxBuildLogBytes = 64 * 1024

func (l *logBuffer) WriteString(s string) {
	l.b.WriteString(s)
}

// WriteResult appends a step's stdout/stderr, redacted, bounded.
func (l *logBuffer) WriteResult(res *agentpb.ExecResult, stdoutRedacted, stderr []byte) {
	out := stdoutRedacted
	if out == nil {
		out = res.GetStdout()
	}
	if len(out) > 0 {
		l.appendBounded(out)
		l.b.WriteByte('\n')
	}
	if len(stderr) > 0 {
		l.b.WriteString("[stderr]\n")
		l.appendBounded(stderr)
		l.b.WriteByte('\n')
	}
}

func (l *logBuffer) appendBounded(b []byte) {
	if l.b.Len() >= maxBuildLogBytes {
		return
	}
	if l.b.Len()+len(b) > maxBuildLogBytes {
		l.b.WriteString("...[log truncated]...\n")
		return
	}
	l.b.Write(b)
}

func (l *logBuffer) String() string {
	return l.b.String()
}

// parseCommitMeta extracts the WATCHMAN_COMMIT/WATCHMAN_MESSAGE markers from
// the fetch step output.
func parseCommitMeta(stdout []byte) (commit, message string) {
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "WATCHMAN_COMMIT=") {
			commit = strings.TrimPrefix(line, "WATCHMAN_COMMIT=")
		} else if strings.HasPrefix(line, "WATCHMAN_MESSAGE=") {
			message = strings.TrimPrefix(line, "WATCHMAN_MESSAGE=")
		}
	}
	return commit, message
}

// injectToken embeds an access token into a https git URL for clone/fetch.
func injectToken(repoURL, token string) string {
	// https://host/path -> https://x-access-token:TOKEN@host/path
	if strings.HasPrefix(repoURL, "https://") {
		return "https://x-access-token:" + token + "@" + strings.TrimPrefix(repoURL, "https://")
	}
	if strings.HasPrefix(repoURL, "http://") {
		return "http://x-access-token:" + token + "@" + strings.TrimPrefix(repoURL, "http://")
	}
	return repoURL
}

// shellQuote single-quotes a value for the POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// redact strips the access token from text logged back to the server.
func redact(raw []byte, token string) []byte {
	if token == "" {
		return raw
	}
	return []byte(strings.ReplaceAll(string(raw), token, "$GIT_TOKEN"))
}

// resolveVaultRef replaces "{{ vault:<id>:<key> }}" placeholders. Unknown
// refs are left as-is so the failure is visible in the container env.
func resolveVaultRef(vs *vault.Store, v string) string {
	const prefix = "{{ vault:"
	if !strings.HasPrefix(v, prefix) || vs == nil {
		return v
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(v, prefix), "}}")
	parts := strings.SplitN(inner, ":", 2)
	if len(parts) != 2 {
		return v
	}
	cred, ok := vs.Get(strings.TrimSpace(parts[0]))
	if !ok {
		return v
	}
	if strings.TrimSpace(parts[1]) == "secret" {
		return cred.Secret
	}
	// Fall back to the whole secret for single-field credentials.
	return cred.Secret
}
