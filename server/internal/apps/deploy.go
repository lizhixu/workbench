package apps

import (
	"context"
	"encoding/base64"
	"errors"
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

// gitHubAppTokenProvider resolves a git-fetch credential from a GitHub App
// installation covering the repository. It sits between the app's explicit
// Vault credential and the legacy provider token in the deploy credential
// chain.
type gitHubAppTokenProvider interface {
	InstallationTokenForRepo(repoURL string) (string, bool)
}

// activeDeployment tracks an in-flight deployment so an operator can abort it
// (Engine.CancelDeployment): the cancel func tears down the run's context,
// nextName lets cleanup remove the half-built replacement container, and hub
// is the agent channel to send that removal over. All three are only valid
// while the run is registered in Engine.running.
type activeDeployment struct {
	depID    string
	cancel   context.CancelFunc
	hub      *rpc.Hub
	nextName string
}

// Engine executes build+rollout deployments on managed hosts through the
// existing agent gRPC channel (ExecRequest / DockerOp), one deployment per
// application at a time (Dokploy-style serial build queue).
type Engine struct {
	reg          *rpc.Registry
	store        *Store
	vault        *vault.Store
	networkStore *network.Store
	gitProvider  gitTokenProvider
	ghAppToken   gitHubAppTokenProvider
	// proxy re-points a bound domain at the new host when an app migrates
	// (migrate.go), and rolls it back on failure. nil until wired.
	proxy *ProxyHandler
	// vols stores app volume backup/restore archive metadata (volbackup.go);
	// nil until wired, in which case volume work reports a clear error.
	vols *VolumeStore
	// s3 resolves the S3 destination volume archives are uploaded to/downloaded
	// from (implemented by snapshots.Store).
	s3 S3TargetStore
	// snapJobs cleans up the snapshot backup jobs bound to an app that is being
	// purged (implemented by snapshots.Store).
	snapJobs SnapshotJobStore
	log      *slog.Logger

	mu      sync.Mutex
	running map[string]*activeDeployment // app_id -> in-flight deployment

	// volMu/volBusy serialize volume backup+restore per app: two concurrent
	// agent scripts over the same data dir would interleave tar with untar.
	volMu   sync.Mutex
	volBusy map[string]bool
}

// SetGitProvider injects a token provider (e.g. GitHub connection) for auto-cloning.
func (e *Engine) SetGitProvider(gp gitTokenProvider) {
	e.gitProvider = gp
}

// SetGitHubAppTokenProvider injects the GitHub App installation-token
// provider for auto-cloning App-installed repositories.
func (e *Engine) SetGitHubAppTokenProvider(p gitHubAppTokenProvider) {
	e.ghAppToken = p
}

// SetNetworkStore injects the overlay network store to resolve Tailscale mesh IPs.
func (e *Engine) SetNetworkStore(ns *network.Store) {
	e.networkStore = ns
}

// SetProxyHandler injects the reverse-proxy handler so a migration can move the
// vhost (and roll it back) together with the application.
func (e *Engine) SetProxyHandler(ph *ProxyHandler) {
	e.proxy = ph
}

// NewEngine creates the deployment engine. A nil log falls back to the
// default logger (tests do this).
func NewEngine(reg *rpc.Registry, store *Store, vs *vault.Store, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{reg: reg, store: store, vault: vs, log: log,
		running: make(map[string]*activeDeployment), volBusy: make(map[string]bool)}
}

// StartDeployment enqueues a deployment. It returns immediately; the caller
// polls the deployment status. Returns the created record or nil when a
// build is already running for this application.
func (e *Engine) StartDeployment(app *Application, trigger, startedBy string) (*Deployment, error) {
	dep := &Deployment{
		ID:        "dep_" + randomToken(8),
		AppID:     app.ID,
		Trigger:   trigger,
		Status:    DeployQueued,
		StartedBy: startedBy,
		StartedAt: time.Now(),
	}
	// A volume backup/restore owns the app's data dir; deploying would recreate
	// the container underneath it. Checked before registering and again after
	// (see registerRunning) to close the race with a volume op starting in
	// between.
	if e.VolumeBusy(app.ID) {
		return nil, errVolumeOpInProgress
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.registerRunning(app.ID, &activeDeployment{depID: dep.ID, cancel: cancel}); err != nil {
		cancel()
		return nil, err
	}
	if e.VolumeBusy(app.ID) {
		e.finishRunning(app.ID, dep.ID)
		cancel()
		return nil, errVolumeOpInProgress
	}
	if err := e.store.AddDeployment(dep); err != nil {
		e.finishRunning(app.ID, dep.ID)
		cancel()
		return nil, err
	}

	go e.runWithPin(ctx, app, dep, "")
	return dep, nil
}

// StartRollback re-runs the pipeline pinned to a historical commit. The
// recorded commit's image is still referenced by the recorded deployment, so
// rollback rebuilds from the checked-out commit for full auditability.
func (e *Engine) StartRollback(app *Application, commit, startedBy string) (*Deployment, error) {
	dep := &Deployment{
		ID:        "dep_" + randomToken(8),
		AppID:     app.ID,
		Trigger:   "rollback",
		Status:    DeployQueued,
		StartedBy: startedBy,
		StartedAt: time.Now(),
	}
	if e.VolumeBusy(app.ID) {
		return nil, errVolumeOpInProgress
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.registerRunning(app.ID, &activeDeployment{depID: dep.ID, cancel: cancel}); err != nil {
		cancel()
		return nil, err
	}
	if e.VolumeBusy(app.ID) {
		e.finishRunning(app.ID, dep.ID)
		cancel()
		return nil, errVolumeOpInProgress
	}
	if err := e.store.AddDeployment(dep); err != nil {
		e.finishRunning(app.ID, dep.ID)
		cancel()
		return nil, err
	}

	go e.runWithPin(ctx, app, dep, commit)
	return dep, nil
}

// IsRunning reports whether a deployment is executing for the app.
func (e *Engine) IsRunning(appID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[appID] != nil
}

// registerRunning claims the single-slot build queue for one app: returns
// ErrDeployInProgress when another run already holds it.
func (e *Engine) registerRunning(appID string, active *activeDeployment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.running[appID]; ok {
		return fmt.Errorf("another deployment is already running for this app")
	}
	e.running[appID] = active
	return nil
}

// activeHub records the agent channel of the in-flight run so CancelDeployment
// can remove the half-built container over the same connection.
func (e *Engine) activeHub(appID string, hub *rpc.Hub) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a, ok := e.running[appID]; ok && a != nil {
		a.hub = hub
	}
}

// activeNextName records the replacement container name once `docker run -d`
// has started it, so a cancel removes that container rather than the live one.
func (e *Engine) activeNextName(appID, nextName string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a, ok := e.running[appID]; ok && a != nil {
		a.nextName = nextName
	}
}

func (e *Engine) finishRunning(appID, depID string) {
	e.mu.Lock()
	if a, ok := e.running[appID]; ok && a != nil && (depID == "" || a.depID == depID) {
		delete(e.running, appID)
	}
	e.mu.Unlock()
}

// isTerminalDeploymentStatus reports whether a deployment record has reached
// a final state. Terminal records are append-only history: no later stage,
// cancel or success path may rewrite them, otherwise a cancel that landed
// between two stage boundaries gets overwritten by the next stage's status
// write and the UI reports the opposite of what the operator asked for.
func isTerminalDeploymentStatus(status string) bool {
	return status == DeploySuccess || status == DeployFailed || status == DeployCancelled
}

// updateDeploymentIfActive runs mutate only when the record has not reached
// a terminal state, and reports whether the mutation ran. Stage status
// writes must go through here (or setDeploymentStatus): a bare
// UpdateDeployment at a stage boundary resurrects a record that a cancel
// already finalized.
func (e *Engine) updateDeploymentIfActive(appID, depID string, mutate func(*Deployment)) bool {
	ran := false
	_ = e.store.UpdateDeployment(appID, depID, func(d *Deployment) {
		if !isTerminalDeploymentStatus(d.Status) {
			mutate(d)
			ran = true
		}
	})
	return ran
}

// setDeploymentStatus moves a record to a non-terminal status (building /
// deploying) only when it has not already been finalized.
func (e *Engine) setDeploymentStatus(appID, depID, status string) bool {
	return e.updateDeploymentIfActive(appID, depID, func(d *Deployment) { d.Status = status })
}

// nextContainerName scopes the replacement container to one deployment run:
// two runs of the same app must never share a -next name, otherwise a late
// cancel cleanup from the old run deletes the new run's container (the
// force-redeploy race: CancelDeployment frees the slot immediately while the
// old pipeline is still wedged in an agent exec).
func nextContainerName(container, depID string) string {
	return container + "-next-" + depID
}

// CancelDeployment aborts the in-flight deployment for an app. The run's
// context is cancelled, which makes every stage check in the pipeline bail
// out at its next step boundary.
// It immediately updates the deployment state in store to DeployCancelled,
// tags CancelledBy with operator, asynchronously cleans up the half-built -next
// container, and frees the engine running slot so a new deployment can run.
// Returns false if no deployment is actively running for this app.
func (e *Engine) CancelDeployment(appID, operator string) bool {
	e.mu.Lock()
	active, ok := e.running[appID]
	if !ok || active == nil {
		e.mu.Unlock()
		return false
	}
	active.cancel()
	depID := active.depID
	hub := active.hub
	nextName := active.nextName
	// Immediately free the running slot so force-redeploy can claim it
	delete(e.running, appID)
	e.mu.Unlock()

	now := time.Now()
	_ = e.store.UpdateDeployment(appID, depID, func(d *Deployment) {
		if d.Status == DeployBuilding || d.Status == DeployDeploying || d.Status == DeployQueued {
			d.Status = DeployCancelled
			d.FinishedAt = now
			d.DurationMS = now.Sub(d.StartedAt).Milliseconds()
			d.Error = "部署已被用户取消"
			if operator != "" {
				d.CancelledBy = operator
			} else {
				d.CancelledBy = "操作员"
			}
			d.BuildLog = d.BuildLog + "\n[CANCELLED] 部署已被取消\n"
		}
	})

	if hub != nil && nextName != "" {
		go e.removeContainer(hub, nextName)
	}

	e.log.Info("deployment cancelled", "app", appID, "deployment", depID, "by", operator)
	return true
}

// cleanupCancelled removes the half-built replacement container (the live one
// keeps serving) and finalizes the deployment record as cancelled. Called by
// the pipeline goroutine when it observes ctx.Done() and by the handler when
// the pipeline is wedged in an agent exec that has not yet returned.
// operator records who aborted the run (empty when the pipeline self-observes
// the cancel and the username is stamped by the cancel handler instead).
func (e *Engine) cleanupCancelled(app *Application, dep *Deployment, hub *rpc.Hub, nextName, operator string, logBuf *logBuffer) {
	if hub != nil && nextName != "" {
		e.removeContainer(hub, nextName)
	}
	now := time.Now()
	finalLog := logBuf.String() + "\n[CANCELLED] 部署已被用户取消\n"
	_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		// Terminal guard: a success or failure that already landed (e.g. the
		// swap completed just before the cancel arrived) must not be
		// rewritten as cancelled, and vice versa.
		if isTerminalDeploymentStatus(d.Status) {
			return
		}
		d.Status = DeployCancelled
		d.FinishedAt = now
		d.DurationMS = now.Sub(d.StartedAt).Milliseconds()
		d.Error = "部署已被用户取消"
		if operator != "" {
			d.CancelledBy = operator
		} else if d.CancelledBy == "" {
			d.CancelledBy = "操作员"
		}
		d.BuildLog = finalLog
	})
	e.log.Info("deployment cancelled", "app", app.ID, "deployment", dep.ID, "by", operator)
	e.finishRunning(app.ID, dep.ID)
}

