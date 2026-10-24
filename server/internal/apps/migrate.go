package apps

// 应用跨主机迁移(2026-10-23)。
//
// 把一个已部署的应用从源主机平滑搬到目标主机:可选把声明的挂载卷数据经 S3
// 中转到目标主机,切换应用主机绑定,重定向反向代理与组网转发,再在目标主机
// 完成一次完整构建+发布。任一步失败即自动回滚——把应用主机绑定、反代、组网
// 转发全部退回源主机,并在源主机重新拉起旧容器,避免服务失联。
//
// 迁移复用既有的部署管线语义(镜像/模板/Compose/Git 四种来源)、S3 预签名
// 直传原则(direct-to-storage,GB 级归档不过控制端)、以及卷打包/校验/解压的
// 脚本契约。区别在于:迁移过程中源容器先停、目标容器后起,失败必须能退回。

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"watchman/server/internal/rpc"
	"watchman/server/internal/snapshots"
)

// StartMigration enqueues a cross-host migration deployment. It validates the
// request synchronously (target host reachable, Linux, no conflicting volume
// op) so the caller gets a precise error instead of a queued run that fails.
func (e *Engine) StartMigration(app *Application, req MigrateAppReq, startedBy string) (*Deployment, error) {
	req.TargetHostID = strings.TrimSpace(req.TargetHostID)
	if req.TargetHostID == "" {
		return nil, fmt.Errorf("目标主机不能为空")
	}
	if req.TargetHostID == app.HostID {
		return nil, fmt.Errorf("目标主机不能与当前主机相同")
	}
	if e.hubFor(req.TargetHostID) == nil {
		return nil, fmt.Errorf("目标主机 Agent 不在线")
	}
	if ag := e.reg.GetAgent(req.TargetHostID); ag != nil && strings.EqualFold(ag.OS, "windows") {
		return nil, fmt.Errorf("目标主机为 Windows 系统,暂不支持容器反代与迁移")
	}
	if e.IsRunning(app.ID) {
		return nil, fmt.Errorf("该应用已有部署或迁移任务在执行")
	}
	if e.VolumeBusy(app.ID) {
		return nil, errVolumeOpInProgress
	}
	// Volume migration needs a destination. Checked here so the operator learns
	// about the missing S3 target before an async run is created.
	wantVolumes := req.MigrateVolumes && len(app.Volumes) > 0
	if wantVolumes {
		if _, err := e.defaultS3Target(); err != nil {
			return nil, fmt.Errorf("数据卷迁移需要 S3 存储目标: %v", err)
		}
		if !e.acquireVolumeSlot(app.ID) {
			return nil, errVolumeOpInProgress
		}
	}

	dep := &Deployment{
		ID:        "dep_" + randomToken(8),
		AppID:     app.ID,
		Trigger:   "migrate",
		Status:    DeployQueued,
		StartedBy: startedBy,
		StartedAt: time.Now(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := e.registerRunning(app.ID, &activeDeployment{depID: dep.ID, cancel: cancel}); err != nil {
		if wantVolumes {
			e.releaseVolumeSlot(app.ID)
		}
		cancel()
		return nil, err
	}
	if err := e.store.AddDeployment(dep); err != nil {
		e.finishRunning(app.ID, dep.ID)
		if wantVolumes {
			e.releaseVolumeSlot(app.ID)
		}
		cancel()
		return nil, err
	}

	go e.runMigration(ctx, app, dep, req)
	return dep, nil
}

// PrecheckMigration reports whether the target host is ready to receive the
// app. It is read-only: it never mutates store or host state.
func (e *Engine) PrecheckMigration(app *Application, targetHostID string) (*MigratePrecheckResp, error) {
	targetHostID = strings.TrimSpace(targetHostID)
	if targetHostID == "" {
		return nil, fmt.Errorf("target_host_id 不能为空")
	}
	resp := &MigratePrecheckResp{
		TargetHostID: targetHostID,
		VolumeCount:  len(app.Volumes),
		Warnings:     []string{},
	}
	if targetHostID == app.HostID {
		resp.Warnings = append(resp.Warnings, "目标主机与当前主机相同,无法迁移")
	}

	if tgt, err := e.defaultS3Target(); err == nil && tgt != nil {
		resp.S3Ready = true
		resp.S3Bucket = tgt.Bucket
	}
	if len(app.Volumes) > 0 && !resp.S3Ready {
		resp.Warnings = append(resp.Warnings, "未配置默认 S3 存储目标,无法自动同步挂载卷数据")
	}
	if app.SourceType == "raw_compose" && len(app.Volumes) > 0 {
		resp.Warnings = append(resp.Warnings, "Compose 应用的挂载卷在 YAML 内声明,不支持自动数据卷同步")
	}

	// Topology warnings depend only on the app's own configuration, so they
	// must surface even when the target agent is unreachable — that is exactly
	// the moment the operator is deciding whether the move can work at all.
	if app.MeshForwardEnabled && app.MeshForwardHostID == targetHostID {
		resp.Warnings = append(resp.Warnings, "组网转发入口主机即目标主机,迁移后入口不再可行,需重新配置入口主机")
	}
	if len(app.ProxyDomains) > 0 && app.ProxyMode == "gateway" && app.ProxyGatewayHostID == targetHostID {
		resp.Warnings = append(resp.Warnings, "网关主机即目标主机,迁移后网关上游客即目标主机自身,建议更换网关主机")
	}

	// An unwired registry means no agent can be online; report it as offline
	// rather than silently returning a half-filled response.
	targetHub := e.hubFor(targetHostID)
	if targetHub == nil {
		resp.Warnings = append(resp.Warnings, "目标主机 Agent 离线,无法迁移")
		return resp, nil
	}
	resp.AgentOnline = true

	if ag := e.reg.GetAgent(targetHostID); ag != nil && strings.EqualFold(ag.OS, "windows") {
		resp.Warnings = append(resp.Warnings, "目标主机为 Windows 系统,暂不支持容器反代与迁移")
	}

	if res, err := execOnAgent(targetHub, "mig-pre-docker-"+randomToken(4),
		"command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1", 15); err == nil && res.GetExitCode() == 0 {
		resp.DockerRunning = true
	} else {
		resp.Warnings = append(resp.Warnings, "目标主机 Docker 未安装或未运行")
	}

	if resp.DockerRunning && len(app.Ports) > 0 {
		resp.PortConflicts = detectPortConflicts(targetHub, app.Ports)
		if len(resp.PortConflicts) > 0 {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("目标主机以下端口已被占用: %s", intsToCSV(resp.PortConflicts)))
		}
	}

	return resp, nil
}

// detectPortConflicts returns the host ports already listening on the target.
func detectPortConflicts(hub *rpc.Hub, ports []PortMapping) []int {
	portStrs := make([]string, 0, len(ports))
	for _, p := range ports {
		if p.Host > 0 {
			portStrs = append(portStrs, strconv.Itoa(p.Host))
		}
	}
	if len(portStrs) == 0 {
		return nil
	}
	script := fmt.Sprintf(`for p in %s; do
  if (ss -tuln 2>/dev/null || netstat -tuln 2>/dev/null) | grep -q ":$p "; then echo $p; fi
done`, strings.Join(portStrs, " "))
	res, err := execOnAgent(hub, "mig-pre-ports-"+randomToken(4), script, 15)
	if err != nil || res.GetExitCode() != 0 {
		return nil
	}
	var out []int
	for _, line := range strings.Split(strings.TrimSpace(string(res.GetStdout())), "\n") {
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func intsToCSV(in []int) string {
	parts := make([]string, 0, len(in))
	for _, n := range in {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

// runMigration drives the whole migration: quiesce source, sync volumes,
// rebind + reroute, deploy on target, optional source cleanup. Any failure
// rolls the app back onto the source host so it never ends up half-moved.
func (e *Engine) runMigration(ctx context.Context, app *Application, dep *Deployment, req MigrateAppReq) {
	defer e.finishRunning(app.ID, dep.ID)
	if req.MigrateVolumes && len(app.Volumes) > 0 {
		defer e.releaseVolumeSlot(app.ID)
	}

	logBuf := &logBuffer{}
	fail := func(format string, args ...any) {
		e.fail(app, dep, logBuf, fmt.Sprintf(format, args...))
	}
	cancelled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		e.cleanupCancelled(app, dep, e.reg.Hub(app.HostID), "", "", logBuf)
		return true
	}

	oldHostID := app.HostID
	targetHostID := req.TargetHostID
	container := app.ContainerName
	if container == "" {
		container = "watchman-app-" + app.ID
	}
	composePath := fmt.Sprintf("/var/lib/watchman/apps/%s/compose.yaml", app.ID)

	sourceHub := e.hubFor(oldHostID)
	targetHub := e.hubFor(targetHostID)
	if targetHub == nil {
		fail("目标主机 Agent 不在线")
		return
	}

	logBuf.WriteString(fmt.Sprintf("[MIGRATE] 服务迁移 %s: %s -> %s\n", app.Name, oldHostID, targetHostID))
	logBuf.WriteString(fmt.Sprintf("[MIGRATE] 迁移数据卷=%v 清理源容器=%v\n", req.MigrateVolumes, req.CleanSource))
	_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployBuilding })

	// ---- Stage 1: quiesce the source container so the volume snapshot is
	// consistent (no writes racing the tar).
	sourceWasRunning := false
	if sourceHub != nil {
		if st, err := execOnAgent(sourceHub, "mig-src-state-"+randomToken(4), volumeContainerStateScript(container), 15); err == nil &&
			strings.TrimSpace(string(st.GetStdout())) == "RUNNING" {
			sourceWasRunning = true
			logBuf.WriteString("$ 停止源主机容器(迁移期间不写入)\n")
			if cancelled() {
				return
			}
			stopCmd := fmt.Sprintf("docker stop %s", shellQuote(container))
			if app.SourceType == "raw_compose" {
				stopCmd = fmt.Sprintf("docker compose -f %s stop", shellQuote(composePath))
			}
			if _, err := execOnAgent(sourceHub, "mig-src-stop-"+randomToken(4), stopCmd, 90); err != nil {
				logBuf.WriteString(fmt.Sprintf("WARN: 停止源主机容器失败: %v\n", err))
			}
		}
	}

	rollback := func(cause error) {
		logBuf.WriteString(fmt.Sprintf("\n[ROLLBACK] 迁移失败: %v\n[ROLLBACK] 回滚应用至源主机 %s\n", cause, oldHostID))
		_ = e.store.UpdateApp(app.ID, func(a *Application) error {
			a.HostID = oldHostID
			return nil
		})
		app.HostID = oldHostID
		if e.proxy != nil && len(app.ProxyDomains) > 0 {
			if err := e.proxy.SyncProxyForHost(app, targetHostID); err != nil {
				logBuf.WriteString(fmt.Sprintf("WARN: 回滚反向代理失败: %v\n", err))
			} else {
				logBuf.WriteString("[ROLLBACK] 反向代理已指回源主机\n")
			}
		}
		if app.MeshForwardEnabled {
			e.syncMeshForwarder(app, logBuf)
		}
		if sourceWasRunning && sourceHub != nil {
			restartCmd := fmt.Sprintf("docker start %s", shellQuote(container))
			if app.SourceType == "raw_compose" {
				restartCmd = fmt.Sprintf("docker compose -f %s up -d", shellQuote(composePath))
			}
			if _, err := execOnAgent(sourceHub, "mig-rb-start-"+randomToken(4), restartCmd, 90); err != nil {
				logBuf.WriteString(fmt.Sprintf("WARN: 重启源主机容器失败: %v\n", err))
			} else {
				logBuf.WriteString("[ROLLBACK] 源主机容器已重新启动\n")
			}
		}
		if targetHub != nil {
			cleanCmd := fmt.Sprintf("docker rm -f %s %s-next 2>/dev/null || true", shellQuote(container), shellQuote(container))
			if app.SourceType == "raw_compose" {
				cleanCmd = fmt.Sprintf("docker compose -f %s down 2>/dev/null || true", shellQuote(composePath))
			}
			if _, err := execOnAgent(targetHub, "mig-rb-clean-"+randomToken(4), cleanCmd, 60); err != nil {
				logBuf.WriteString(fmt.Sprintf("WARN: 清理目标主机残留容器失败: %v\n", err))
			}
		}
	}

	// ---- Stage 2: migrate volume data through S3 (direct-to-storage).
	if req.MigrateVolumes && len(app.Volumes) > 0 {
		if sourceHub == nil {
			fail("源主机 Agent 离线,无法迁移数据卷")
			rollback(fmt.Errorf("源主机 Agent 离线"))
			return
		}
		target, err := e.defaultS3Target()
		if err != nil {
			fail("未配置默认 S3 存储目标")
			rollback(err)
			return
		}
		s3c := snapshots.NewS3Client(target)
		logBuf.WriteString("$ 经 S3 同步挂载卷数据\n")

		for _, spec := range app.Volumes {
			if cancelled() {
				rollback(fmt.Errorf("操作已取消"))
				return
			}
			src, dst, _, ok := parseVolumeSpec(spec)
			if !ok {
				logBuf.WriteString(fmt.Sprintf("WARN: 跳过无效卷规格 %s\n", spec))
				continue
			}
			logBuf.WriteString(fmt.Sprintf("--> 同步卷 %s -> %s\n", src, dst))

			srcDir, err := resolveVolumeDir(sourceHub, src)
			if err != nil {
				fail("解析源主机卷路径失败: %v", err)
				rollback(err)
				return
			}
			migID := "mig-" + randomToken(10)
			srcStaging := volumeStagingPath(app.ID, migID)
			s3Key := volumeObjectKey(target, app.ID, migID)

			res, err := execOnAgent(sourceHub, "mig-tar-"+migID, volumeBackupScript(srcDir, srcStaging), volumeS3OpTimeout)
			if err != nil || res.GetExitCode() != 0 {
				e.cleanupHostPaths(sourceHub, srcStaging)
				cerr := fmt.Errorf("打包卷数据失败(%s): %v", src, firstErr(err, res))
				fail("%v", cerr)
				rollback(cerr)
				return
			}
			_, sha := parseVolumeArchiveMeta(res.GetStdout())

			putURL, err := s3c.PresignPut(s3Key, 2*time.Hour)
			if err != nil {
				e.cleanupHostPaths(sourceHub, srcStaging)
				fail("生成 S3 上传地址失败: %v", err)
				rollback(err)
				return
			}
			upRes, upErr := execOnAgent(sourceHub, "mig-up-"+migID, volumeUploadScript(srcStaging, putURL), volumeS3OpTimeout)
			e.cleanupHostPaths(sourceHub, srcStaging)
			if upErr != nil || upRes.GetExitCode() != 0 {
				e.dropS3Object(s3c, s3Key)
				cerr := fmt.Errorf("上传卷数据至 S3 失败(%s): %v", src, firstErr(upErr, upRes))
				fail("%v", cerr)
				rollback(cerr)
				return
			}

			// Prepare the destination directory / named volume on the target.
			tgtDir, err := ensureVolumeDir(targetHub, src)
			if err != nil {
				e.dropS3Object(s3c, s3Key)
				fail("准备目标主机卷失败: %v", err)
				rollback(err)
				return
			}

			getURL, err := s3c.PresignGet(s3Key, 2*time.Hour)
			if err != nil {
				e.dropS3Object(s3c, s3Key)
				fail("生成 S3 下载地址失败: %v", err)
				rollback(err)
				return
			}
			tStaging := volumeRestorePath(app.ID, migID)
			dlRes, dlErr := execOnAgent(targetHub, "mig-dl-"+migID, volumeDownloadScript(getURL, tStaging), volumeS3OpTimeout)
			if dlErr != nil || dlRes.GetExitCode() != 0 {
				e.cleanupHostPaths(targetHub, tStaging)
				e.dropS3Object(s3c, s3Key)
				cerr := fmt.Errorf("下载卷数据失败(%s): %v", src, firstErr(dlErr, dlRes))
				fail("%v", cerr)
				rollback(cerr)
				return
			}

			sumRes, sumErr := execOnAgent(targetHub, "mig-sha-"+migID, volumeSHA256Script(tStaging), 120)
			tSha := strings.TrimSpace(string(sumRes.GetStdout()))
			if sumErr != nil || sumRes.GetExitCode() != 0 || (sha != "" && tSha != sha) {
				e.cleanupHostPaths(targetHub, tStaging)
				e.dropS3Object(s3c, s3Key)
				cerr := fmt.Errorf("卷数据校验不一致(%s: 源 %s / 目标 %s)", src, sha, tSha)
				fail("%v", cerr)
				rollback(cerr)
				return
			}

			unpack := fmt.Sprintf("set -e\nmkdir -p %s\ntar -C %s -xzf %s",
				shellQuote(tgtDir), shellQuote(tgtDir), shellQuote(tStaging))
			unRes, unErr := execOnAgent(targetHub, "mig-unpack-"+migID, unpack, volumeS3OpTimeout)
			e.cleanupHostPaths(targetHub, tStaging)
			e.dropS3Object(s3c, s3Key)
			if unErr != nil || unRes.GetExitCode() != 0 {
				cerr := fmt.Errorf("解压卷数据失败(%s): %v", src, firstErr(unErr, unRes))
				fail("%v", cerr)
				rollback(cerr)
				return
			}
			logBuf.WriteString(fmt.Sprintf("--> 卷 %s 同步完成 (sha256 %s)\n", src, tSha))
		}
	}

	if cancelled() {
		rollback(fmt.Errorf("操作已取消"))
		return
	}

	// ---- Stage 3: rebind the app to the target host and reroute traffic.
	logBuf.WriteString("$ 切换应用主机绑定与网络路由\n")
	_ = e.store.UpdateApp(app.ID, func(a *Application) error {
		a.HostID = targetHostID
		return nil
	})
	app.HostID = targetHostID

	if e.proxy != nil && len(app.ProxyDomains) > 0 {
		if err := e.proxy.SyncProxyForHost(app, oldHostID); err != nil {
			logBuf.WriteString(fmt.Sprintf("WARN: 同步反向代理失败: %v\n", err))
		} else {
			logBuf.WriteString("[PROXY] 反向代理已切换到目标主机\n")
		}
	}
	if app.MeshForwardEnabled {
		e.syncMeshForwarder(app, logBuf)
	}

	// ---- Stage 4: full build + rollout on the target host.
	_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) { d.Status = DeployDeploying })
	e.activeHub(app.ID, targetHub)

	if err := e.deployOnTargetHost(ctx, targetHub, app, dep, logBuf, container); err != nil {
		fail("目标主机部署未通过: %v", err)
		rollback(err)
		return
	}

	// ---- Stage 5: optional source cleanup. Bind-mount host dirs are never
	// touched, only the containers/volumes created by the platform.
	if req.CleanSource && sourceHub != nil {
		cleanCmd := fmt.Sprintf("docker rm -f %s %s-prev %s-next 2>/dev/null || true",
			shellQuote(container), shellQuote(container), shellQuote(container))
		if app.SourceType == "raw_compose" {
			cleanCmd = fmt.Sprintf("docker compose -f %s down --remove-orphans 2>/dev/null || true", shellQuote(composePath))
		}
		logBuf.WriteString("$ 清理源主机容器(保留宿主数据目录)\n")
		if _, err := execOnAgent(sourceHub, "mig-src-clean-"+randomToken(4), cleanCmd, 90); err != nil {
			logBuf.WriteString(fmt.Sprintf("WARN: 清理源主机容器失败: %v\n", err))
		} else {
			logBuf.WriteString("[CLEAN] 源主机容器已清理,数据目录严格保留\n")
		}
	} else if sourceWasRunning {
		logBuf.WriteString("[SOURCE] 源主机容器已停止,作为冷备保留\n")
	}

	// ---- Stage 6: done.
	e.finishDeploySuccess(app, dep, dep.CommitHash, app.Image, container, logBuf)
	logBuf.WriteString(fmt.Sprintf("[MIGRATE] 迁移完成: %s -> %s\n", oldHostID, targetHostID))
	e.log.Info("application migrated", "app", app.ID, "from", oldHostID, "to", targetHostID, "deployment", dep.ID)
}

