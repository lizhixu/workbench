package apps

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/network"
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
	networkStore *network.Store
	gitProvider gitTokenProvider
	log         *slog.Logger

	mu      sync.Mutex
	running map[string]bool // app_id -> a deployment is executing
}

// SetGitProvider injects a token provider (e.g. GitHub connection) for auto-cloning.
func (e *Engine) SetGitProvider(gp gitTokenProvider) {
	e.gitProvider = gp
}

// SetNetworkStore injects the overlay network store to resolve Tailscale mesh IPs.
func (e *Engine) SetNetworkStore(ns *network.Store) {
	e.networkStore = ns
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
		e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
			d.Status = DeployDeploying
			// Snapshot the compose content so a later rollback can restore
			// exactly this version (the live app.ComposeContent keeps moving).
			d.ComposeContent = app.ComposeContent
		})
		logBuf.WriteString("$ deploy raw docker compose stack\n")
		appDir := fmt.Sprintf("/var/lib/watchman/apps/%s", app.ID)
		composePath := fmt.Sprintf("%s/compose.yaml", appDir)

		// Safely write compose.yaml via base64 decoding on agent.
		b64Content := base64.StdEncoding.EncodeToString([]byte(app.ComposeContent))
		writeScript := fmt.Sprintf(`mkdir -p %s && echo %s | base64 -d > %s`,
			shellQuote(appDir), shellQuote(b64Content), shellQuote(composePath))
		if res, err := execOnAgent(hub, "write-compose-"+randomToken(4), writeScript, 30); err != nil || res.GetExitCode() != 0 {
			fail("写入 Compose 配置文件失败: %v", err)
			return
		}

		composeCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --remove-orphans",
			shellQuote(composePath), shellQuote("watchman-"+app.ID))
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

	// Branch for single pre-built image deployment (app store / quick run):
	if app.SourceType == "image" {
		e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployDeploying })
		logBuf.WriteString("$ deploy pre-built image\n")
		e.deploySingleContainer(hub, app, dep, logBuf, app.Image)
		return
	}

	// Branch for built-in template deployment:
	if app.SourceType == "template" {
		e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployDeploying })
		logBuf.WriteString("$ deploy built-in template\n")
		if err := ResolveTemplate(app); err != nil {
			fail("解析应用模板失败: %v", err)
			return
		}
		e.deploySingleContainer(hub, app, dep, logBuf, app.Image)
		return
	}

	// Step 1: fetch or clone the linked repository, then check out the branch
	// head — or the pinned commit for a rollback.
	// The git credential (when configured) travels as an http.extraHeader on
	// these two commands only; it is never written into the on-disk remote
	// URL, so a token cannot linger in the agent's .git/config.
	authArgs := gitAuthArgs(authToken)
	checkout := "origin/" + branch
	branchLabel := branch
	if pin != "" {
		checkout = pin
		branchLabel = branch + " @ " + pin
	}
	// NOTE: set -e is load-bearing. Without it, a failed `fetch` would fall
	// through to `checkout --force` on the stale local copy and the pipeline
	// would silently build + deploy the previous commit as if it succeeded.
	fetchScript := fmt.Sprintf(`set -e
if [ -d %[1]s/.git ]; then
  git %[5]s -C %[1]s fetch --force origin
else
  mkdir -p %[1]s
  git %[5]s clone --branch %[3]s %[2]s %[1]s
fi
git -C %[1]s checkout --force %[4]s
git -C %[1]s reset --hard %[4]s
git -C %[1]s clean -fd
COMMIT=$(git -C %[1]s rev-parse --short HEAD)
MESSAGE=$(git -C %[1]s log -1 --pretty=%%s)
echo "WATCHMAN_COMMIT=$COMMIT"
echo "WATCHMAN_MESSAGE=$MESSAGE"`,
		shellQuote(repoDir), shellQuote(app.RepoURL), shellQuote(branch), shellQuote(checkout), authArgs)
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
		composeCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --build --remove-orphans",
			shellQuote(composeFile), shellQuote("watchman-"+app.ID))
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
	buildCmd := fmt.Sprintf("docker build -t %s -f %s %s",
		shellQuote(imageRef), shellQuote(dockerfile), shellQuote(context))
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
	e.deploySingleContainer(hub, app, dep, logBuf, imageRef)
}