// runWithPin executes the whole build+rollout pipeline; pin, when non-empty,
// checks out that exact commit after fetch (rollback path). Steps log into a
// bounded buffer that lands on the deployment record so the UI can replay it.
// ctx is cancelled by Engine.CancelDeployment: every stage checks it at its
// boundary and bails out, then finalizes the record as cancelled.
func (e *Engine) runWithPin(ctx context.Context, app *Application, dep *Deployment, pin string) {
	defer e.finishRunning(app.ID, dep.ID)

	if pin != "" {
		e.updateDeploymentIfActive(app.ID, dep.ID, func(d *Deployment) { d.CommitHash = pin })
	}

	logBuf := &logBuffer{}
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		e.fail(app, dep, logBuf, msg)
	}
	// cancelled: observed at a stage boundary. Finalizes the run as cancelled
	// (the -next container cleanup happens in deploySingleContainer, which
	// owns that name) and reports true. The operator username is stamped by
	// the cancel handler, not here.
	cancelled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		e.cleanupCancelled(app, dep, e.reg.Hub(app.HostID), "", "", logBuf)
		return true
	}

	// A cancel that landed before this goroutine started must not be
	// resurrected by the first status write: bail out when the record is
	// already terminal.
	if !e.setDeploymentStatus(app.ID, dep.ID, DeployBuilding) {
		return
	}

	var hub *rpc.Hub
	if e.reg != nil {
		hub = e.reg.Hub(app.HostID)
	}
	e.activeHub(app.ID, hub)
	if hub == nil {
		fail("目标主机 Agent 不在线")
		return
	}

	// Preflight: every deploy path ends in docker (build / run / compose),
	// so a host without Docker — or with a dead daemon — must fail fast
	// here with an actionable message. Without this the pipeline runs to
	// the container-start step and dies with a bare "docker: command not
	// found" (exit 127), which reads as a server bug and wastes the whole
	// fetch/build run before it.
	if cancelled() {
		return
	}
	logBuf.WriteString("$ check docker availability\n")
	if err := e.checkDockerReady(hub); err != nil {
		fail("%v", err)
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
		if !e.updateDeploymentIfActive(app.ID, dep.ID, func(d *Deployment) {
			d.Status = DeployDeploying
			// Snapshot the compose content so a later rollback can restore
			// exactly this version (the live app.ComposeContent keeps moving).
			d.ComposeContent = app.ComposeContent
		}) {
			return
		}
		logBuf.WriteString("$ deploy raw docker compose stack\n")
		appDir := fmt.Sprintf("/var/lib/watchman/apps/%s", app.ID)
		composePath := fmt.Sprintf("%s/compose.yaml", appDir)

		// Safely write compose.yaml via base64 decoding on agent.
		b64Content := base64.StdEncoding.EncodeToString([]byte(app.ComposeContent))
		writeScript := fmt.Sprintf(`mkdir -p %s && echo %s | base64 -d > %s`,
			shellQuote(appDir), shellQuote(b64Content), shellQuote(composePath))
		if cancelled() {
			return
		}
		if res, err := execOnAgent(hub, "write-compose-"+randomToken(4), writeScript, 30); err != nil || res.GetExitCode() != 0 {
			fail("写入 Compose 配置文件失败: %v", err)
			return
		}

		composeCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --remove-orphans",
			shellQuote(composePath), shellQuote("watchman-"+app.ID))
		logBuf.WriteString("$ " + composeCmd + "\n")
		if cancelled() {
			return
		}
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
		// The app may have been deleted while `up` was running: do not
		// leave an orphan stack behind a deleted record.
		if _, ok := e.store.GetApp(app.ID); !ok {
			downRes, downErr := execOnAgent(hub, "compose-down-"+randomToken(4),
				fmt.Sprintf("docker compose -f %s -p %s down", shellQuote(composePath), shellQuote("watchman-"+app.ID)), 60)
			if downErr != nil || downRes.GetExitCode() != 0 {
				logBuf.WriteString(fmt.Sprintf("WARN: 应用已删除但清理 Compose 栈失败: %v\n", firstErr(downErr, downRes)))
				fail("应用在部署期间已被删除，部署已中止；清理 Compose 栈失败，栈可能仍在运行，请手动执行 docker compose down 清理")
			} else {
				fail("应用在部署期间已被删除，部署已中止")
			}
			return
		}
		if ctx.Err() != nil {
			// `compose up -d` cannot be interrupted mid-exec, so a cancel
			// that landed during it leaves a live stack: tear it down
			// before finalizing as cancelled, and say so loudly when the
			// teardown itself fails instead of reporting a clean cancel.
			downOK := e.cancelComposeStack(hub,
				fmt.Sprintf("docker compose -f %s -p %s down", shellQuote(composePath), shellQuote("watchman-"+app.ID)), logBuf)
			e.cleanupCancelled(app, dep, hub, "", "", logBuf)
			if !downOK {
				_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
					d.Error = "部署已取消，但 Compose 栈清理失败，栈仍在运行，请手动执行 docker compose down 清理"
				})
			}
			return
		}
		e.finishDeploySuccess(app, dep, "compose", imageTag, app.ContainerName, logBuf)
		return
	}

		// Branch for single pre-built image deployment (app store / quick run):
		if app.SourceType == "image" {
			if !e.setDeploymentStatus(app.ID, dep.ID, DeployDeploying) {
				return
			}
			logBuf.WriteString("$ deploy pre-built image\n")
			e.deploySingleContainer(ctx, hub, app, dep, logBuf, app.Image)
			return
		}

		// Branch for built-in template deployment:
	if app.SourceType == "template" {
		if !e.setDeploymentStatus(app.ID, dep.ID, DeployDeploying) {
			return
		}
		logBuf.WriteString("$ deploy built-in template\n")
		if err := ResolveTemplate(app); err != nil {
			fail("解析应用模板失败: %v", err)
			return
		}
		e.deploySingleContainer(ctx, hub, app, dep, logBuf, app.Image)
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
	if cancelled() {
		return
	}
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
	if cancelled() {
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
		if !e.setDeploymentStatus(app.ID, dep.ID, DeployDeploying) {
			return
		}
		composeFile := app.Dockerfile
		if composeFile == "" || composeFile == "Dockerfile" {
			composeFile = "compose.yaml"
		}
		composeCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --build --remove-orphans",
			shellQuote(composeFile), shellQuote("watchman-"+app.ID))
		logBuf.WriteString("$ " + composeCmd + "\n")
		if cancelled() {
			return
		}
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
		if _, ok := e.store.GetApp(app.ID); !ok {
			downRes, downErr := execOnAgent(hub, "compose-down-"+randomToken(4),
				fmt.Sprintf("cd %s && docker compose -f %s -p %s down", shellQuote(repoDir), shellQuote(composeFile), shellQuote("watchman-"+app.ID)), 60)
			if downErr != nil || downRes.GetExitCode() != 0 {
				logBuf.WriteString(fmt.Sprintf("WARN: 应用已删除但清理 Compose 栈失败: %v\n", firstErr(downErr, downRes)))
				fail("应用在部署期间已被删除，部署已中止；清理 Compose 栈失败，栈可能仍在运行，请手动执行 docker compose down 清理")
			} else {
				fail("应用在部署期间已被删除，部署已中止")
			}
			return
		}
		if ctx.Err() != nil {
			// Same live-stack problem as the raw_compose branch: `up -d`
			// already went live, so tear it down before finalizing.
			downOK := e.cancelComposeStack(hub,
				fmt.Sprintf("cd %s && docker compose -f %s -p %s down", shellQuote(repoDir), shellQuote(composeFile), shellQuote("watchman-"+app.ID)), logBuf)
			e.cleanupCancelled(app, dep, hub, "", "", logBuf)
			if !downOK {
				_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
					d.Error = "部署已取消，但 Compose 栈清理失败，栈仍在运行，请手动执行 docker compose down 清理"
				})
			}
			return
		}
		e.finishDeploySuccess(app, dep, commit, imageRef, app.ContainerName, logBuf)
		return
	}

	// Standard Dockerfile build:
	buildCmd := fmt.Sprintf("docker build -t %s -f %s %s",
		shellQuote(imageRef), shellQuote(dockerfile), shellQuote(context))
	logBuf.WriteString("$ " + buildCmd + "\n")
	if cancelled() {
		return
	}
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

	if !e.setDeploymentStatus(app.ID, dep.ID, DeployDeploying) {
		return
	}

	// Step 3: start the replacement container under a -next name, health
	// probe, then swap. The old container keeps running the whole time so a
	// failure leaves the previous version untouched (auto-rollback-by-design).
	e.deploySingleContainer(ctx, hub, app, dep, logBuf, imageRef)
}

