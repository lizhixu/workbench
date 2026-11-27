package apps

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/network"
	"watchman/server/internal/repoinspect"
	"watchman/server/internal/rpc"
	"watchman/server/internal/vault"
)

// gitHubAppTokenProvider resolves a git-fetch credential from a GitHub App
// installation covering the repository.
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
	ghAppToken   gitHubAppTokenProvider
	// proxy re-points a bound domain at the new host when an app migrates
	// (migrate.go), and rolls it back on failure. nil until wired.
	proxy *ProxyHandler
	// vols stores app volume backup/restore archive metadata (volbackup.go);
	// nil until wired, in which case volume work reports a clear error.
	vols *VolumeStore
	// volUploads stages operator-uploaded restore files on the panel plus
	// their one-time agent download tokens (volupload.go); nil until wired,
	// in which case upload-restores report a clear error.
	volUploads *VolumeUploadStore
	// s3 resolves the S3 destination volume archives are uploaded to/downloaded
	// from (implemented by snapshots.Store).
	s3 S3TargetStore
	// snapJobs cleans up the snapshot backup jobs bound to an app that is being
	// purged (implemented by snapshots.Store).
	snapJobs SnapshotJobStore
	log      *slog.Logger

	mu      sync.Mutex
	running map[string]*activeDeployment // app_id -> in-flight deployment

	// liveLogs holds the in-flight build output of running deployments
	// (deployment id -> *logBuffer). A deployment record only carries the
	// final BuildLog once the pipeline ends, so this is what makes the log
	// viewer usable during a build: GET /deployments/:depID overlays it.
	liveLogs sync.Map

	// volMu/volBusy serialize volume backup+restore per app: two concurrent
	// agent scripts over the same data dir would interleave tar with untar.
	volMu   sync.Mutex
	volBusy map[string]bool
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
	return e.enqueueDeployment(app, trigger, startedBy, "")
}

// StartRollback re-runs the pipeline pinned to a historical commit. The
// recorded commit's image is still referenced by the recorded deployment, so
// rollback rebuilds from the checked-out commit for full auditability.
func (e *Engine) StartRollback(app *Application, commit, startedBy string) (*Deployment, error) {
	return e.enqueueDeployment(app, "rollback", startedBy, commit)
}

// StartUpgrade enqueues a deployment that first pulls the latest images
// (see runWithPin) and then runs the normal rollout. It reuses the same
// single-slot queue and deployment record as a manual redeploy, so an
// upgrade in flight blocks a concurrent deploy/upgrade with the usual
// already-running error and the pull step is visible in the build log.
func (e *Engine) StartUpgrade(app *Application, startedBy string) (*Deployment, error) {
	return e.enqueueDeployment(app, TriggerUpgrade, startedBy, "")
}

// StartCIDeploy enqueues a CI webhook deployment (e.g. GitHub Actions pushing
// a new image) using the same pull-first pipeline with trigger="ci".
func (e *Engine) StartCIDeploy(app *Application, startedBy string) (*Deployment, error) {
	return e.enqueueDeployment(app, TriggerCI, startedBy, "")
}

// enqueueDeployment creates the deployment record, claims the single-slot
// build queue and starts the pipeline goroutine. StartDeployment,
// StartRollback and StartUpgrade share it so the volume-slot and
// already-running guards cannot drift between entry points.
func (e *Engine) enqueueDeployment(app *Application, trigger, startedBy, pin string) (*Deployment, error) {
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

	go e.runWithPin(ctx, app, dep, pin)
	return dep, nil
}

// IsRunning reports whether a deployment is executing for the app.
func (e *Engine) IsRunning(appID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[appID] != nil
}

// LiveLog returns the build output accumulated so far for a deployment that
// is still running. The second return value is false once the run finished
// (the final log then lives on the record itself).
func (e *Engine) LiveLog(depID string) (string, bool) {
	v, ok := e.liveLogs.Load(depID)
	if !ok {
		return "", false
	}
	buf, ok := v.(*logBuffer)
	if !ok || buf == nil {
		return "", false
	}
	return buf.String(), true
}

