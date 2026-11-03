package apps

import (
	"fmt"
	"path"
	"strings"

	"watchman/server/internal/rpc"
)

// DiscoveredVolume represents an active volume mount discovered from an
// application (either declared on a single-container app or inspected from
// a Docker Compose project).
type DiscoveredVolume struct {
	VolumeSpec    string `json:"volume_spec"`       // e.g. /host:/container or volname:/container
	VolumeType    string `json:"volume_type"`       // "bind" | "volume"
	HostPath      string `json:"host_path"`         // resolved host directory or volume mountpoint
	ContainerPath string `json:"container_path"`    // mount path inside container
	Service       string `json:"service,omitempty"` // compose service name when applicable
	ReadOnly      bool   `json:"read_only"`         // read-only flag (:ro)
}

// isComposeApp reports whether an application is managed by Docker Compose.
func isComposeApp(app *Application) bool {
	if app == nil {
		return false
	}
	if app.SourceType == "raw_compose" {
		return true
	}
	if app.SourceType == "git" && strings.EqualFold(strings.TrimSpace(app.BuildType), "compose") {
		return true
	}
	return false
}

// composeProjectName returns the standard docker compose project name for an app.
func composeProjectName(appID string) string {
	return "watchman-" + appID
}

// composeConfigPath returns the compose file path and working directory for an app.
func composeConfigPath(app *Application) (composeFile, workDir string) {
	if app.SourceType == "raw_compose" {
		return fmt.Sprintf("/var/lib/watchman/apps/%s/compose.yaml", app.ID), ""
	}
	// git + compose
	repoDir := fmt.Sprintf("/var/lib/watchman/apps/%s/repo", app.ID)
	cf := strings.TrimSpace(app.Dockerfile)
	if cf == "" || cf == "Dockerfile" {
		cf = "compose.yaml"
	}
	return cf, repoDir
}

// composeDiscoverScript generates a shell script to inspect all volumes and mounts
// belonging to a Docker Compose project on the managed host.
func composeDiscoverScript(project string) string {
	return fmt.Sprintf(`set -e
PROJECT=%s

# 1. Inspect named volumes belonging to the compose project
docker volume ls --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Name}}' 2>/dev/null | while read -r vname; do
  [ -z "$vname" ] && continue
  mp=$(docker volume inspect --format '{{.Mountpoint}}' "$vname" 2>/dev/null || true)
  echo "NAMED_VOL|$vname|$mp"
done

# 2. Inspect containers and their mounts belonging to the compose project
docker ps -a --filter "label=com.docker.compose.project=$PROJECT" --format '{{.ID}}' 2>/dev/null | while read -r cid; do
  [ -z "$cid" ] && continue
  svc=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$cid" 2>/dev/null || true)
  docker inspect --format '{{range .Mounts}}{{println .Type "|" .Name "|" .Source "|" .Destination "|" .RW}}{{end}}' "$cid" 2>/dev/null | while IFS='|' read -r mtype mname msrc mdst mrw; do
    [ -z "$mdst" ] && continue
    echo "CONTAINER_MOUNT|$svc|$mtype|$mname|$msrc|$mdst|$mrw"
  done
done`, shellQuote(project))
}

// isIgnoredInternalMount checks if a container mount is a Docker system-internal mount
// that should not be exposed or archived as an application data volume.
func isIgnoredInternalMount(containerPath string) bool {
	cp := path.Clean(containerPath)
	switch cp {
	case "/etc/hosts", "/etc/hostname", "/etc/resolv.conf",
		"/var/run/docker.sock", "/run/docker.sock",
		"/proc", "/sys", "/dev":
		return true
	}
	if strings.HasPrefix(cp, "/sys/") || strings.HasPrefix(cp, "/proc/") || strings.HasPrefix(cp, "/dev/") {
		return true
	}
	return false
}

