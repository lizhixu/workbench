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
	// The source must be reachable too: without it stage 1 cannot stop the
	// source container, and a volumeless migration would otherwise "succeed"
	// while the source instance keeps running — a live dual-active pair.
	if e.hubFor(app.HostID) == nil {
		return nil, fmt.Errorf("源主机 Agent 不在线")
	}
	if ag := e.reg.GetAgent(app.HostID); ag != nil && strings.EqualFold(ag.OS, "windows") {
		return nil, fmt.Errorf("源主机为 Windows 系统,暂不支持容器反代与迁移")
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

// migratedVolumeData records one volume unpacked onto the target during this
// run so rollback can remove exactly the files this run wrote. The whole
// target directory is never wiped: a pre-existing target directory may hold
// data the migration never touched, and deleting it would destroy data the
// migration itself did not overwrite.
type migratedVolumeData struct {
	tgtDir string
	files  []string
}

// volumeFileCleanupScript removes exactly the files this run unpacked onto
// the target (tar members recorded before unpacking). It never wipes the
// directory itself: pre-existing target data must survive a rollback.
func volumeFileCleanupScript(mv migratedVolumeData) string {
	var sb strings.Builder
	sb.WriteString("set -e\n")
	for _, f := range mv.files {
		sb.WriteString(fmt.Sprintf("rm -f %s\n", shellQuote(strings.TrimSuffix(mv.tgtDir, "/")+"/"+f)))
	}
	return sb.String()
}

// parseTarMembers extracts the file members of a tar listing (tar -tzf
// output): directory entries, absolute paths, parent traversals and
// control-character names are dropped — rollback must only ever delete
// paths strictly under the volume's target directory.
func parseTarMembers(out string) []string {
	var members []string
	for _, line := range strings.Split(out, "\n") {
		m := strings.TrimPrefix(strings.TrimSpace(line), "./")
		if m == "" || m == "." || strings.HasSuffix(m, "/") ||
			strings.HasPrefix(m, "/") || strings.Contains(m, "\n") || strings.Contains(m, "\r") {
			continue
		}
		bad := false
		for _, part := range strings.Split(m, "/") {
			if part == ".." {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		members = append(members, m)
	}
	return members
}

// targetContainerCleanupScript removes the app's container plus any -next
// replacement containers (legacy unsuffixed and new dep-suffixed names).
// The -next match uses a docker name filter, not a shell glob: globs expand
// against filenames, never container names.
func targetContainerCleanupScript(container string) string {
	return fmt.Sprintf("docker rm -f %s 2>/dev/null || true; for id in $(docker ps -aq --filter %s); do docker rm -f $id 2>/dev/null || true; done",
		shellQuote(container), shellQuote("name=^/"+container+"-next"))
}

// gitComposeFile resolves the compose file of a git-backed compose app, with
// the same defaulting as the deploy path (Dockerfile field holds the compose
// file path for compose builds; empty or "Dockerfile" means compose.yaml).
func gitComposeFile(app *Application) string {
	f := app.Dockerfile
	if f == "" || f == "Dockerfile" {
		return "compose.yaml"
	}
	return f
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
	defer e.trackLog(dep.ID, logBuf)()
	fail := func(format string, args ...any) {
		e.fail(app, dep, logBuf, fmt.Sprintf(format, args...))
	}

	oldHostID := app.HostID
	targetHostID := req.TargetHostID
	container := app.ContainerName
	if container == "" {
		container = "watchman-app-" + app.ID
	}
	composePath := fmt.Sprintf("/var/lib/watchman/apps/%s/compose.yaml", app.ID)
	repoDir := fmt.Sprintf("/var/lib/watchman/apps/%s/repo", app.ID)

	// Volumes unpacked onto the target by this run, for rollback cleanup.
	var migrated []migratedVolumeData

	sourceHub := e.hubFor(oldHostID)
	targetHub := e.hubFor(targetHostID)
	if targetHub == nil {
		fail("目标主机 Agent 不在线")
		return
	}
	// The source may have gone offline between acceptance (which now checks
	// it) and this goroutine starting: nothing has moved yet, so fail
	// without rolling back rather than proceeding into a dual-active pair.
	if sourceHub == nil {
		fail("源主机 Agent 不在线")
		return
	}

	logBuf.WriteString(fmt.Sprintf("[MIGRATE] 服务迁移 %s: %s -> %s\n", app.Name, oldHostID, targetHostID))
	logBuf.WriteString(fmt.Sprintf("[MIGRATE] 迁移数据卷=%v 清理源容器=%v\n", req.MigrateVolumes, req.CleanSource))
	// A cancel that landed between acceptance and this goroutine starting
	// already finalized the record: a bare write would resurrect it to
	// building, so go through the terminal guard and stop if final.
	if !e.setDeploymentStatus(app.ID, dep.ID, DeployBuilding) {
		return
	}

	// rollback undoes a half-finished migration: host binding, proxy and
	// relay back to the source, source container restarted, target residue
	// (including this run's unpacked volume files) removed. It returns the
	// steps that failed so the caller can stamp "rollback incomplete" onto
	// the record — a rollback that itself failed must never look like a
	// clean failure-with-rollback.
	sourceWasRunning := false
	rollback := func(cause error) []string {
		var issues []string
		logBuf.WriteString(fmt.Sprintf("\n[ROLLBACK] 迁移失败: %v\n[ROLLBACK] 回滚应用至源主机 %s\n", cause, oldHostID))
		if app.HostID != oldHostID {
			_ = e.store.UpdateApp(app.ID, func(a *Application) error {
				a.HostID = oldHostID
				return nil
			})
			app.HostID = oldHostID
		}
		if e.proxy != nil && len(app.ProxyDomains) > 0 {
			if err := e.proxy.SyncProxyForHost(app, targetHostID); err != nil {
				issues = append(issues, fmt.Sprintf("反向代理指回失败: %v", err))
				logBuf.WriteString(fmt.Sprintf("WARN: 回滚反向代理失败: %v\n", err))
			} else {
				logBuf.WriteString("[ROLLBACK] 反向代理已指回源主机\n")
			}
		}
		if app.MeshForwardEnabled {
			if err := e.syncMeshForwarder(app, logBuf); err != nil {
				issues = append(issues, fmt.Sprintf("组网转发重定向失败: %v", err))
			}
		}
		if sourceWasRunning && sourceHub != nil {
			restartCmd := fmt.Sprintf("docker start %s", shellQuote(container))
			if app.SourceType == "raw_compose" {
				restartCmd = fmt.Sprintf("docker compose -f %s -p %s up -d", shellQuote(composePath), shellQuote("watchman-"+app.ID))
			}
			restartRes, restartErr := execOnAgent(sourceHub, "mig-rb-start-"+randomToken(4), restartCmd, 90)
			if restartErr != nil || restartRes.GetExitCode() != 0 {
				issues = append(issues, fmt.Sprintf("源主机容器重启失败: %v", firstErr(restartErr, restartRes)))
				logBuf.WriteString(fmt.Sprintf("WARN: 重启源主机容器失败: %v\n", firstErr(restartErr, restartRes)))
			} else {
				logBuf.WriteString("[ROLLBACK] 源主机容器已重新启动\n")
			}
		}
		if targetHub != nil {
			cleanCmd := targetContainerCleanupScript(container)
			if app.SourceType == "raw_compose" {
				cleanCmd = fmt.Sprintf("docker compose -f %s -p %s down 2>/dev/null || true", shellQuote(composePath), shellQuote("watchman-"+app.ID))
			} else if app.SourceType == "git" && app.BuildType == "compose" {
				// A git-backed compose stack is not a single container:
				// `docker rm` would miss every <project>-<service>-1
				// container, so bring the stack down instead.
				cleanCmd = fmt.Sprintf("cd %s && docker compose -f %s -p %s down 2>/dev/null || true",
					shellQuote(repoDir), shellQuote(gitComposeFile(app)), shellQuote("watchman-"+app.ID))
			}
			cleanRes, cleanErr := execOnAgent(targetHub, "mig-rb-clean-"+randomToken(4), cleanCmd, 60)
			if cleanErr != nil || cleanRes.GetExitCode() != 0 {
				issues = append(issues, fmt.Sprintf("目标主机残留清理失败: %v", firstErr(cleanErr, cleanRes)))
				logBuf.WriteString(fmt.Sprintf("WARN: 清理目标主机残留容器失败: %v\n", firstErr(cleanErr, cleanRes)))
			}
			// Remove exactly the files this run unpacked onto the target:
			// a retry must not eat stale data, and wiping the whole
			// directory could destroy pre-existing target data. The
			// script runs under `set -e`, so a mid-script abort surfaces
			// as a non-zero exit and must mark the rollback incomplete
			// instead of passing silently on a clean transport.
			for _, mv := range migrated {
				if len(mv.files) == 0 {
					continue
				}
				volRes, volErr := execOnAgent(targetHub, "mig-rb-vol-"+randomToken(4), volumeFileCleanupScript(mv), 120)
				if volErr != nil || volRes.GetExitCode() != 0 {
					volFail := firstErr(volErr, volRes)
					issues = append(issues, fmt.Sprintf("目标卷数据清理失败(%s): %v", mv.tgtDir, volFail))
					logBuf.WriteString(fmt.Sprintf("WARN: 回滚清理目标卷数据失败(%s): %v\n", mv.tgtDir, volFail))
				}
			}
		}
		return issues
	}

	// failRollback records the failure, rolls back, and stamps the record
	// when rollback itself was incomplete.
	failRollback := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		fail("%s", msg)
		e.markRollbackIncomplete(app, dep, rollback(fmt.Errorf("%s", msg)))
	}

	// cancelled finalizes a cancelled run. Before the host switch it rolls
	// back first (restart the stopped source container, clean target
	// residue) and only then marks cancelled — a cancel must not leave the
	// service stopped on the source behind a bare "cancelled" record.
	cancelled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		issues := rollback(fmt.Errorf("操作已取消"))
		e.cleanupCancelled(app, dep, nil, "", "", logBuf)
		// cleanupCancelled rewrote Error, so re-stamp the rollback marker.
		e.markRollbackIncomplete(app, dep, issues)
		return true
	}

	// abortIfGone stops the run when the application record vanished
	// mid-run (a delete that raced the start): continuing would deploy,
	// reroute or clean up resources for an app that no longer exists.
	abortIfGone := func() bool {
		if _, ok := e.store.GetApp(app.ID); ok {
			return false
		}
		if targetHub != nil {
			cleanCmd := targetContainerCleanupScript(container)
			if app.SourceType == "raw_compose" {
				cleanCmd = fmt.Sprintf("docker compose -f %s -p %s down 2>/dev/null || true", shellQuote(composePath), shellQuote("watchman-"+app.ID))
			} else if app.SourceType == "git" && app.BuildType == "compose" {
				// A git-backed compose stack is not a single container:
				// `docker rm` would miss every <project>-<service>-1
				// container, so bring the stack down instead.
				cleanCmd = fmt.Sprintf("cd %s && docker compose -f %s -p %s down 2>/dev/null || true",
					shellQuote(repoDir), shellQuote(gitComposeFile(app)), shellQuote("watchman-"+app.ID))
			}
			_, _ = execOnAgent(targetHub, "mig-gone-clean-"+randomToken(4), cleanCmd, 60)
			// Best-effort cleanup of volume files this run already
			// unpacked onto the target: the app record is gone, so
			// nothing will ever consume them. Files whose tar listing
			// failed (empty member list) cannot be enumerated safely
			// and are left behind rather than wiping the directory.
			for _, mv := range migrated {
				if len(mv.files) == 0 {
					continue
				}
				if _, err := execOnAgent(targetHub, "mig-gone-vol-"+randomToken(4), volumeFileCleanupScript(mv), 120); err != nil {
					logBuf.WriteString(fmt.Sprintf("WARN: 应用已删除但清理目标卷数据失败(%s): %v\n", mv.tgtDir, err))
				}
			}
		}
		fail("应用在迁移期间已被删除，迁移已中止")
		return true
	}

	if abortIfGone() {
		return
	}

	// ---- Stage 1: quiesce the source container so the volume snapshot is
	// consistent (no writes racing the tar).
	if sourceHub != nil {
		st, stErr := execOnAgent(sourceHub, "mig-src-state-"+randomToken(4), volumeContainerStateScript(container), 15)
		// A failed probe must fail closed: proceeding to tar while the
		// source keeps writing would pack a torn snapshot. (A clean
		// STOPPED answer only means there is nothing to stop.)
		if stErr != nil || st.GetExitCode() != 0 {
			failRollback("探测源主机容器状态失败: %v", firstErr(stErr, st))
			return
		}
		if strings.TrimSpace(string(st.GetStdout())) == "RUNNING" {
			sourceWasRunning = true
			logBuf.WriteString("$ 停止源主机容器(迁移期间不写入)\n")
			if cancelled() {
				return
			}
			stopCmd := fmt.Sprintf("docker stop %s", shellQuote(container))
			if app.SourceType == "raw_compose" {
				stopCmd = fmt.Sprintf("docker compose -f %s -p %s stop", shellQuote(composePath), shellQuote("watchman-"+app.ID))
			}
			// A failed stop breaks the snapshot-consistency guarantee the
			// whole migration rests on: fail and roll back instead of
			// packing a torn snapshot while the source keeps writing.
			stopRes, stopErr := execOnAgent(sourceHub, "mig-src-stop-"+randomToken(4), stopCmd, 90)
			if stopErr != nil || stopRes.GetExitCode() != 0 {
				failRollback("停止源主机容器失败: %v", firstErr(stopErr, stopRes))
				return
			}
		}
	}

	// ---- Stage 2: migrate volume data through S3 (direct-to-storage).
	if req.MigrateVolumes && len(app.Volumes) > 0 {
		if sourceHub == nil {
			// Unreachable now (the run start checks the source), kept as a
			// guard so a later refactor cannot silently reintroduce it.
			failRollback("源主机 Agent 离线,无法迁移数据卷")
			return
		}
		target, err := e.defaultS3Target()
		if err != nil {
			failRollback("未配置默认 S3 存储目标")
			return
		}
		s3c := snapshots.NewS3Client(target)
		logBuf.WriteString("$ 经 S3 同步挂载卷数据\n")

		for _, spec := range app.Volumes {
			// cancelled() already rolls back internally.
			if cancelled() {
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
				failRollback("解析源主机卷路径失败: %v", err)
				return
			}
			migID := "mig-" + randomToken(10)
			srcStaging := volumeStagingPath(app.ID, migID)
			s3Key := volumeObjectKey(target, app.ID, migID)

			res, err := execOnAgent(sourceHub, "mig-tar-"+migID, volumeBackupScript(srcDir, srcStaging), volumeS3OpTimeout)
			if err != nil || res.GetExitCode() != 0 {
				e.cleanupHostPaths(sourceHub, srcStaging)
				cerr := fmt.Errorf("打包卷数据失败(%s): %v", src, firstErr(err, res))
				failRollback("%v", cerr)
				return
			}
			_, sha := parseVolumeArchiveMeta(res.GetStdout())

			putURL, err := s3c.PresignPut(s3Key, 2*time.Hour)
			if err != nil {
				e.cleanupHostPaths(sourceHub, srcStaging)
				failRollback("生成 S3 上传地址失败: %v", err)
				return
			}
			upRes, upErr := execOnAgent(sourceHub, "mig-up-"+migID, volumeUploadScript(srcStaging, putURL), volumeS3OpTimeout)
			e.cleanupHostPaths(sourceHub, srcStaging)
			if upErr != nil || upRes.GetExitCode() != 0 {
				e.dropS3Object(s3c, s3Key)
				cerr := fmt.Errorf("上传卷数据至 S3 失败(%s): %v", src, firstErr(upErr, upRes))
				failRollback("%v", cerr)
				return
			}

			// Prepare the destination directory / named volume on the target.
			tgtDir, err := ensureVolumeDir(targetHub, src)
			if err != nil {
				e.dropS3Object(s3c, s3Key)
				failRollback("准备目标主机卷失败: %v", err)
				return
			}

			getURL, err := s3c.PresignGet(s3Key, 2*time.Hour)
			if err != nil {
				e.dropS3Object(s3c, s3Key)
				failRollback("生成 S3 下载地址失败: %v", err)
				return
			}
			tStaging := volumeRestorePath(app.ID, migID)
			dlRes, dlErr := execOnAgent(targetHub, "mig-dl-"+migID, volumeDownloadScript(getURL, tStaging), volumeS3OpTimeout)
			if dlErr != nil || dlRes.GetExitCode() != 0 {
				e.cleanupHostPaths(targetHub, tStaging)
				e.dropS3Object(s3c, s3Key)
				cerr := fmt.Errorf("下载卷数据失败(%s): %v", src, firstErr(dlErr, dlRes))
				failRollback("%v", cerr)
				return
			}

			sumRes, sumErr := execOnAgent(targetHub, "mig-sha-"+migID, volumeSHA256Script(tStaging), 120)
			tSha := strings.TrimSpace(string(sumRes.GetStdout()))
			if sumErr != nil || sumRes.GetExitCode() != 0 {
				e.cleanupHostPaths(targetHub, tStaging)
				e.dropS3Object(s3c, s3Key)
				cerr := fmt.Errorf("校验卷数据失败(%s): %v", src, firstErr(sumErr, sumRes))
				failRollback("%v", cerr)
				return
			}
			// An empty source digest means the tar step produced no
			// integrity proof (its script always emits one on success):
			// skipping the comparison would bless unverified bytes.
			if sha == "" {
				e.cleanupHostPaths(targetHub, tStaging)
				e.dropS3Object(s3c, s3Key)
				failRollback("卷数据校验失败(%s: 源归档未返回 SHA256，无法校验完整性)", src)
				return
			}
			if tSha != sha {
				e.cleanupHostPaths(targetHub, tStaging)
				e.dropS3Object(s3c, s3Key)
				cerr := fmt.Errorf("卷数据校验不一致(%s: 源 %s / 目标 %s)", src, sha, tSha)
				failRollback("%v", cerr)
				return
			}

			// List the archive members before unpacking so rollback can
			// remove exactly what this run wrote (see migratedVolumeData).
			// A failed listing only degrades rollback cleanup for this
			// volume; the unpack itself still proceeds.
			var members []string
			if listRes, listErr := execOnAgent(targetHub, "mig-list-"+migID, fmt.Sprintf("tar -tzf %s", shellQuote(tStaging)), 120); listErr == nil && listRes.GetExitCode() == 0 {
				members = parseTarMembers(string(listRes.GetStdout()))
			} else {
				logBuf.WriteString(fmt.Sprintf("WARN: 无法列出卷 %s 的归档成员，回滚时无法清理该卷\n", src))
			}

			unpack := fmt.Sprintf("set -e\nmkdir -p %s\ntar -C %s -xzf %s",
				shellQuote(tgtDir), shellQuote(tgtDir), shellQuote(tStaging))
			unRes, unErr := execOnAgent(targetHub, "mig-unpack-"+migID, unpack, volumeS3OpTimeout)
			e.cleanupHostPaths(targetHub, tStaging)
			e.dropS3Object(s3c, s3Key)
			if unErr != nil || unRes.GetExitCode() != 0 {
				cerr := fmt.Errorf("解压卷数据失败(%s): %v", src, firstErr(unErr, unRes))
				failRollback("%v", cerr)
				return
			}
			migrated = append(migrated, migratedVolumeData{tgtDir: tgtDir, files: members})
			logBuf.WriteString(fmt.Sprintf("--> 卷 %s 同步完成 (sha256 %s)\n", src, tSha))
		}
	}

	// cancelled() already rolls back internally.
	if cancelled() {
		return
	}
	if abortIfGone() {
		return
	}

	// ---- Stage 3: rebind the app to the target host and reroute traffic.
	logBuf.WriteString("$ 切换应用主机绑定与网络路由\n")
	_ = e.store.UpdateApp(app.ID, func(a *Application) error {
		a.HostID = targetHostID
		return nil
	})
	app.HostID = targetHostID

	// A failed reroute must fail the migration (and roll back): the source
	// container is already stopped, so "success with a WARN" would leave
	// the domain pointing at a stopped container.
	if e.proxy != nil && len(app.ProxyDomains) > 0 {
		if err := e.proxy.SyncProxyForHost(app, oldHostID); err != nil {
			// A cancel landing mid-reroute must finalize as cancelled
			// (with rollback), not as a failure: failRollback would
			// stamp failed over the operator's cancel.
			if cancelled() {
				return
			}
			failRollback("同步反向代理失败: %v", err)
			return
		}
		logBuf.WriteString("[PROXY] 反向代理已切换到目标主机\n")
	}
	if app.MeshForwardEnabled {
		if err := e.syncMeshForwarder(app, logBuf); err != nil {
			if cancelled() {
				return
			}
			failRollback("同步组网转发失败: %v", err)
			return
		}
	}

	// Cancelled between the reroute and stage 4: without this check the
	// stage-4 entry below sees the terminal-cancelled record, bails out,
	// and the run returns with the service still stopped on the source.
	if cancelled() {
		return
	}

	// ---- Stage 4: full build + rollout on the target host.
	if !e.setDeploymentStatus(app.ID, dep.ID, DeployDeploying) {
		// The record is already terminal (a cancel landed in the gap
		// above): roll back instead of returning with the service
		// stopped on the source. cancelled() is a no-op when the
		// context was not cancelled.
		cancelled()
		return
	}
	e.activeHub(app.ID, targetHub)
	if abortIfGone() {
		return
	}

	if err := e.deployOnTargetHost(ctx, targetHub, app, dep, logBuf, container); err != nil {
		if ctx.Err() != nil {
			// Cancelled mid-deploy: roll back, then finalize as
			// cancelled (not failed) — and re-stamp any rollback
			// incompleteness that cleanupCancelled just overwrote.
			issues := rollback(err)
			e.cleanupCancelled(app, dep, nil, "", "", logBuf)
			e.markRollbackIncomplete(app, dep, issues)
			return
		}
		failRollback("目标主机部署未通过: %v", err)
		return
	}

	// ---- Stage 5: optional source cleanup. Bind-mount host dirs are never
	// touched, only the containers/volumes created by the platform.
	if abortIfGone() {
		return
	}
	if req.CleanSource && sourceHub != nil {
		cleanCmd := targetContainerCleanupScript(container) + fmt.Sprintf("; docker rm -f %s 2>/dev/null || true", shellQuote(container+"-prev"))
		if app.SourceType == "raw_compose" {
			cleanCmd = fmt.Sprintf("docker compose -f %s -p %s down --remove-orphans 2>/dev/null || true", shellQuote(composePath), shellQuote("watchman-"+app.ID))
		} else if app.SourceType == "git" && app.BuildType == "compose" {
			cleanCmd = fmt.Sprintf("cd %s && docker compose -f %s -p %s down 2>/dev/null || true",
				shellQuote(repoDir), shellQuote(gitComposeFile(app)), shellQuote("watchman-"+app.ID))
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
		if err := ctx.Err(); err != nil {
			return err
		}
		// Compose stack containers are named <project>-<service>-1, not
		// watchman-app-<id>: a container-name probe can never pass here
		// (the normal deploy path does not probe compose stacks at all).
		// Only a configured HTTP health check is meaningful.
		if app.HealthcheckURL != "" && !e.probe(ctx, hub, app, container, logBuf) {
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
		composeFile := gitComposeFile(app)
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
		if err := ctx.Err(); err != nil {
			return err
		}
		// Same container-name probe problem as the raw_compose branch:
		// only a configured HTTP health check is meaningful here.
		if app.HealthcheckURL != "" && !e.probe(ctx, hub, app, container, logBuf) {
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

// markRollbackIncomplete stamps a visible marker onto the deployment record
// when rollback itself failed: without it the record reads as a clean
// failure-with-rollback while the source container or the proxy may still be
// down. It also escalates to the server log — the previous code only left a
// WARN line inside the build log, which no operator watches.
// Note: there is no alert-channel wiring on Engine (that would need main.go
// injection), so the record marker plus the server error log are the
// notification here; wiring a real alert remains follow-up work.
func (e *Engine) markRollbackIncomplete(app *Application, dep *Deployment, issues []string) {
	if len(issues) == 0 {
		return
	}
	msg := "回滚未完成，需人工检查源主机容器与反代状态：" + strings.Join(issues, "；")
	_ = e.store.UpdateDeployment(app.ID, dep.ID, func(d *Deployment) {
		if d.Error != "" {
			d.Error += "；" + msg
		} else {
			d.Error = msg
		}
	})
	e.log.Error("migration rollback incomplete", "app", app.ID, "deployment", dep.ID, "issues", strings.Join(issues, "; "))
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