// deployOnTargetHost runs the full build + rollout pipeline against the target
// host. It mirrors runWithPin's branches (template/image/raw_compose/git) but
// returns an error instead of finalizing the deployment, so the caller can roll
// back on failure.
func (e *Engine) deployOnTargetHost(ctx context.Context, hub *rpc.Hub, app *Application, dep *Deployment, logBuf *logBuffer, container string) error {
	buildTimeout := app.BuildTimeout
	if buildTimeout <= 0 {
		buildTimeout = 900
	}

	switch app.SourceType {
	case "raw_compose":
		logBuf.WriteString("$ 目标主机部署 Compose 编排\n")
		appDir := fmt.Sprintf("/var/lib/watchman/apps/%s", app.ID)
		composePath := fmt.Sprintf("%s/compose.yaml", appDir)
		b64 := base64.StdEncoding.EncodeToString([]byte(app.ComposeContent))
		writeScript := fmt.Sprintf(`mkdir -p %s && echo %s | base64 -d > %s`,
			shellQuote(appDir), shellQuote(b64), shellQuote(composePath))
		if res, err := execOnAgent(hub, "mig-write-compose-"+randomToken(4), writeScript, 30); err != nil || res.GetExitCode() != 0 {
			return fmt.Errorf("写入 Compose 配置失败: %v", firstErr(err, res))
		}
		upCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --remove-orphans", shellQuote(composePath), shellQuote("watchman-"+app.ID))
		logBuf.WriteString("$ " + upCmd + "\n")
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := execOnAgent(hub, "mig-compose-up-"+randomToken(4), upCmd, buildTimeout)
		if err != nil {
			return fmt.Errorf("Docker Compose 启动失败: %v", err)
		}
		logBuf.WriteResult(res, nil, res.GetStderr())
		if res.GetExitCode() != 0 {
			return fmt.Errorf("Docker Compose 启动失败 (exit %d)", res.GetExitCode())
		}
		if !e.probe(ctx, hub, app, container, logBuf) {
			return fmt.Errorf("健康检查未通过")
		}
		return nil

	case "image", "template":
		if app.SourceType == "template" {
			if err := ResolveTemplate(app); err != nil {
				return fmt.Errorf("解析应用模板失败: %v", err)
			}
		}
		logBuf.WriteString("$ 目标主机拉起容器\n")
		return e.runContainerOnTarget(ctx, hub, app, logBuf, app.Image, container)
	}

	// source_type == "git": clone/fetch, then build (dockerfile or compose).
	repoDir := fmt.Sprintf("/var/lib/watchman/apps/%s/repo", app.ID)
	imageTag := fmt.Sprintf("watchman-app-%s", app.ID)
	branch := app.Branch
	if branch == "" {
		branch = "main"
	}
	authToken, authErr := e.gitToken(app)
	if authErr != nil {
		return fmt.Errorf("获取 Git 凭据失败: %v", authErr)
	}
	authArgs := gitAuthArgs(authToken)
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
		shellQuote(repoDir), shellQuote(app.RepoURL), shellQuote(branch), shellQuote("origin/"+branch), authArgs)
	logBuf.WriteString("$ git fetch/checkout " + app.RepoURL + " (" + branch + ")\n")
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := execOnAgent(hub, "mig-git-"+randomToken(4), fetchScript, 300)
	if err != nil {
		return fmt.Errorf("代码拉取失败: %v", err)
	}
	logBuf.WriteResult(res, redact(res.GetStdout(), authToken), res.GetStderr())
	if res.GetExitCode() != 0 {
		return fmt.Errorf("代码拉取失败 (exit %d)", res.GetExitCode())
	}
	commit, message := parseCommitMeta(res.GetStdout())
	if commit != "" {
		dep.CommitHash = commit
		dep.CommitMessage = message
		_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
			d.CommitHash = commit
			d.CommitMessage = message
		})
	}

	if app.BuildType == "compose" {
		composeFile := app.Dockerfile
		if composeFile == "" || composeFile == "Dockerfile" {
			composeFile = "compose.yaml"
		}
		upCmd := fmt.Sprintf("docker compose -f %s -p %s up -d --build --remove-orphans",
			shellQuote(composeFile), shellQuote("watchman-"+app.ID))
		logBuf.WriteString("$ " + upCmd + "\n")
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err = execOnAgent(hub, "mig-compose-up-"+randomToken(4),
			fmt.Sprintf("cd %s && %s", shellQuote(repoDir), upCmd), buildTimeout)
		if err != nil {
			return fmt.Errorf("Docker Compose 部署失败: %v", err)
		}
		logBuf.WriteResult(res, nil, res.GetStderr())
		if res.GetExitCode() != 0 {
			return fmt.Errorf("Docker Compose 部署失败 (exit %d)", res.GetExitCode())
		}
		if !e.probe(ctx, hub, app, container, logBuf) {
			return fmt.Errorf("健康检查未通过")
		}
		return nil
	}

	imageRef := imageTag
	if commit != "" {
		imageRef = fmt.Sprintf("%s:%s", imageTag, commit)
	}
	dockerfile := app.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	buildContext := app.BuildContext
	if buildContext == "" {
		buildContext = "."
	}
	buildCmd := fmt.Sprintf("docker build -t %s -f %s %s", shellQuote(imageRef), shellQuote(dockerfile), shellQuote(buildContext))
	logBuf.WriteString("$ " + buildCmd + "\n")
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err = execOnAgent(hub, "mig-build-"+randomToken(4),
		fmt.Sprintf("cd %s && %s", shellQuote(repoDir), buildCmd), buildTimeout)
	if err != nil {
		return fmt.Errorf("Docker 构建失败: %v", err)
	}
	logBuf.WriteResult(res, nil, res.GetStderr())
	if res.GetExitCode() != 0 {
		return fmt.Errorf("Docker 构建失败 (exit %d)", res.GetExitCode())
	}
	return e.runContainerOnTarget(ctx, hub, app, logBuf, imageRef, container)
}