// parseComposeDiscoverOutput parses stdout from composeDiscoverScript into structured DiscoveredVolumes.
func parseComposeDiscoverOutput(stdout []byte) []DiscoveredVolume {
	namedMountpoints := make(map[string]string)
	type mountKey struct {
		spec    string
		service string
	}
	seen := make(map[mountKey]bool)
	var out []DiscoveredVolume

	lines := strings.Split(string(stdout), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) >= 3 && parts[0] == "NAMED_VOL" {
			vname := strings.TrimSpace(parts[1])
			mp := strings.TrimSpace(parts[2])
			if vname != "" && mp != "" {
				namedMountpoints[vname] = mp
			}
			continue
		}
		if len(parts) >= 7 && parts[0] == "CONTAINER_MOUNT" {
			svc := strings.TrimSpace(parts[1])
			mtype := strings.TrimSpace(parts[2])
			mname := strings.TrimSpace(parts[3])
			msrc := strings.TrimSpace(parts[4])
			mdst := strings.TrimSpace(parts[5])
			mrw := strings.TrimSpace(parts[6])

			if isIgnoredInternalMount(mdst) {
				continue
			}

			isRO := mrw == "false" || mrw == "ro"
			var spec, hostPath, volType string

			if mtype == "volume" && mname != "" {
				volType = "volume"
				spec = mname + ":" + mdst
				if isRO {
					spec += ":ro"
				}
				hostPath = msrc
				if hostPath == "" {
					hostPath = namedMountpoints[mname]
				}
			} else if mtype == "bind" || volumeIsBindMount(msrc) {
				volType = "bind"
				spec = msrc + ":" + mdst
				if isRO {
					spec += ":ro"
				}
				hostPath = msrc
			} else {
				// anonymous volume or other type; retain as volume if named
				if mname != "" {
					volType = "volume"
					spec = mname + ":" + mdst
					if isRO {
						spec += ":ro"
					}
					hostPath = msrc
				} else {
					continue
				}
			}

			key := mountKey{spec: spec, service: svc}
			if seen[key] {
				continue
			}
			seen[key] = true

			out = append(out, DiscoveredVolume{
				VolumeSpec:    spec,
				VolumeType:    volType,
				HostPath:      hostPath,
				ContainerPath: mdst,
				Service:       svc,
				ReadOnly:      isRO,
			})
		}
	}
	return out
}

// DiscoverAppVolumes inspects the host to discover all operational volumes of an application.
// For single-container apps, it parses app.Volumes and optionally enriches mountpoints.
// For Compose apps, it queries docker volume and container mounts on the agent.
func (e *Engine) DiscoverAppVolumes(app *Application) ([]DiscoveredVolume, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}

	hub := e.volumeHub(app)
	if isComposeApp(app) {
		if hub == nil {
			return nil, fmt.Errorf("目标主机 Agent 不在线，无法动态发现 Compose 挂载卷")
		}
		proj := composeProjectName(app.ID)
		res, err := execOnAgent(hub, "voldisc-"+randomToken(4), composeDiscoverScript(proj), 30)
		if err != nil {
			return nil, fmt.Errorf("查询 Compose 挂载卷失败: %v", err)
		}
		if res.GetExitCode() != 0 {
			msg := strings.TrimSpace(string(res.GetStderr()))
			if msg == "" {
				msg = fmt.Sprintf("exit %d", res.GetExitCode())
			}
			return nil, fmt.Errorf("查询 Compose 挂载卷失败: %s", msg)
		}
		return parseComposeDiscoverOutput(res.GetStdout()), nil
	}

	// Single-container app
	var out []DiscoveredVolume
	for _, spec := range app.Volumes {
		src, dst, mode, ok := parseVolumeSpec(spec)
		if !ok {
			continue
		}
		volType := "bind"
		hostDir := src
		if !volumeIsBindMount(src) {
			volType = "volume"
			if hub != nil {
				res, err := execOnAgent(hub, "volpath-"+randomToken(4), volumeResolveMountpointScript(src), 15)
				if err == nil && res.GetExitCode() == 0 {
					if mp := strings.TrimSpace(string(res.GetStdout())); mp != "" {
						hostDir = mp
					}
				}
			}
		}
		out = append(out, DiscoveredVolume{
			VolumeSpec:    spec,
			VolumeType:    volType,
			HostPath:      hostDir,
			ContainerPath: dst,
			ReadOnly:      mode == "ro",
		})
	}
	return out, nil
}