// trackLog registers the in-flight buffer for the API to read. The returned
// done func must be deferred by the pipeline so the map does not grow.
func (e *Engine) trackLog(depID string, buf *logBuffer) func() {
	e.liveLogs.Store(depID, buf)
	return func() { e.liveLogs.Delete(depID) }
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
		go removeContainerFn(e, hub, nextName)
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
		removeContainerFn(e, hub, nextName)
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
	defer e.trackLog(dep.ID, logBuf)()
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
	if err := e.checkDockerReady(ctx, hub); err != nil {
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
	buildContext := app.BuildContext
	if buildContext == "" {
		buildContext = "."
	}
	buildTimeout := app.BuildTimeout
	if buildTimeout <= 0 {
		buildTimeout = 1800
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
		if res, err := execOnAgent(ctx, hub, "write-compose-"+randomToken(4), writeScript, 30, deployStallSec, nil); err != nil || res.GetExitCode() != 0 {
			fail("写入 Compose 配置文件失败: %v", err)
			return
		}

		// Upgrade/CI step: refresh every image the stack references before
		// bringing it up, so a floating tag actually moves. A failed pull
		// fails the run before `up` touches any container.
		if dep.Trigger == TriggerUpgrade || dep.Trigger == TriggerCI {
			pullCmd := fmt.Sprintf("docker compose -f %s -p %s pull",
				shellQuote(composePath), shellQuote("watchman-"+app.ID))
			if !e.execRegistryCommand(ctx, hub, app, "", "compose-pull-"+randomToken(4), pullCmd, "docker compose pull", 1800, logBuf, fail, "拉取 Compose 镜像失败") {
				return
			}
		}

		composeCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --remove-orphans",
			shellQuote(composePath), shellQuote("watchman-"+app.ID))
		logBuf.WriteString("$ " + composeCmd + "\n")
		if cancelled() {
			return
		}
		res, err := execOnAgent(ctx, hub, "compose-up-"+randomToken(4), composeCmd, buildTimeout, deployStallSec, logBuf)
		if err != nil {
			if ctx.Err() == nil {
				fail("Docker Compose 启动失败: %v", err)
				return
			}
			// Cancelled mid-exec: execOnAgent already stopped the
			// command on the agent, but a half-started stack may
			// still be up. Fall through to the ctx.Err() teardown
			// below instead of failing the (already cancelled) record.
		} else if res.GetExitCode() != 0 {
			fail("Docker Compose 启动失败 (exit %d)", res.GetExitCode())
			return
		}
		// The app may have been deleted while `up` was running: do not
		// leave an orphan stack behind a deleted record.
		if _, ok := e.store.GetApp(app.ID); !ok {
			downRes, downErr := execOnAgent(context.Background(), hub, "compose-down-"+randomToken(4),
				fmt.Sprintf("docker compose -f %s -p %s down", shellQuote(composePath), shellQuote("watchman-"+app.ID)), 60, deployStallSec, nil)
			if downErr != nil || downRes.GetExitCode() != 0 {
				logBuf.WriteString(fmt.Sprintf("WARN: 应用已删除但清理 Compose 栈失败: %v\n", firstErr(downErr, downRes)))
				fail("应用在部署期间已被删除，部署已中止；清理 Compose 栈失败，栈可能仍在运行，请手动执行 docker compose down 清理")
			} else {
				fail("应用在部署期间已被删除，部署已中止")
			}
			return
		}
		if ctx.Err() != nil {
			// `compose up -d` is interrupted mid-exec when the operator
			// cancels (execOnAgent stops the command on the agent), but
			// a half-started stack may still be up: tear it down
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

	// Branch for single pre-built image deployment (app store / quick run / CI image):
	if app.SourceType == "image" {
		if !e.setDeploymentStatus(app.ID, dep.ID, DeployDeploying) {
			return
		}
		logBuf.WriteString("$ deploy pre-built image\n")
		if app.Image != "" {
			if dep.Trigger == TriggerUpgrade || dep.Trigger == TriggerCI {
				if !e.pullImage(ctx, hub, app, app.Image, logBuf, cancelled, fail) {
					return
				}
			} else if !e.ensureImagePresent(ctx, hub, app, app.Image, logBuf, cancelled, fail) {
				return
			}
		}
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
		if app.Image != "" {
			if dep.Trigger == TriggerUpgrade || dep.Trigger == TriggerCI {
				if !e.pullImage(ctx, hub, app, app.Image, logBuf, cancelled, fail) {
					return
				}
			} else if !e.ensureImagePresent(ctx, hub, app, app.Image, logBuf, cancelled, fail) {
				return
			}
		}
		e.deploySingleContainer(ctx, hub, app, dep, logBuf, app.Image)
		return
	}

	// Step 1: shallow-clone/fetch the linked repository, then check out the
	// branch head — or the pinned commit for a rollback.
	authArgs := gitAuthArgs(authToken)
	branchLabel := branch
	if pin != "" {
		branchLabel = branch + " @ " + pin
	}
	fetchScript := buildGitFetchScript(repoDir, app.RepoURL, branch, pin, authArgs)
	logBuf.WriteString("$ git fetch/checkout " + app.RepoURL + " (" + branchLabel + ")\n")
	if cancelled() {
		return
	}
	res, err := execOnAgent(ctx, hub, "git-env-"+randomToken(4), fetchScript, 1800, deployStallSec, nil)
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
		workDir, composeFile := gitComposeLocation(repoDir, app)
		composeCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --build --remove-orphans",
			shellQuote(composeFile), shellQuote("watchman-"+app.ID))
		logBuf.WriteString(fmt.Sprintf("$ (cd %s && %s)\n", workDir, composeCmd))
		if cancelled() {
			return
		}
		res, err = execOnAgent(ctx, hub, "compose-up-"+randomToken(4),
			fmt.Sprintf("cd %s && %s", shellQuote(workDir), composeCmd), buildTimeout, deployStallSec, logBuf)
		if err != nil {
			if ctx.Err() == nil {
				fail("Docker Compose 部署失败: %v", err)
				return
			}
		} else if res.GetExitCode() != 0 {
			fail("Docker Compose 部署失败 (exit %d)", res.GetExitCode())
			return
		}
		if _, ok := e.store.GetApp(app.ID); !ok {
			downRes, downErr := execOnAgent(context.Background(), hub, "compose-down-"+randomToken(4),
				fmt.Sprintf("cd %s && docker compose -f %s -p %s down", shellQuote(workDir), shellQuote(composeFile), shellQuote("watchman-"+app.ID)), 60, deployStallSec, nil)
			if downErr != nil || downRes.GetExitCode() != 0 {
				logBuf.WriteString(fmt.Sprintf("WARN: 应用已删除但清理 Compose 栈失败: %v\n", firstErr(downErr, downRes)))
				fail("应用在部署期间已被删除，部署已中止；清理 Compose 栈失败，栈可能仍在运行，请手动执行 docker compose down 清理")
			} else {
				fail("应用在部署期间已被删除，部署已中止")
			}
			return
		}
		if ctx.Err() != nil {
			downOK := e.cancelComposeStack(hub,
				fmt.Sprintf("cd %s && docker compose -f %s -p %s down", shellQuote(workDir), shellQuote(composeFile), shellQuote("watchman-"+app.ID)), logBuf)
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
		shellQuote(imageRef), shellQuote(dockerfile), shellQuote(buildContext))
	logBuf.WriteString("$ " + buildCmd + "\n")
	if cancelled() {
		return
	}
	res, err = execOnAgent(ctx, hub, "docker-build-"+randomToken(4),
		fmt.Sprintf("cd %s && %s", shellQuote(repoDir), buildCmd), buildTimeout, deployStallSec, logBuf)
	if err != nil {
		fail("Docker 构建失败: %v", err)
		return
	}
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

	// stoppedOld tracks whether the live container was stopped by the
	// stop-first step below. A cancel that lands after the stop must
	// restart it: without that the app is left down with only a
	// "cancelled" record to show for it. `docker start` on an
	// already-running container is a harmless no-op, so a double
	// restart (handler path + pipeline path) is safe.
	stoppedOld := false

	cancelled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		if stoppedOld {
			// Remove the replacement FIRST: it holds the host ports,
			// so `docker start` of the old container would fail with
			// "port is already allocated" while it is still there.
			// (The docker-run error branch already does it in this
			// order; the cancel path did it backwards and the restart
			// never landed for port-bound apps.)
			if hub != nil && nextName != "" {
				removeContainerFn(e, hub, nextName)
			}
			e.restartOldContainer(hub, container, logBuf)
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
		meshIP = e.resolveMeshIP(ctx, hub, app.HostID, logBuf)
	}

	// Preflight: clear stale -next containers left by earlier failed runs
	// and fail fast when a required host port is already held. Without
	// this a timeout orphan keeps the port and every retry dies with a
	// bare "port is already allocated" at the docker-run step.
	if cancelled() {
		return
	}
	stopOld, err := e.preflightSingleContainer(ctx, hub, app, container, meshIP, logBuf)
	if err != nil {
		fail("%v", err)
		return
	}

	// Stop-first: when the live container still holds a required host
	// port, the rolling replace cannot bind the same port while the old
	// one runs. Stop it first (brief downtime), then start the
	// replacement. The swap step below is unchanged: it renames the
	// (now stopped) live container to -prev and the replacement to the
	// live name. Any failure after the stop restarts the old container
	// (rollbackNote, and the cancelled() closure above) so the app is
	// never left down with a failed record to show for it.
	if stopOld {
		if cancelled() {
			return
		}
		logBuf.WriteString("旧容器仍在运行并占用端口，先停止旧容器再启动新容器（短暂停机）\n")
		if !e.stopOldContainer(ctx, hub, container, logBuf) {
			fail("停止旧容器失败，部署已中止（旧容器仍在运行，端口未释放）")
			return
		}
		stoppedOld = true
	}

	// rollbackNote restarts the live container after a stop-first deploy
	// failed past the stop, and returns the suffix to append to the
	// failure message. It must run before fail(): fail() snapshots the
	// log buffer into the record at call time, so rollback lines written
	// afterwards would never reach the UI.
	rollbackNote := func() string {
		if !stoppedOld {
			return ""
		}
		if e.restartOldContainer(hub, container, logBuf) {
			return "（旧容器已重新启动）"
		}
		return "（旧容器回滚启动失败，仍处于停止状态，请在 Docker 页手动启动）"
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
	res, err := execOnAgent(ctx, hub, "docker-run-"+randomToken(4), runCmd, 600, deployStallSec, logBuf)
	if err != nil {
		// The run can end without a container for three reasons: the
		// agent-side command failed, the server-side wait expired, or
		// the operator cancelled (execOnAgent already stopped the
		// command on the agent via ExecCancel). In the first two cases
		// a late-finishing `docker run` may still have created the
		// -next container; in the third the container may exist too
		// (the cancel landed after `docker run` started it). Remove
		// it best-effort so the next retry does not hit "port is
		// already allocated". The cleanup runs on a fresh context: it
		// must still execute when the pipeline context is cancelled.
		if _, cleanErr := execOnAgent(context.Background(), hub, "deploy-cleanup-"+randomToken(4), "docker rm -f "+shellQuote(nextName), 30, deployStallSec, nil); cleanErr != nil {
			logBuf.WriteString(fmt.Sprintf("WARN: 启动失败后清理替换容器失败: %v\n", cleanErr))
		}
		fail("启动新容器失败: %v%s", err, rollbackNote())
		return
	}
	if res.GetExitCode() != 0 {
		// Remove the replacement FIRST, same ordering as the cancel
		// path and the run-error branch: it holds the host ports, so
		// restarting the stopped old container would fail with "port
		// is already allocated" while it is still there. Synchronous:
		// removeContainer returns immediately when the agent stream
		// is dead, so this cannot stall the engine slot the way the
		// old async removal was meant to avoid.
		removeContainerFn(e, hub, nextName)
		note := rollbackNote()
		fail("启动新容器失败 (exit %d)%s", res.GetExitCode(), note)
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
		// Remove the replacement FIRST: it holds the host ports, so
		// restarting the stopped old container would fail with "port
		// is already allocated" while it is still there. Synchronous
		// for the same reason as the run-failure branch above.
		removeContainerFn(e, hub, nextName)
		note := rollbackNote()
		if note == "" {
			fail("健康检查未通过，已保留旧版本容器")
		} else {
			fail("健康检查未通过%s", note)
		}
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
		removeContainerFn(e, hub, nextName)
		fail("应用在部署期间已被删除，部署已中止")
		return
	}
	res, err = execOnAgent(ctx, hub, "docker-swap-"+randomToken(4), swapCmd, 60, deployStallSec, logBuf)
	if err != nil {
		if ctx.Err() != nil {
			// Cancelled mid-swap: the script may have renamed the
			// live container aside already. Put it back (and restart
			// it) before finalizing as cancelled, or the app is left
			// down with only a "cancelled" record to show for it.
			e.rollbackSwap(hub, container, nextName, logBuf)
			cancelled()
			return
		}
		fail("容器切换失败: %v%s", err, e.swapRollbackNote(hub, container, nextName, logBuf))
		return
	}
	if res.GetExitCode() != 0 {
		fail("容器切换失败 (exit %d)%s", res.GetExitCode(), e.swapRollbackNote(hub, container, nextName, logBuf))
		return
	}

	// The live name now belongs to the replacement: a cancel landing
	// after this point must not "restart" it as if it were the old one.
	// (With stop-first the old container was already stopped before the
	// run; the swap script still renames it to -prev, keeping it around
	// for inspection, and the next deploy drops it.)
	stoppedOld = false

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
		if _, err := execOnAgent(ctx, hub, "docker-prune-"+randomToken(4), pruneCmd, 60, deployStallSec, nil); err != nil {
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

// publishedPort is one host-side port binding reported by `docker ps`:
// ip is the bind address ("0.0.0.0"/"::" = wildcard) and port the host port.
type publishedPort struct {
	ip   string
	port int
}

// parseDockerPsPorts parses `docker ps -a --format '{{.Names}} {{.Ports}}'`
// output into container names and, per container, its published host ports.
// Unpublished ports ("33060/tcp") and stopped containers (empty Ports)
// contribute no bindings.
func parseDockerPsPorts(out string) (names []string, bindings map[string][]publishedPort) {
	bindings = map[string][]publishedPort{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, ports, _ := strings.Cut(line, " ")
		names = append(names, name)
		for _, tok := range strings.Split(strings.TrimSpace(ports), ",") {
			tok = strings.TrimSpace(tok)
			left, _, ok := strings.Cut(tok, "->")
			if !ok {
				continue
			}
			i := strings.LastIndex(left, ":")
			if i < 0 {
				continue
			}
			p, err := strconv.Atoi(left[i+1:])
			if err != nil {
				continue
			}
			bindings[name] = append(bindings[name], publishedPort{ip: left[:i], port: p})
		}
	}
	return names, bindings
}

// portBindingConflicts reports whether a new bind of newIP:newPort would be
// refused because held already occupies that host port. A wildcard bind on
// either side (0.0.0.0 / ::) covers every address; otherwise the IPs must
// be equal. Two different specific IPs may share one host port.
func portBindingConflicts(newIP string, newPort int, held publishedPort) bool {
	if held.port != newPort {
		return false
	}
	if newIP == "" {
		newIP = "0.0.0.0"
	}
	if newIP == "0.0.0.0" || newIP == "::" || held.ip == "0.0.0.0" || held.ip == "::" {
		return true
	}
	return held.ip == newIP
}

// preflightSingleContainer runs before a single-container deploy starts:
// it removes stale -next containers left by earlier failed runs of this
// app, fails fast when a required host port is already held by a foreign
// container, and reports (stopOld) when the port is held by this app's own
// live container — that case is not a failure: the caller stops the old
// container first (stop-first) and then starts the replacement. The sweep
// prefix is exact (`<container>-next-dep_`, see nextContainerName), so the
// live container and the -prev rollback are never touched. A preflight
// transport failure is fail-open with a WARN: docker-run stays the final
// arbiter, and a flaky agent must not mask the real error.
func (e *Engine) preflightSingleContainer(ctx context.Context, hub *rpc.Hub, app *Application, container, meshIP string, logBuf *logBuffer) (stopOld bool, err error) {
	res, err := execOnAgent(ctx, hub, "deploy-preflight-"+randomToken(4), "docker ps -a --format '{{.Names}} {{.Ports}}'", 30, deployStallSec, nil)
	if err != nil {
		logBuf.WriteString(fmt.Sprintf("WARN: 部署前预检失败（%v），继续部署\n", err))
		return false, nil
	}
	if res.GetExitCode() != 0 {
		logBuf.WriteString(fmt.Sprintf("WARN: 部署前预检失败 (exit %d)，继续部署\n", res.GetExitCode()))
		return false, nil
	}
	names, bindings := parseDockerPsPorts(string(res.GetStdout()))

	// 1. Sweep stale -next containers from earlier failed runs.
	sweepPrefix := container + "-next-dep_"
	for _, name := range names {
		if !strings.HasPrefix(name, sweepPrefix) {
			continue
		}
		rmRes, rmErr := execOnAgent(ctx, hub, "deploy-sweep-"+randomToken(4), "docker rm -f "+shellQuote(name), 30, deployStallSec, nil)
		if rmErr != nil || rmRes.GetExitCode() != 0 {
			logBuf.WriteString(fmt.Sprintf("WARN: 清理上次失败残留的容器 %s 失败: %v\n", name, firstErr(rmErr, rmRes)))
			continue
		}
		logBuf.WriteString(fmt.Sprintf("已清理上次失败残留的容器 %s\n", name))
		// The container is gone: it no longer holds any port.
		delete(bindings, name)
	}

	// 2. Fail fast on host ports already held by a running container.
	// A swept orphan is gone by now; anything still listed genuinely
	// holds the port.
	for _, pm := range app.Ports {
		if pm.Host <= 0 {
			continue
		}
		newIP := "0.0.0.0"
		if pm.BindScope == "mesh" && meshIP != "" {
			newIP = meshIP
		}
		for name, held := range bindings {
			for _, h := range held {
				if !portBindingConflicts(newIP, pm.Host, h) {
					continue
				}
				if strings.HasPrefix(name, sweepPrefix) {
					return false, fmt.Errorf("端口 %d 仍被上次失败残留的容器 %s 占用（清理失败），请在 Docker 页手动移除该容器后重试", pm.Host, name)
				}
				if name == container {
					// The live container holds the port: not a failure.
					// The caller stops it first, then starts the
					// replacement (stop-first). Keep scanning: a foreign
					// holder on another port must still fail fast.
					stopOld = true
					continue
				}
				return false, fmt.Errorf("端口 %d 已被容器 %s 占用（不是本应用的容器），请先移除该容器或更换端口后重试", pm.Host, name)
			}
		}
	}
	return stopOld, nil
}

// stopOldContainer stops the live container so the replacement can bind
// its host ports (stop-first). Returns false when the container is still
// running afterwards; a container that vanished in the meantime (operator
// removed it) counts as stopped — the port is free either way. The -t 30
// grace gives databases time for a clean shutdown instead of the 10s
// default SIGKILL.
func (e *Engine) stopOldContainer(ctx context.Context, hub *rpc.Hub, container string, logBuf *logBuffer) bool {
	res, err := execOnAgent(ctx, hub, "deploy-stop-old-"+randomToken(4), "docker stop -t 30 "+shellQuote(container), 60, deployStallSec, nil)
	if err == nil && res.GetExitCode() == 0 {
		return true
	}
	logBuf.WriteString(fmt.Sprintf("WARN: 停止旧容器失败: %v\n", firstErr(err, res)))
	// Distinguish "container is gone" (fine) from "still running" (the
	// port is still held, starting the replacement would collide).
	stateRes, stateErr := execOnAgent(ctx, hub, "deploy-stop-check-"+randomToken(4),
		fmt.Sprintf("docker inspect --format '{{.State.Status}}' %s", shellQuote(container)), 15, deployStallSec, nil)
	if stateErr != nil || stateRes.GetExitCode() != 0 {
		logBuf.WriteString("旧容器已不存在，继续部署\n")
		return true
	}
	if strings.TrimSpace(string(stateRes.GetStdout())) == "running" {
		return false
	}
	return true
}

// restartOldContainer restarts the live container after a stop-first deploy
// failed past the stop (or was cancelled there). Best effort: when the
// restart itself fails the app is left down, and the log must say so
// plainly instead of claiming a rollback happened.
//
// The restart runs on a fresh context, not the pipeline's: it is needed
// precisely when the pipeline context is already cancelled, and a
// cancelled context would make the exec return before sending anything.
func (e *Engine) restartOldContainer(hub *rpc.Hub, container string, logBuf *logBuffer) bool {
	res, err := execOnAgent(context.Background(), hub, "deploy-rollback-"+randomToken(4), "docker start "+shellQuote(container), 60, deployStallSec, nil)
	if err != nil || res.GetExitCode() != 0 {
		logBuf.WriteString(fmt.Sprintf("WARN: 回滚启动旧容器失败: %v（旧容器仍处于停止状态，请在 Docker 页手动启动 %s）\n", firstErr(err, res), container))
		return false
	}
	logBuf.WriteString("已回滚：旧容器重新启动\n")
	return true
}

// rollbackSwap restores the old container after a swap that failed (or
// was cancelled) mid-script. The swap renames the live container to -prev
// and stops it before renaming -next into place, so a failure between the
// two renames leaves no container under the live name and the app down.
// The replacement is removed FIRST: it holds the host ports, so starting
// the old container would fail with "port is already allocated" while it
// is still there. The restore script is then state-aware, so it is safe
// to call on any swap failure:
//   - live name exists but stopped (swap failed before the rename): start it.
//   - live name missing, -prev exists (cut between the renames): rename it
//     back and start it.
//   - live name exists and running (swap actually completed, only the
//     final inspect failed): no-op.
//
// It returns true when it restored a running container. Like
// restartOldContainer it runs on a fresh context: a swap cancelled
// mid-script needs the rollback most, and the pipeline context is dead
// by then.
func (e *Engine) rollbackSwap(hub *rpc.Hub, container, nextName string, logBuf *logBuffer) bool {
	if hub != nil && nextName != "" {
		removeContainerFn(e, hub, nextName)
	}
	script := fmt.Sprintf(`if docker inspect %[1]s >/dev/null 2>&1; then
  if [ "$(docker inspect --format '{{.State.Status}}' %[1]s)" != "running" ]; then
    docker start %[1]s >/dev/null && echo RESTORED_LIVE
  fi
elif docker inspect %[2]s >/dev/null 2>&1; then
  docker rename %[2]s %[1]s && docker start %[1]s >/dev/null && echo RESTORED_PREV
fi`,
		shellQuote(container), shellQuote(container+"-prev"))
	res, err := execOnAgent(context.Background(), hub, "deploy-swap-rollback-"+randomToken(4), script, 60, deployStallSec, nil)
	if err != nil || res.GetExitCode() != 0 {
		logBuf.WriteString(fmt.Sprintf("WARN: 切换失败后恢复旧容器失败: %v\n", firstErr(err, res)))
		return false
	}
	switch strings.TrimSpace(string(res.GetStdout())) {
	case "RESTORED_LIVE":
		logBuf.WriteString("已回滚：旧容器重新启动\n")
		return true
	case "RESTORED_PREV":
		logBuf.WriteString("已回滚：旧容器已从 -prev 恢复并重新启动\n")
		return true
	}
	return false
}

// swapRollbackNote runs rollbackSwap after a failed swap and returns the
// suffix to append to the failure message.
func (e *Engine) swapRollbackNote(hub *rpc.Hub, container, nextName string, logBuf *logBuffer) string {
	if e.rollbackSwap(hub, container, nextName, logBuf) {
		return "（旧容器已恢复并重新启动）"
	}
	return ""
}

// ensureImagePresent pulls imageRef only when the target host does not
// already have it. Uses app.RegistryVaultID when configured.
func (e *Engine) ensureImagePresent(ctx context.Context, hub *rpc.Hub, app *Application, imageRef string, logBuf *logBuffer, cancelled func() bool, fail func(string, ...any)) bool {
	if cancelled() {
		return false
	}
	script := fmt.Sprintf("docker image inspect %s >/dev/null 2>&1 || docker pull %s", shellQuote(imageRef), shellQuote(imageRef))
	return e.execRegistryCommand(ctx, hub, app, imageRef, "docker-pull-"+randomToken(4), script, script, 1800, logBuf, fail, "拉取镜像失败")
}

// resolveMeshIP returns the host's current Tailscale IPv4 for mesh-scoped port
// bindings. It probes the node live first (one exec round-trip) because the
// cached NodeStatus.IP goes stale when the tailnet address changes outside
// Watchman (e.g. `tailscale up --reset` re-registers the node and Headscale
// reassigns its 100.x). The cache is only a fallback; an empty return means
// "fall back to public binding".
func (e *Engine) resolveMeshIP(ctx context.Context, hub *rpc.Hub, hostID string, logBuf *logBuffer) string {
	if e.networkStore == nil {
		logBuf.WriteString("WARN: 需要绑定异地组网，但 NetworkStore 未初始化，回退为公网绑定\n")
		return ""
	}
	// Live probe first: `tailscale ip -4` takes no arguments and is
	// shell-agnostic (sh/bash/powershell/cmd), so no quoting concerns.
	liveIP := ""
	if res, err := execOnAgent(ctx, hub, "mesh-ip-"+randomToken(4), "tailscale ip -4", 15, deployStallSec, nil); err == nil && res != nil && res.GetExitCode() == 0 {
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
		if imageRef != "" {
			d.ImageRef = imageRef
		}
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
	// CancelDeployment removes the running entry the moment it marks the
	// record cancelled, so a missing entry means the operator already owns
	// the terminal state: a pipeline goroutine that is still unwinding
	// (e.g. an execOnAgent wait that just returned context.Canceled)
	// must not overwrite it with a failure.
	if !e.IsRunning(app.ID) {
		return
	}
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

// cancelComposeStack tears down a compose stack that went live (or
// half-started) while the operator cancelled. `compose up -d` is
// interrupted mid-exec on cancel, so a cancel landing during it must
// bring the stack back down; otherwise the record would say "cancelled"
// while the new stack keeps serving. It returns false when the teardown
// itself failed — the caller must then say so on the record instead of
// reporting a clean cancel.
//
// The teardown runs on a fresh context, not the pipeline's: it is needed
// precisely when the pipeline context is already cancelled.
func (e *Engine) cancelComposeStack(hub *rpc.Hub, downCmd string, logBuf *logBuffer) bool {
	res, err := execOnAgent(context.Background(), hub, "compose-down-"+randomToken(4), downCmd, 120, deployStallSec, nil)
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
			res, err := execOnAgent(ctx, hub, "probe-"+randomToken(4),
				fmt.Sprintf("curl -s -o /dev/null -w '%%{http_code}' --max-time 5 %s", shellQuote(app.HealthcheckURL)), 15, deployStallSec, nil)
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
	res, err := execOnAgent(ctx, hub, "probe-"+randomToken(4),
		fmt.Sprintf("docker inspect --format '{{.State.Status}}' %s", shellQuote(container)), 15, deployStallSec, nil)
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
	return "", nil
}

// removeContainerFn is the seam tests use to observe container removals;
// production always calls (*Engine).removeContainer. It exists because
// removeContainer talks to the agent through hub.Send directly, so the
// execOnAgent stub the tests use cannot see its commands.
var removeContainerFn = (*Engine).removeContainer

func (e *Engine) removeContainer(hub *rpc.Hub, name string) {
	opID := "app-rm-" + randomToken(4)
	done := make(chan struct{}, 1)
	hub.SetRespHandler(opID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			hub.SetRespHandler(opID, nil)
			done <- struct{}{}
			return
		}
		// Only the final ExecResult ends the wait: a streaming chunk
		// (ExecOutput) from `docker rm -f` stderr must not.
		if msg.GetExecResult() == nil {
			return
		}
		hub.SetRespHandler(opID, nil)
		done <- struct{}{}
	})
	// Send returning false means there is no live stream to the agent:
	// the removal can never be answered, so waiting the full 30s for a
	// result that cannot come would only stall the caller (the cancel
	// path calls this synchronously). The preflight sweep on the next
	// run is the backstop for a container left behind.
	if !hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:          opID,
				Command:         "docker rm -f " + shellQuote(name),
				TimeoutSec:      30,
				StallTimeoutSec: deployStallSec,
			},
		},
	}) {
		hub.SetRespHandler(opID, nil)
		return
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		hub.SetRespHandler(opID, nil)
	}
}