// runContainerOnTarget starts the container (with mesh-aware port bindings) and
// health-probes it. Unlike deploySingleContainer it does not swap or prune: the
// previous instance lives on another host, so there is nothing to replace here.
func (e *Engine) runContainerOnTarget(ctx context.Context, hub *rpc.Hub, app *Application, logBuf *logBuffer, imageRef, container string) error {
	envArgs := e.resolveEnvArgs(app, logBuf)
	var runArgs []string

	needsMesh := false
	for _, pm := range app.Ports {
		if pm.BindScope == "mesh" {
			needsMesh = true
			break
		}
	}
	meshIP := ""
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

	runCmd := fmt.Sprintf("docker rm -f %s 2>/dev/null; docker run -d --name %s --restart unless-stopped %s %s %s",
		shellQuote(container), shellQuote(container), strings.Join(envArgs, " "), strings.Join(runArgs, " "), shellQuote(imageRef))
	logBuf.WriteString("$ 启动目标主机容器\n")
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := execOnAgent(hub, "mig-run-"+randomToken(4), runCmd, 120)
	if err != nil {
		return fmt.Errorf("启动容器失败: %v", err)
	}
	logBuf.WriteResult(res, nil, res.GetStderr())
	if res.GetExitCode() != 0 {
		return fmt.Errorf("启动容器失败 (exit %d)", res.GetExitCode())
	}
	if !e.probe(ctx, hub, app, container, logBuf) {
		return fmt.Errorf("健康检查未通过")
	}
	return nil
}