// ---- Application Container Lifecycle Control ----

// IsAppRunning checks whether any container of the app is currently running.
func (e *Engine) IsAppRunning(hub *rpc.Hub, app *Application) (bool, error) {
	if hub == nil {
		return false, fmt.Errorf("agent offline")
	}
	if isComposeApp(app) {
		proj := composeProjectName(app.ID)
		cmd := fmt.Sprintf(`docker ps --filter %s --filter "status=running" -q`, shellQuote("label=com.docker.compose.project="+proj))
		res, err := execOnAgent(hub, "volst-"+randomToken(4), cmd, 30)
		if err != nil {
			return false, err
		}
		if res.GetExitCode() != 0 {
			return false, fmt.Errorf("检查 Compose 状态失败 (exit %d): %s", res.GetExitCode(), string(res.GetStderr()))
		}
		cids := strings.TrimSpace(string(res.GetStdout()))
		return cids != "", nil
	}

	// Single container
	container := app.ContainerName
	if container == "" {
		return false, nil
	}
	res, err := execOnAgent(hub, "volst-"+randomToken(4), volumeContainerStateScript(container), 30)
	if err != nil {
		return false, err
	}
	if res.GetExitCode() != 0 {
		return false, fmt.Errorf("检查容器状态失败 (exit %d): %s", res.GetExitCode(), string(res.GetStderr()))
	}
	return parseVolumeContainerState(res.GetStdout())
}

// PauseApp pauses container execution to suspend writes during backup.
func (e *Engine) PauseApp(hub *rpc.Hub, app *Application) error {
	if hub == nil {
		return fmt.Errorf("agent offline")
	}
	if isComposeApp(app) {
		proj := composeProjectName(app.ID)
		cmd := fmt.Sprintf(`CIDS=$(docker ps --filter %s --filter "status=running" -q); if [ -n "$CIDS" ]; then docker pause $CIDS; fi`,
			shellQuote("label=com.docker.compose.project="+proj))
		res, err := execOnAgent(hub, "volpause-"+randomToken(4), cmd, 60)
		if err != nil {
			return fmt.Errorf("暂停 Compose 容器失败: %v", err)
		}
		if res.GetExitCode() != 0 {
			return fmt.Errorf("暂停 Compose 容器失败: %s", string(res.GetStderr()))
		}
		return nil
	}

	container := app.ContainerName
	if container == "" {
		return nil
	}
	res, err := execOnAgent(hub, "volpause-"+randomToken(4), "docker pause "+shellQuote(container), 60)
	if err != nil {
		return fmt.Errorf("暂停应用容器失败: %v", err)
	}
	if res.GetExitCode() != 0 {
		return fmt.Errorf("暂停应用容器失败: %s", string(res.GetStderr()))
	}
	return nil
}

// UnpauseApp unpauses paused containers after backup completes.
func (e *Engine) UnpauseApp(hub *rpc.Hub, app *Application) error {
	if hub == nil {
		return fmt.Errorf("agent offline")
	}
	if isComposeApp(app) {
		proj := composeProjectName(app.ID)
		cmd := fmt.Sprintf(`CIDS=$(docker ps --filter %s --filter "status=paused" -q); if [ -n "$CIDS" ]; then docker unpause $CIDS; fi`,
			shellQuote("label=com.docker.compose.project="+proj))
		res, err := execOnAgent(hub, "volunpause-"+randomToken(4), cmd, 60)
		if err != nil {
			return fmt.Errorf("恢复 Compose 容器运行失败: %v", err)
		}
		if res.GetExitCode() != 0 {
			return fmt.Errorf("恢复 Compose 容器运行失败: %s", string(res.GetStderr()))
		}
		return nil
	}

	container := app.ContainerName
	if container == "" {
		return nil
	}
	res, err := execOnAgent(hub, "volunpause-"+randomToken(4), "docker unpause "+shellQuote(container), 60)
	if err != nil {
		return fmt.Errorf("恢复应用容器运行失败: %v", err)
	}
	if res.GetExitCode() != 0 {
		return fmt.Errorf("恢复应用容器运行失败: %s", string(res.GetStderr()))
	}
	return nil
}