// deploySingleContainer handles the generic run-probe-swap pipeline for both
// pre-built images (source_type=image/template) and built images (source_type=git).
func (e *Engine) deploySingleContainer(hub *rpc.Hub, app *Application, dep *Deployment, logBuf *logBuffer, imageRef string) {
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		e.fail(app, dep, logBuf, msg)
	}

	container := app.ContainerName
	if container == "" {
		container = "watchman-app-" + app.ID
	}
	nextName := container + "-next"

	envArgs := e.resolveEnvArgs(app, logBuf)
	var runArgs []string

	// Resolve mesh IP if any port requires internal network binding
	meshIP := ""
	needsMesh := false
	for _, pm := range app.Ports {
		if pm.BindScope == "mesh" {
			needsMesh = true
			break
		}
	}
	if needsMesh {
		meshIP = e.resolveMeshIP(hub, app.HostID, logBuf)
	}

	for _, pm := range app.Ports {
		if pm.BindScope == "mesh" && meshIP != "" {
			runArgs = append(runArgs, "-p", fmt.Sprintf("%s:%d:%d", meshIP, pm.Host, pm.Container))
		} else {
			runArgs = append(runArgs, "-p", fmt.Sprintf("%d:%d", pm.Host, pm.Container))
		}
	}
	for _, v := range app.Volumes {
		runArgs = append(runArgs, "-v", shellQuote(v))
	}
	runArgs = append(runArgs, "--label", shellQuote("watchman.app="+app.ID))
	// All interpolated values are shell-quoted: container names, env values
	// and image refs all originate from user input (spaces would otherwise
	// break `docker run`, metacharacters would inject commands — and the
	// audit log would not reflect what actually ran).
	runCmd := fmt.Sprintf("docker rm -f %s 2>/dev/null; docker run -d --name %s --restart unless-stopped %s %s %s",
		shellQuote(nextName), shellQuote(nextName), strings.Join(envArgs, " "), strings.Join(runArgs, " "), shellQuote(imageRef))
	logBuf.WriteString("$ start replacement container\n")
	res, err := execOnAgent(hub, "docker-run-"+randomToken(4), runCmd, 120)
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
	// set -e: a failed `docker rename` must abort the script — otherwise the
	// trailing `docker inspect` would still exit 0 against the OLD container
	// and the deploy would wrongly report success.
	swapCmd := fmt.Sprintf(`set -e
docker rm -f %[1]s 2>/dev/null || true
if docker inspect %[2]s >/dev/null 2>&1; then
  docker rename %[2]s %[1]s
  docker stop %[1]s >/dev/null 2>&1 || true
fi
docker rename %[3]s %[2]s
docker inspect --format '{{.State.Status}}' %[2]s`,
		shellQuote(container+"-prev"), shellQuote(container), shellQuote(nextName))
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

	// Step 6: prune old image tags built by Watchman (keep the newest 5 for
	// rollback). Only images we built (watchman-app-*) are touched — never
	// upstream/pulled images the operator may share with other workloads.
	// NOTE: the previous version listed `docker images <repo>:<tag>` (which
	// matches a single tag, so pruning never fired) and used
	// `head -n 6 | tail -n +6` (which selects exactly one line).
	if strings.HasPrefix(imageRef, "watchman-app-") {
		repo := imageRef
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		pruneCmd := fmt.Sprintf(`docker images %[1]s --format '{{.CreatedAt}}|{{.Tag}}' | sort -r | cut -d'|' -f2- | grep -v '^latest$' | tail -n +6 | while read t; do docker rmi %[1]s:$$t 2>/dev/null; done`, shellQuote(repo))
		if _, err := execOnAgent(hub, "docker-prune-"+randomToken(4), pruneCmd, 60); err != nil {
			e.log.Warn("image prune failed", "err", err, "app", app.ID)
		}
	}

	e.finishDeploySuccess(app, dep, dep.CommitHash, imageRef, container, logBuf)
}