// deploySingleContainer handles the generic run-probe-swap pipeline for both
// pre-built images (source_type=image/template) and built images (source_type=git).
// ctx is the run's cancellation context: it is checked before every step so an
// operator-triggered stop bails out instead of pushing a half-built image live.
func (e *Engine) deploySingleContainer(ctx context.Context, hub *rpc.Hub, app *Application, dep *Deployment, logBuf *logBuffer, imageRef string) {
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		e.fail(app, dep, logBuf, msg)
	}

	container := app.ContainerName
	if container == "" {
		container = "watchman-app-" + app.ID
	}
	nextName := nextContainerName(container, dep.ID)

	cancelled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		e.cleanupCancelled(app, dep, hub, nextName, "", logBuf)
		return true
	}

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
	if cancelled() {
		return
	}
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
	// The -next container now exists: record it so a later cancel can remove
	// exactly this container, never the live one.
	e.activeNextName(app.ID, nextName)

	// Step 4: health probe (container state; HTTP probe if configured).
	if cancelled() {
		return
	}
	ok := e.probe(ctx, hub, app, nextName, logBuf)
	if !ok {
		// A cancel that landed during the probe must finalize as cancelled
		// (keeping CancelledBy), not as a health-check failure overwriting
		// the cancelled record.
		if cancelled() {
			return
		}
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
	if cancelled() {
		return
	}
	// The app may have been deleted while the replacement was built: never
	// swap a container into place for a record that no longer exists.
	if _, ok := e.store.GetApp(app.ID); !ok {
		e.removeContainer(hub, nextName)
		fail("应用在部署期间已被删除，部署已中止")
		return
	}
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

	// A cancel that landed after the swap must finalize as cancelled, not
	// be rewritten as success by the finish step below. (The swapped
	// container is intentionally left alone: cleanupCancelled only removes
	// the -next name, which no longer exists after the rename.)
	if cancelled() {
		return
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
	finalized := false
	_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		if isTerminalDeploymentStatus(d.Status) {
			// A cancel (or a delete racing the run) already finalized this
			// record; never rewrite it as success behind the operator.
			finalized = true
			return
		}
		d.Status = DeploySuccess
		d.FinishedAt = now
		d.DurationMS = now.Sub(d.StartedAt).Milliseconds()
		d.BuildLog = finalLog
	})
	if finalized {
		return
	}
	if _, ok := e.store.GetApp(app.ID); !ok {
		// Deleted mid-run (a delete that raced the start): the record must
		// not claim success for an app that no longer exists, and no
		// forwarder may be created for it below. This intentionally
		// demotes the success this same call just wrote: the terminal
		// guard above protects against competing paths, but this branch
		// is the correction of our own write, not a competing one.
		_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
			d.Status = DeployFailed
			d.Error = "应用在部署期间已被删除"
			d.BuildLog = finalLog + "\n[FAILED] 应用在部署期间已被删除\n"
		})
		return
	}
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
		// The cached app pointer predates the update above, and a purge may have
		// run while this deploy was finishing: re-read before any relay action so
		// a deleted app never recreates its forwarder behind purge's back.
		fresh, ok := e.store.GetApp(app.ID)
		if !ok {
			logBuf.WriteString("\nmesh-forward: 应用在部署期间已被删除，跳过中继创建\n")
			e.log.Info("deployment succeeded but app was removed; skipping mesh forwarder", "app", app.ID)
			return
		}
		if fresh.MeshForwardEnabled && fresh.MeshForwardHostID != "" && fresh.MeshForwardEntryPort > 0 {
			// A failed relay must not fail the deploy itself (the
			// container is live), but the WARN is already in the log.
			_ = e.syncMeshForwarder(fresh, logBuf)
		} else if fresh.MeshForwardHostID != "" {
			if err := e.cleanMeshForwarder(fresh.MeshForwardHostID, fresh.ID); err != nil {
				logBuf.WriteString(fmt.Sprintf("WARN: 清理组网中继容器失败: %v\n", err))
			}
		}
		e.log.Info("deployment succeeded", "app", app.ID, "commit", commit, "deployment", dep.ID)
}