// ---- helpers ----------------------------------------------------------

// deployStallSec is the stall budget (seconds of silence) the deploy
// engine asks for on every exec: a step that stops talking for this long
// is killed by the agent. Long silent transfers (tar/curl in volume and
// migration steps) pass stall 0 instead — they are legitimately quiet for
// minutes at a time.
const deployStallSec = 120

// sendExecCancel asks the agent to stop a running exec (whole process
// group). Best-effort: the agent may already have finished, or be gone;
// either way the caller still fails the step with its own error.
func sendExecCancel(hub *rpc.Hub, execID string) {
	if hub == nil {
		return
	}
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_ExecCancel{
			ExecCancel: &agentpb.ExecCancel{ExecId: execID},
		},
	})
}

// execOnAgent runs one command on the agent and waits for the result. It is
// a variable (not a plain func) so tests can stub the agent round-trip;
// production code always uses the default implementation below.
//
// ctx is the caller's cancellation source (nil = no cancellation; the
// wall-clock timeout still applies). When ctx is cancelled or the wait
// times out, the agent is told to stop the exec with ExecCancel, so the
// command really stops instead of becoming an orphan on the host.
//
// stallSec is the agent-side stall budget (0 = disabled). log receives
// live output chunks as they arrive and the un-streamed tail when the
// result lands (nil = the caller does not want the output in the log;
// e.g. preflight probes, or steps whose output is redacted before
// logging). Only callers that pass log may rely on the step's output
// being in the log; callers that pass nil must log the result themselves
// (usually via logBuf.WriteResult, e.g. the redacted git fetch).
var execOnAgent = func(ctx context.Context, hub *rpc.Hub, execID string, command string, timeoutSec, stallSec int32, log *logBuffer) (*agentpb.ExecResult, error) {
	if log != nil {
		log.BeginStream()
	}
	resultCh := make(chan *agentpb.ExecResult, 1)
	hub.SetRespHandler(execID, func(msg *agentpb.AgentMessage) {
		if msg == nil {
			// The agent disconnected mid-exec: unblock the wait below.
			hub.SetRespHandler(execID, nil)
			resultCh <- nil
			return
		}
		if out := msg.GetExecOutput(); out != nil {
			// Streaming chunk: route it to the live log and keep
			// waiting. The handler stays registered for later chunks
			// and the final result.
			if log != nil {
				log.WriteChunk(out.GetStream(), out.GetChunk())
			}
			return
		}
		hub.SetRespHandler(execID, nil)
		resultCh <- msg.GetExecResult()
	})
	hub.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Exec{
			Exec: &agentpb.ExecRequest{
				ExecId:          execID,
				Command:         command,
				IsScript:        true,
				TimeoutSec:      timeoutSec,
				StallTimeoutSec: stallSec,
			},
		},
	})
	wait := time.Duration(timeoutSec)*time.Second + 30*time.Second
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}
	select {
	case res := <-resultCh:
		if res == nil {
			return nil, fmt.Errorf("agent disconnected")
		}
		if log != nil {
			log.WriteTail(res)
		}
		return res, nil
	case <-time.After(wait):
		// The agent stopped answering (or its result was lost). Tell it
		// to stop the command so nothing keeps running on the host after
		// the deployment already failed.
		hub.SetRespHandler(execID, nil)
		sendExecCancel(hub, execID)
		return nil, fmt.Errorf("agent exec timeout")
	case <-ctxDone:
		// The pipeline was cancelled (operator cancel, or a step that
		// gave up waiting). Same treatment: stop the command on the
		// agent, then report the caller's cancellation.
		hub.SetRespHandler(execID, nil)
		sendExecCancel(hub, execID)
		return nil, ctx.Err()
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

// pullImage pulls one image reference on the target host. It is the upgrade
// and CI trigger path — floating tags (:latest) must re-pull instead of
// reusing a stale local copy.
func (e *Engine) pullImage(ctx context.Context, hub *rpc.Hub, app *Application, imageRef string, logBuf *logBuffer, cancelled func() bool, fail func(string, ...any)) bool {
	if cancelled() {
		return false
	}
	pullCmd := fmt.Sprintf("docker pull %s", shellQuote(imageRef))
	return e.execRegistryCommand(ctx, hub, app, imageRef, "docker-pull-"+randomToken(4), pullCmd, pullCmd, 1800, logBuf, fail, "拉取镜像失败")
}

// execRegistryCommand executes a docker pull command on the agent, using an
// ephemeral isolated DOCKER_CONFIG login when app.RegistryVaultID is set so
// registry credentials never linger in ~/.docker/config.json.
func (e *Engine) execRegistryCommand(ctx context.Context, hub *rpc.Hub, app *Application, imageRef, execPrefix, dockerCmd, displayCmd string, timeoutSec int32, logBuf *logBuffer, fail func(string, ...any), errTitle string) bool {
	var user, pass, regServer string
	if app != nil && app.RegistryVaultID != "" {
		if e.vault == nil {
			fail("获取镜像仓库凭据失败: vault 未启用")
			return false
		}
		cred, ok := e.vault.Get(app.RegistryVaultID)
		if !ok {
			fail("获取镜像仓库凭据失败: 凭据 %q 不存在", app.RegistryVaultID)
			return false
		}
		user = strings.TrimSpace(cred.Username)
		pass = cred.Secret
		if user == "" || pass == "" {
			fail("镜像仓库凭据 %q 缺少用户名或密码/Token", cred.Name)
			return false
		}
		if imageRef == "" && app.ComposeContent != "" {
			imageRef = firstComposeImage(app.ComposeContent)
		}
		regServer = resolveRegistryHost(imageRef)
	}

	if pass == "" {
		logBuf.WriteString("$ " + displayCmd + "\n")
		res, err := execOnAgent(ctx, hub, execPrefix, dockerCmd, timeoutSec, deployStallSec, logBuf)
		if err != nil {
			fail("%s: %v", errTitle, err)
			return false
		}
		if res.GetExitCode() != 0 {
			fail("%s (exit %d)", errTitle, res.GetExitCode())
			return false
		}
		return true
	}

	loginArgs := fmt.Sprintf("login -u %s --password-stdin", shellQuote(user))
	if regServer != "" {
		loginArgs = fmt.Sprintf("login %s -u %s --password-stdin", shellQuote(regServer), shellQuote(user))
	}
	wrapped := fmt.Sprintf(`set -e
TMP_DC=$(mktemp -d)
trap 'rm -rf "$TMP_DC"' EXIT
printf '%%s' %s | docker --config "$TMP_DC" %s >/dev/null
export DOCKER_CONFIG="$TMP_DC"
%s`, shellQuote(pass), loginArgs, dockerCmd)

	logBuf.WriteString("$ " + displayCmd + " (using vault registry credential)\n")
	res, err := execOnAgent(ctx, hub, execPrefix, wrapped, timeoutSec, deployStallSec, nil)
	if err != nil {
		fail("%s: %s", errTitle, string(redact([]byte(err.Error()), pass)))
		return false
	}
	logBuf.WriteResult(res, redact(res.GetStdout(), pass), redact(res.GetStderr(), pass))
	if res.GetExitCode() != 0 {
		fail("%s (exit %d)", errTitle, res.GetExitCode())
		return false
	}
	return true
}

func resolveRegistryHost(imageRef string) string {
	s := strings.TrimSpace(imageRef)
	first, _, hasSlash := strings.Cut(s, "/")
	if !hasSlash {
		return ""
	}
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first
	}
	return ""
}