// resolveMeshIP returns the host's current Tailscale IPv4 for mesh-scoped port
// bindings. It probes the node live first (one exec round-trip) because the
// cached NodeStatus.IP goes stale when the tailnet address changes outside
// Watchman (e.g. `tailscale up --reset` re-registers the node and Headscale
// reassigns its 100.x). The cache is only a fallback; an empty return means
// "fall back to public binding".
func (e *Engine) resolveMeshIP(hub *rpc.Hub, hostID string, logBuf *logBuffer) string {
	if e.networkStore == nil {
		logBuf.WriteString("WARN: 需要绑定异地组网，但 NetworkStore 未初始化，回退为公网绑定\n")
		return ""
	}
	// Live probe first: `tailscale ip -4` takes no arguments and is
	// shell-agnostic (sh/bash/powershell/cmd), so no quoting concerns.
	liveIP := ""
	if res, err := execOnAgent(hub, "mesh-ip-"+randomToken(4), "tailscale ip -4", 15); err == nil && res != nil && res.GetExitCode() == 0 {
		liveIP = parseIPv4(string(res.GetStdout()))
	}
	cached, ok := e.networkStore.GetNodeStatus(hostID)
	ip, warn := pickMeshIP(liveIP, cached, ok)
	if warn != "" {
		logBuf.WriteString("WARN: " + warn + "\n")
	}
	if liveIP != "" {
		// Refresh the cache so the gateway proxy and UI see the fresh value.
		cached.HostID = hostID
		cached.IP = liveIP
		_ = e.networkStore.SetNodeStatus(cached)
	}
	return ip
}

// pickMeshIP decides which Mesh IP to bind: the live probe wins; the cached
// NodeStatus.IP is only a fallback for when the probe fails (e.g. tailscaled
// mid-restart). It returns the chosen IP ("" = fall back to public binding)
// and a warning message ("" = none).
func pickMeshIP(liveIP string, cached network.NodeStatus, cacheOK bool) (string, string) {
	if liveIP != "" {
		return liveIP, ""
	}
	if cacheOK && cached.IP != "" {
		return cached.IP, "异地组网 IP 实时探测失败，使用缓存值 " + cached.IP + "；若部署失败请检查节点组网状态"
	}
	return "", "无法获取主机异地组网 IP，回退为公网绑定"
}

// parseIPv4 extracts the first valid IPv4 address from command output.
func parseIPv4(out string) string {
	for _, f := range strings.Fields(out) {
		if ip := net.ParseIP(f); ip != nil && ip.To4() != nil {
			return f
		}
	}
	return ""
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
		fmt.Sprintf("docker inspect --format '{{.State.Status}}' %s", shellQuote(container)), 15)
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
		// Quote the whole K=V pair: values routinely contain spaces
		// (e.g. JAVA_OPTS) which would otherwise split into extra argv
		// entries and break `docker run`.
		args = append(args, "-e", shellQuote(k+"="+v))
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
				Command:    "docker rm -f " + shellQuote(name),
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

// gitAuthArgs renders the `git -c http.extraHeader=...` fragment that carries
// the access token as a Basic auth header on clone/fetch. The semantics match
// the old token-in-URL approach (user "git", token as password — what GitHub
// expects), but the secret never lands in the agent's .git/config on disk.
func gitAuthArgs(token string) string {
	if token == "" {
		return ""
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("git:" + token))
	return "-c " + shellQuote("http.extraHeader=Authorization: Basic "+b64)
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