// hubFor returns the agent channel for a host, or nil when unwired/offline.
func (e *Engine) hubFor(hostID string) *rpc.Hub {
	if e.reg == nil || hostID == "" {
		return nil
	}
	return e.reg.Hub(hostID)
}

// resolveVolumeDir turns a docker -v source into an absolute host directory:
// bind mounts are used verbatim, named volumes are resolved via docker.
func resolveVolumeDir(hub *rpc.Hub, src string) (string, error) {
	if volumeIsBindMount(src) {
		return src, nil
	}
	res, err := execOnAgent(hub, "mig-vol-mp-"+randomToken(4), volumeResolveMountpointScript(src), 30)
	if err != nil {
		return "", err
	}
	if res.GetExitCode() != 0 {
		return "", fmt.Errorf("具名卷 %s 不存在或解析失败", src)
	}
	dir := strings.TrimSpace(string(res.GetStdout()))
	if dir == "" {
		return "", fmt.Errorf("具名卷 %s 挂载点为空", src)
	}
	return dir, nil
}

// ensureVolumeDir prepares the destination on the target: named volumes are
// created (idempotent) and resolved to their mountpoint; bind-mount directories
// are created. Returns the host directory to unpack into.
func ensureVolumeDir(hub *rpc.Hub, src string) (string, error) {
	if volumeIsBindMount(src) {
		if _, err := execOnAgent(hub, "mig-mkdir-"+randomToken(4), fmt.Sprintf("mkdir -p %s", shellQuote(src)), 30); err != nil {
			return "", err
		}
		return src, nil
	}
	script := fmt.Sprintf("docker volume create %s >/dev/null && %s", shellQuote(src), volumeResolveMountpointScript(src))
	res, err := execOnAgent(hub, "mig-vol-create-"+randomToken(4), script, 30)
	if err != nil {
		return "", err
	}
	if res.GetExitCode() != 0 {
		return "", fmt.Errorf("在目标主机创建具名卷 %s 失败", src)
	}
	dir := strings.TrimSpace(string(res.GetStdout()))
	if dir == "" {
		return "", fmt.Errorf("目标主机具名卷 %s 挂载点为空", src)
	}
	return dir, nil
}

// dropS3Object deletes a migration staging object; it is best-effort because a
// leftover object is harmless (no metadata references it) and the run's real
// failure is reported elsewhere.
func (e *Engine) dropS3Object(c *snapshots.S3Client, key string) {
	if c == nil || key == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.DeleteObject(ctx, key); err != nil {
		e.log.Warn("migration staging object cleanup failed", "key", key, "err", err)
	}
}

// firstErr returns the transport error when present, else the exec result's
// stderr (so a non-zero exit carries its message into the failure reason).
func firstErr(err error, res interface{ GetStderr() []byte }) error {
	if err != nil {
		return err
	}
	if msg := strings.TrimSpace(string(res.GetStderr())); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("非零退出")
}