func firstComposeImage(composeYAML string) string {
	for _, line := range strings.Split(composeYAML, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "image:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
			return strings.Trim(val, `'"`)
		}
	}
	return ""
}

// buildGitFetchScript renders the shallow-clone / branch-switch / rollback
// fetch script shared by deploy and migration pipelines.
func buildGitFetchScript(repoDir, repoURL, branch, pin, authArgs string) string {
	qDir := shellQuote(repoDir)
	qURL := shellQuote(repoURL)
	qBranch := shellQuote(branch)
	if pin != "" {
		qPin := shellQuote(pin)
		return fmt.Sprintf(`set -e
if [ -d %[1]s/.git ]; then
  CUR_URL=$(git -C %[1]s remote get-url origin 2>/dev/null || true)
  if [ "$CUR_URL" != %[2]s ]; then
    rm -rf %[1]s
  fi
fi
if [ ! -d %[1]s/.git ]; then
  mkdir -p %[1]s
  git %[5]s clone --depth 1 --branch %[3]s %[2]s %[1]s
fi
if ! git -C %[1]s cat-file -e %[4]s^{commit} 2>/dev/null; then
  if ! git %[5]s -C %[1]s fetch --depth 1 origin %[4]s 2>/dev/null; then
    if [ -f %[1]s/.git/shallow ]; then
      git %[5]s -C %[1]s fetch --unshallow origin '+refs/heads/*:refs/remotes/origin/*'
    else
      git %[5]s -C %[1]s fetch --force origin '+refs/heads/*:refs/remotes/origin/*'
    fi
  fi
fi
git -C %[1]s checkout --force %[4]s
git -C %[1]s reset --hard %[4]s
git -C %[1]s clean -fd
COMMIT=$(git -C %[1]s rev-parse --short HEAD)
MESSAGE=$(git -C %[1]s log -1 --pretty=%%s)
echo "WATCHMAN_COMMIT=$COMMIT"
echo "WATCHMAN_MESSAGE=$MESSAGE"`,
			qDir, qURL, qBranch, qPin, authArgs)
	}
	return fmt.Sprintf(`set -e
if [ -d %[1]s/.git ]; then
  CUR_URL=$(git -C %[1]s remote get-url origin 2>/dev/null || true)
  if [ "$CUR_URL" != %[2]s ]; then
    rm -rf %[1]s
  fi
fi
if [ ! -d %[1]s/.git ]; then
  mkdir -p %[1]s
  git %[4]s clone --depth 1 --branch %[3]s %[2]s %[1]s
fi
git %[4]s -C %[1]s fetch --depth 1 --force origin %[3]s:refs/watchman/target
git -C %[1]s checkout --force refs/watchman/target
git -C %[1]s reset --hard refs/watchman/target
git -C %[1]s clean -fd
COMMIT=$(git -C %[1]s rev-parse --short HEAD)
MESSAGE=$(git -C %[1]s log -1 --pretty=%%s)
echo "WATCHMAN_COMMIT=$COMMIT"
echo "WATCHMAN_MESSAGE=$MESSAGE"`,
		qDir, qURL, qBranch, authArgs)
}