func (e *Engine) fail(app *Application, dep *Deployment, logBuf *logBuffer, msg string) {
	now := time.Now()
	finalLog := logBuf.String() + "\n[FAILED] " + msg + "\n"
	_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		// Terminal guard: never overwrite a cancelled record with a
		// failure (probe-cancel and late stage errors both funnel here),
		// and never overwrite an earlier terminal state at all.
		if isTerminalDeploymentStatus(d.Status) {
			return
		}
		d.Status = DeployFailed
		d.FinishedAt = now
		d.DurationMS = now.Sub(d.StartedAt).Milliseconds()
		d.Error = msg
		d.BuildLog = finalLog
	})
	e.log.Warn("deployment failed", "app", app.ID, "deployment", dep.ID, "err", msg)
}

// cancelComposeStack tears down a compose stack that went live while the
// operator cancelled. `compose up -d` cannot be interrupted mid-exec, so a
// cancel landing during it must bring the stack back down; otherwise the
// record would say "cancelled" while the new stack keeps serving. It
// returns false when the teardown itself failed — the caller must then say
// so on the record instead of reporting a clean cancel.
func (e *Engine) cancelComposeStack(hub *rpc.Hub, downCmd string, logBuf *logBuffer) bool {
	res, err := execOnAgent(hub, "compose-down-"+randomToken(4), downCmd, 120)
	if err != nil {
		logBuf.WriteString(fmt.Sprintf("WARN: 取消后清理 Compose 栈失败: %v\n", err))
		return false
	}
	if res.GetExitCode() != 0 {
		logBuf.WriteString(fmt.Sprintf("WARN: 取消后清理 Compose 栈失败 (exit %d): %s\n", res.GetExitCode(), string(res.GetStderr())))
		return false
	}
	logBuf.WriteString("已取消部署，Compose 栈已清理\n")
	return true
}