// StopApp stops the app container(s) before a cold backup or clean restore.
func (e *Engine) StopApp(hub *rpc.Hub, app *Application) error {
	if hub == nil {
		return fmt.Errorf("agent offline")
	}
	if isComposeApp(app) {
		composeFile, workDir := composeConfigPath(app)
		proj := composeProjectName(app.ID)
		var cmd string
		if workDir != "" {
			cmd = fmt.Sprintf("cd %s && docker compose -f %s -p %s stop", shellQuote(workDir), shellQuote(composeFile), shellQuote(proj))
		} else {
			cmd = fmt.Sprintf("docker compose -f %s -p %s stop", shellQuote(composeFile), shellQuote(proj))
		}
		res, err := execOnAgent(hub, "volstop-"+randomToken(4), cmd, 180)
		if err != nil {
			return fmt.Errorf("停止 Compose 服务失败: %v", err)
		}
		if res.GetExitCode() != 0 {
			// Fallback: stop by label if compose file wasn't present
			fallback := fmt.Sprintf(`CIDS=$(docker ps --filter %s -q); if [ -n "$CIDS" ]; then docker stop $CIDS; fi`,
				shellQuote("label=com.docker.compose.project="+proj))
			fres, ferr := execOnAgent(hub, "volstop-fb-"+randomToken(4), fallback, 120)
			if ferr != nil || fres.GetExitCode() != 0 {
				return fmt.Errorf("停止 Compose 容器失败: %s", string(res.GetStderr()))
			}
		}
		return nil
	}

	container := app.ContainerName
	if container == "" {
		return nil
	}
	res, err := execOnAgent(hub, "volstop-"+randomToken(4), "docker stop "+shellQuote(container), 300)
	if err != nil {
		return fmt.Errorf("停止应用容器失败: %v", err)
	}
	if res.GetExitCode() != 0 {
		return fmt.Errorf("停止应用容器失败 (exit %d): %s", res.GetExitCode(), string(res.GetStderr()))
	}
	return nil
}

// StartApp starts the app container(s) after restore or cold backup completes.
func (e *Engine) StartApp(hub *rpc.Hub, app *Application) error {
	if hub == nil {
		return fmt.Errorf("agent offline")
	}
	if isComposeApp(app) {
		composeFile, workDir := composeConfigPath(app)
		proj := composeProjectName(app.ID)
		var cmd string
		if workDir != "" {
			cmd = fmt.Sprintf("cd %s && docker compose -f %s -p %s start", shellQuote(workDir), shellQuote(composeFile), shellQuote(proj))
		} else {
			cmd = fmt.Sprintf("docker compose -f %s -p %s start", shellQuote(composeFile), shellQuote(proj))
		}
		res, err := execOnAgent(hub, "volstart-"+randomToken(4), cmd, 180)
		if err != nil {
			return fmt.Errorf("启动 Compose 服务失败: %v", err)
		}
		if res.GetExitCode() != 0 {
			// If `start` failed, try `up -d`
			var upCmd string
			if workDir != "" {
				upCmd = fmt.Sprintf("cd %s && docker compose -f %s -p %s up -d --remove-orphans", shellQuote(workDir), shellQuote(composeFile), shellQuote(proj))
			} else {
				upCmd = fmt.Sprintf("docker compose -f %s -p %s up -d --remove-orphans", shellQuote(composeFile), shellQuote(proj))
			}
			upRes, upErr := execOnAgent(hub, "volup-"+randomToken(4), upCmd, 180)
			if upErr != nil || upRes.GetExitCode() != 0 {
				return fmt.Errorf("启动 Compose 容器失败: %s", string(res.GetStderr()))
			}
		}
		return nil
	}

	container := app.ContainerName
	if container == "" {
		return nil
	}
	res, err := execOnAgent(hub, "volstart-"+randomToken(4), "docker start "+shellQuote(container), 120)
	if err != nil {
		return fmt.Errorf("启动应用容器失败: %v", err)
	}
	if res.GetExitCode() != 0 {
		return fmt.Errorf("启动应用容器失败 (exit %d): %s", res.GetExitCode(), string(res.GetStderr()))
	}
	return nil
}