// gitComposeLocation resolves the execution directory (honoring Monorepo
// build_context) and compose file path relative to that directory.
func gitComposeLocation(repoDir string, app *Application) (workDir, composeFile string) {
	composeFile = gitComposeFile(app)
	sub, err := repoinspect.NormalizeSubdir(app.BuildContext)
	if err != nil || sub == "" {
		return repoDir, composeFile
	}
	workDir = path.Join(repoDir, sub)
	if strings.HasPrefix(composeFile, sub+"/") {
		composeFile = strings.TrimPrefix(composeFile, sub+"/")
	}
	return workDir, composeFile
}

// checkDockerReady runs the preflight on the target host. A transport
// failure (agent gone mid-check, exec timeout) is reported as-is — it is
// not a Docker problem and must not be misclassified as one.
func (e *Engine) checkDockerReady(ctx context.Context, hub *rpc.Hub) error {
	res, err := execOnAgent(ctx, hub, "docker-preflight-"+randomToken(4), dockerPreflightScript(), 30, deployStallSec, nil)
	if err != nil {
		return fmt.Errorf("Docker 可用性检查失败: %w", err)
	}
	return classifyDockerPreflight(res.GetExitCode())
}

// logBuffer accumulates build output with a hard bound so one chatty build
// cannot bloat the JSONL history.
//
// The mutex exists because the same buffer is read by the API while the
// pipeline goroutine keeps appending: GET /deployments/:depID overlays the
// in-flight buffer so operators can watch a build before the final log is
// written onto the record.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
	// streamedOut/streamedErr count how many bytes of the current step's
	// stdout/stderr already arrived as live ExecOutput chunks (see
	// WriteChunk). WriteTail appends only the remainder, so the streamed
	// bytes are never duplicated when the final ExecResult lands.
	streamedOut int
	streamedErr int
}