// probe waits for the replacement container to become healthy. With a
// configured healthcheck URL it polls until the endpoint answers 2xx; it
// falls back to `docker inspect` state when no URL is set. ctx aborts the
// wait early (operator cancelled the deployment).
func (e *Engine) probe(ctx context.Context, hub *rpc.Hub, app *Application, container string, logBuf *logBuffer) bool {
	if app.HealthcheckURL != "" {
		logBuf.WriteString("$ health probe " + app.HealthcheckURL + "\n")
		for i := 0; i < 12; i++ {
			if ctx.Err() != nil {
				return false
			}
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
	if ctx.Err() != nil {
		return false
	}
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
	// If no explicit Vault credential is configured, but the repository is
	// covered by a GitHub App installation, use a fresh installation token
	// (short-lived, minted at deploy time, never persisted).
	if e.ghAppToken != nil {
		if tok, ok := e.ghAppToken.InstallationTokenForRepo(app.RepoURL); ok && tok != "" {
			return tok, nil
		}
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

// Docker preflight exit codes, mirroring proxyEnsureCheckScript's split:
// the user-facing fix differs between "install Docker" and "start the
// daemon", so the two failures must stay distinguishable.
const (
	deployDockerMissing    = 4
	deployDockerDaemonDown = 5
)

// dockerPreflightScript probes only what every deploy path needs: a docker
// binary and a responding daemon. It deliberately does not check the
// compose plugin or images — those fail later with their own step context.
func dockerPreflightScript() string {
	return fmt.Sprintf(`command -v docker >/dev/null 2>&1 || exit %d
docker info >/dev/null 2>&1 || exit %d`,
		deployDockerMissing, deployDockerDaemonDown)
}

// classifyDockerPreflight maps a preflight exit code to an actionable
// error. Exit 0 is the only success; anything else names the host-side
// fix (install vs start) instead of leaking a bare exit code.
func classifyDockerPreflight(exitCode int32) error {
	switch exitCode {
	case 0:
		return nil
	case deployDockerMissing:
		return errors.New("目标主机未安装 Docker：请先在主机详情 Docker 页一键安装 Docker 后重试")
	case deployDockerDaemonDown:
		return errors.New("目标主机 Docker 服务未运行：请先启动 Docker 服务后重试")
	default:
		return fmt.Errorf("目标主机 Docker 不可用 (exit %d)：请检查 Docker 安装与服务状态后重试", exitCode)
	}
}

// checkDockerReady runs the preflight on the target host. A transport
// failure (agent gone mid-check, exec timeout) is reported as-is — it is
// not a Docker problem and must not be misclassified as one.
func (e *Engine) checkDockerReady(hub *rpc.Hub) error {
	res, err := execOnAgent(hub, "docker-preflight-"+randomToken(4), dockerPreflightScript(), 30)
	if err != nil {
		return fmt.Errorf("Docker 可用性检查失败: %w", err)
	}
	return classifyDockerPreflight(res.GetExitCode())
}

// logBuffer accumulates build output with a hard bound so one chatty build
// cannot bloat the JSONL history.
type logBuffer struct {
	b strings.Builder
}

// forwarderImage is the OCI image for 组网转发 (source_type=forward) apps:
// an alpine/socat relay container listening on the entry host and forwarding
// to the target over the overlay network.
const forwarderImage = "alpine/socat"

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

// resolveMeshForwardTarget derives the upstream target (IP:port) on the app host
// for the entry host's socat forwarder container:
//   - 组网路径: if both app host and entry host have Tailscale Mesh IPs -> app host 100.x:port
//   - 直连路径: otherwise app host's public IP:port, then internal IP:port
func (e *Engine) resolveMeshForwardTarget(app *Application) (string, error) {
	port := app.MeshForwardTargetPort
	if port <= 0 {
		if len(app.Ports) > 0 && app.Ports[0].Host > 0 {
			port = app.Ports[0].Host
		} else {
			port = 8080
		}
	}

	appMesh, entryMesh := "", ""
	if e.networkStore != nil {
		if st, ok := e.networkStore.GetNodeStatus(app.HostID); ok {
			appMesh = st.IP
		}
		if app.MeshForwardHostID != "" {
			if st, ok := e.networkStore.GetNodeStatus(app.MeshForwardHostID); ok {
				entryMesh = st.IP
			}
		}
	}
	appPublicIP, appInternalIP := "", ""
	if e.reg != nil {
		if ag := e.reg.GetAgent(app.HostID); ag != nil {
			appPublicIP, appInternalIP = ag.PublicIP, ag.InternalIP
		}
	}
	return resolveForwardUpstream(port, appMesh, entryMesh, appPublicIP, appInternalIP)
}

// resolveForwardUpstream picks the entry host -> app host upstream:
//   - 组网: both ends carry a mesh IP -> the app host's 100.x:port;
//   - 直连: otherwise the app host's public IP, then its internal IP.
func resolveForwardUpstream(port int, appMesh, entryMesh, appPublicIP, appInternalIP string) (string, error) {
	if appMesh != "" && entryMesh != "" {
		return fmt.Sprintf("%s:%d", appMesh, port), nil
	}
	if appPublicIP != "" {
		return fmt.Sprintf("%s:%d", appPublicIP, port), nil
	}
	if appInternalIP != "" {
		return fmt.Sprintf("%s:%d", appInternalIP, port), nil
	}
	return "", fmt.Errorf("无法确定入口主机到应用主机的转发目标地址：应用主机未加入异地组网，且没有可用的公网/内网 IP")
}

// syncMeshForwarder deploys (or replaces) the companion socat relay on the
// entry host. It returns an error when forwarding is configured but the
// relay could not be brought up, so callers that must not report success
// (migration stage 3, update sync) can fail loudly; callers that only log
// (deploy finish) still get the WARN in logBuf. An unconfigured relay is a
// no-op, not an error.
func (e *Engine) syncMeshForwarder(app *Application, logBuf *logBuffer) error {
	if e.reg == nil || app.MeshForwardHostID == "" || app.MeshForwardEntryPort <= 0 {
		return nil
	}
	fwdHub := e.reg.Hub(app.MeshForwardHostID)
	if fwdHub == nil {
		msg := fmt.Sprintf("组网转发入口主机 (%s) 离线，未能启动中继容器", app.MeshForwardHostID)
		if logBuf != nil {
			logBuf.WriteString("WARN: " + msg + "\n")
		}
		return fmt.Errorf("%s", msg)
	}

	target, err := e.resolveMeshForwardTarget(app)
	if err != nil {
		if logBuf != nil {
			logBuf.WriteString(fmt.Sprintf("WARN: 解析组网转发目标失败: %v\n", err))
		}
		return err
	}

	fwdContainer := "watchman-fwd-" + app.ID
	runCmd := fmt.Sprintf(
		"docker rm -f %s 2>/dev/null; docker run -d --name %s --restart unless-stopped -p %d:%d --init --entrypoint /usr/bin/socat %s %s %s",
		shellQuote(fwdContainer),
		shellQuote(fwdContainer),
		app.MeshForwardEntryPort,
		app.MeshForwardEntryPort,
		shellQuote(forwarderImage),
		shellQuote(fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", app.MeshForwardEntryPort)),
		shellQuote("TCP:"+target),
	)

	if logBuf != nil {
		logBuf.WriteString(fmt.Sprintf("$ deploy mesh forwarder on entry host %s (%d -> %s)\n", app.MeshForwardHostID, app.MeshForwardEntryPort, target))
	}
	res, err := execOnAgent(fwdHub, "fwd-run-"+randomToken(4), runCmd, 60)
	if err != nil {
		if logBuf != nil {
			logBuf.WriteString(fmt.Sprintf("WARN: 部署组网中继容器通信失败: %v\n", err))
		}
		return err
	}
	if res.GetExitCode() != 0 {
		msg := fmt.Sprintf("部署组网中继容器失败 (exit %d): %s", res.GetExitCode(), string(res.GetStderr()))
		if logBuf != nil {
			logBuf.WriteString("WARN: " + msg + "\n")
		}
		return fmt.Errorf("%s", msg)
	}

	_ = e.store.UpdateApp(app.ID, func(a *Application) error {
		a.MeshForwardTarget = target
		return nil
	})
	if logBuf != nil {
		logBuf.WriteString(fmt.Sprintf("组网端口转发已就绪: 入口主机端口 %d -> %s\n", app.MeshForwardEntryPort, target))
	}
	return nil
}

// cleanMeshForwarder removes the companion relay container on the given entry
// host. It returns an error when the entry host is offline or the removal
// failed, so callers can refuse to drop the record's pointer to the relay
// (an unremoved relay with no record pointing at it is an orphan purge can
// never find). "No such container" is not a failure: the desired end state
// already holds.
func (e *Engine) cleanMeshForwarder(hostID, appID string) error {
	if e.reg == nil || hostID == "" || appID == "" {
		return nil
	}
	fwdHub := e.reg.Hub(hostID)
	if fwdHub == nil {
		return fmt.Errorf("组网转发入口主机 %s 离线", hostID)
	}
	fwdContainer := "watchman-fwd-" + appID
	res, err := execOnAgent(fwdHub, "clean-fwd-"+randomToken(4), "docker rm -f "+shellQuote(fwdContainer), 30)
	if err != nil {
		return err
	}
	if res.GetExitCode() != 0 {
		if strings.Contains(string(res.GetStderr()), "No such container") {
			return nil
		}
		return fmt.Errorf("删除中继容器失败 (exit %d): %s", res.GetExitCode(), string(res.GetStderr()))
	}
	return nil
}

// CleanMeshForwarder removes the companion relay container on the given entry
// host. Exported so updateApp can tear down the old relay when forwarding is
// disabled or moved to another entry host (the record no longer references it).
func (e *Engine) CleanMeshForwarder(hostID, appID string) error {
	return e.cleanMeshForwarder(hostID, appID)
}

// SyncMeshForwarder updates or clears the companion socat relay container on the entry host.
func (e *Engine) SyncMeshForwarder(app *Application) (string, error) {
	if !app.MeshForwardEnabled {
		if app.MeshForwardHostID != "" {
			if err := e.cleanMeshForwarder(app.MeshForwardHostID, app.ID); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	if app.MeshForwardHostID == "" || app.MeshForwardEntryPort <= 0 {
		return "", fmt.Errorf("组网转发配置不完整")
	}
	if e.reg == nil {
		return "", fmt.Errorf("RPC registry not initialized")
	}
	fwdHub := e.reg.Hub(app.MeshForwardHostID)
	if fwdHub == nil {
		return "", fmt.Errorf("组网转发入口主机离线")
	}
	target, err := e.resolveMeshForwardTarget(app)
	if err != nil {
		return "", err
	}
	fwdContainer := "watchman-fwd-" + app.ID
	runCmd := fmt.Sprintf(
		"docker rm -f %s 2>/dev/null; docker run -d --name %s --restart unless-stopped -p %d:%d --init --entrypoint /usr/bin/socat %s %s %s",
		shellQuote(fwdContainer),
		shellQuote(fwdContainer),
		app.MeshForwardEntryPort,
		app.MeshForwardEntryPort,
		shellQuote(forwarderImage),
		shellQuote(fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", app.MeshForwardEntryPort)),
		shellQuote("TCP:"+target),
	)
	res, err := execOnAgent(fwdHub, "fwd-run-"+randomToken(4), runCmd, 60)
	if err != nil {
		return "", fmt.Errorf("在中继主机执行部署失败: %w", err)
	}
	if res.GetExitCode() != 0 {
		return "", fmt.Errorf("启动中继容器失败 (exit %d): %s", res.GetExitCode(), string(res.GetStderr()))
	}
	_ = e.store.UpdateApp(app.ID, func(a *Application) error {
		a.MeshForwardTarget = target
		return nil
	})
	return target, nil
}