// forwarderImage is the OCI image for 组网转发 (source_type=forward) apps:
// an alpine/socat relay container listening on the entry host and forwarding
// to the target over the overlay network.
const forwarderImage = "alpine/socat"

const maxBuildLogBytes = 64 * 1024

func (l *logBuffer) WriteString(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(s)
}

// BeginStream resets the streamed-byte counters for a new exec step. Call
// it before execOnAgent starts a step whose chunks feed this buffer, so a
// previous step's counts cannot swallow the next step's tail.
func (l *logBuffer) BeginStream() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.streamedOut = 0
	l.streamedErr = 0
}

// WriteChunk appends one live ExecOutput chunk to the log, in arrival
// order. The deployment-log poller overlays this buffer, so the chunk is
// visible in the panel within a poll cycle, long before the step ends.
// The first stderr chunk is preceded by the same "[stderr]" marker
// WriteResult uses, so the live view and the final snapshot read the same.
func (l *logBuffer) WriteChunk(stream string, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if stream == "stderr" {
		if l.streamedErr == 0 {
			l.b.WriteString("[stderr]\n")
		}
		l.appendBounded(chunk)
		l.streamedErr += len(chunk)
		return
	}
	l.appendBounded(chunk)
	l.streamedOut += len(chunk)
}

// WriteTail appends the part of the final ExecResult that did NOT arrive
// as live chunks (the last partial batch, or the whole output when the
// agent is too old to stream). It has the same shape as WriteResult and
// resets the counters for the next step.
func (l *logBuffer) WriteTail(res *agentpb.ExecResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := res.GetStdout()
	errBytes := res.GetStderr()
	// Chunks are a prefix of the final output by construction; clamp
	// defensively so a miscount can never panic the pipeline.
	if l.streamedOut > len(out) {
		l.streamedOut = len(out)
	}
	if l.streamedErr > len(errBytes) {
		l.streamedErr = len(errBytes)
	}
	if tail := out[l.streamedOut:]; len(tail) > 0 {
		l.appendBounded(tail)
		l.b.WriteByte('\n')
	}
	if tail := errBytes[l.streamedErr:]; len(tail) > 0 {
		if l.streamedErr == 0 {
			l.b.WriteString("[stderr]\n")
		}
		l.appendBounded(tail)
		l.b.WriteByte('\n')
	}
	l.streamedOut = 0
	l.streamedErr = 0
}

// WriteResult appends a step's stdout/stderr, redacted, bounded.
func (l *logBuffer) WriteResult(res *agentpb.ExecResult, stdoutRedacted, stderr []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A step that streamed its output reports the tail through WriteTail
	// instead; a direct WriteResult must not inherit stale counters.
	l.streamedOut = 0
	l.streamedErr = 0
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

// appendBounded trims at maxBuildLogBytes; callers must hold l.mu.
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
	l.mu.Lock()
	defer l.mu.Unlock()
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
	switch strings.TrimSpace(parts[1]) {
	case "username":
		return cred.Username
	default:
		// Unknown field: keep the placeholder so the mistake stays visible in
		// the container env instead of silently injecting the secret into a
		// variable the operator did not ask for.
		return v
	}
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
//
// The address is built with net.JoinHostPort, so an IPv6 host is bracketed
// ([2001:db8::1]:8080) for the socat "TCP:" target; a bare "2a13:...:8080"
// would be an invalid socat address.
func resolveForwardUpstream(port int, appMesh, entryMesh, appPublicIP, appInternalIP string) (string, error) {
	if appMesh != "" && entryMesh != "" {
		return net.JoinHostPort(appMesh, strconv.Itoa(port)), nil
	}
	if appPublicIP != "" {
		return net.JoinHostPort(appPublicIP, strconv.Itoa(port)), nil
	}
	if appInternalIP != "" {
		return net.JoinHostPort(appInternalIP, strconv.Itoa(port)), nil
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
	res, err := execOnAgent(nil, fwdHub, "fwd-run-"+randomToken(4), runCmd, 60, deployStallSec, nil)
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
	res, err := execOnAgent(nil, fwdHub, "clean-fwd-"+randomToken(4), "docker rm -f "+shellQuote(fwdContainer), 30, deployStallSec, nil)
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
	res, err := execOnAgent(nil, fwdHub, "fwd-run-"+randomToken(4), runCmd, 60, deployStallSec, nil)
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
