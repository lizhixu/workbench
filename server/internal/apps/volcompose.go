package apps

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

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
		"/etc/localtime", "/etc/timezone",
		"/var/run/docker.sock", "/run/docker.sock",
		"/proc", "/sys", "/dev":
		return true
	}
	if strings.HasPrefix(cp, "/sys/") || strings.HasPrefix(cp, "/proc/") || strings.HasPrefix(cp, "/dev/") {
		return true
	}
	return false
}

type staticComposeYAML struct {
	Services map[string]staticComposeService `yaml:"services"`
}

type staticComposeService struct {
	Volumes []any `yaml:"volumes"`
}

// composeAppsRoot is the directory all raw_compose app data lives under.
// Relative bind sources are resolved within it; see resolveComposeBindHostPath.
const composeAppsRoot = "/var/lib/watchman/apps"

// resolveComposeBindHostPath resolves a relative bind source against the
// raw_compose app directory and checks containment. One leading ".." is
// legitimate ("../shared" -> /var/lib/watchman/apps/shared for shared volumes),
// but any number of ".." segments can escape appsRoot entirely (e.g.
// "../../../../etc" -> /etc). That host path later becomes the clean-restore
// rm -rf / untar target, so an escaping volume must be dropped here instead
// of being offered as a restore target. Absolute bind sources and named
// volumes never go through this resolver.
func resolveComposeBindHostPath(appID, src string) (hostPath string, ok bool) {
	hostPath = path.Join(composeAppsRoot, appID, strings.ReplaceAll(src, "\\", "/"))
	if !strings.HasPrefix(hostPath, composeAppsRoot+"/") {
		return "", false
	}
	return hostPath, true
}

// parseParentRelativeSpec parses a short-form volume spec whose source is a
// parent-relative path ("../shared:/data[:ro]", "..\\shared:/data"). It mirrors
// parseVolumeSpec's splitting but without its ".." traversal guard, which the
// static parser must not apply: the source is resolved with path.Join against
// the compose file's directory, so leading ".." segments are cleaned there.
// Mid-path ".." ("a/../b") and ".." in the container path stay rejected.
func parseParentRelativeSpec(spec string) (src, dst, mode string, ok bool) {
	s := strings.TrimSpace(spec)
	parts := strings.Split(s, ":")
	if n := len(parts); n >= 3 && (parts[n-1] == "ro" || parts[n-1] == "rw") {
		mode = parts[n-1]
		parts = parts[:n-1]
	}
	if len(parts) < 2 {
		return "", "", "", false
	}
	dst = strings.TrimSpace(parts[len(parts)-1])
	src = strings.TrimSpace(strings.Join(parts[:len(parts)-1], ":"))
	if src == "" || dst == "" || !strings.HasPrefix(dst, "/") || strings.Contains(dst, "..") {
		return "", "", "", false
	}
	norm := strings.ReplaceAll(src, "\\", "/")
	if norm != ".." && !strings.HasPrefix(norm, "../") {
		return "", "", "", false
	}
	for _, seg := range strings.Split(norm, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		if strings.Contains(seg, ":") {
			return "", "", "", false
		}
	}
	return src, dst, mode, true
}

// staticComposeVolumes parses app.ComposeContent statically when dynamic container
// inspection on the host is unavailable (containers stopped, not yet deployed, or agent offline).
func staticComposeVolumes(app *Application) []DiscoveredVolume {
	if app == nil || strings.TrimSpace(app.ComposeContent) == "" {
		return nil
	}
	var cf staticComposeYAML
	if err := yaml.Unmarshal([]byte(app.ComposeContent), &cf); err != nil {
		return nil
	}
	if len(cf.Services) == 0 {
		return nil
	}

	// Sort service names for deterministic volume order
	svcNames := make([]string, 0, len(cf.Services))
	for name := range cf.Services {
		svcNames = append(svcNames, name)
	}
	sort.Strings(svcNames)

	type mountKey struct {
		spec    string
		service string
	}
	seen := make(map[mountKey]bool)
	var out []DiscoveredVolume

	for _, svcName := range svcNames {
		svc := cf.Services[svcName]
		for _, raw := range svc.Volumes {
			var spec, hostPath, volType string
			var isRO bool
			var dst string

			switch v := raw.(type) {
			case string:
				v = strings.TrimSpace(v)
				if v == "" {
					continue
				}
				src, d, mode, ok := parseVolumeSpec(v)
				if !ok {
					// parseVolumeSpec rejects any ".." as a traversal guard
					// shared with the backup/migrate paths. The static
					// parser resolves parent-relative binds against the
					// compose file's directory itself, so leading ".."
					// segments are legitimate here and get their own
					// strict parse instead of being dropped.
					src, d, mode, ok = parseParentRelativeSpec(v)
					if !ok {
						continue
					}
				}
				if isIgnoredInternalMount(d) {
					continue
				}
				dst = d
				isRO = mode == "ro"
				spec = v
				if volumeIsBindMount(src) {
					volType = "bind"
					if strings.HasPrefix(src, "/") || (len(src) >= 3 && src[1] == ':' && (src[2] == '/' || src[2] == '\\')) {
						hostPath = src
					} else {
						// Relative bind mount ("./data", "../shared", "."):
						// resolve against the raw_compose file's directory
						// (/var/lib/watchman/apps/<id>/). ComposeContent is
						// only stored for raw_compose apps, so this base is
						// exact; path.Join cleans "." and ".." segments,
						// so "../shared" lands on the parent directory
						// instead of being mangled into <id>/shared. The
						// containment check drops sources that escape the
						// apps root (see resolveComposeBindHostPath).
						hp, contained := resolveComposeBindHostPath(app.ID, src)
						if !contained {
							continue
						}
						hostPath = hp
					}
				} else {
					volType = "volume"
					hostPath = ""
				}

			case map[string]any:
				t, _ := v["type"].(string)
				src, _ := v["source"].(string)
				tgt, _ := v["target"].(string)
				ro, _ := v["read_only"].(bool)
				src = strings.TrimSpace(src)
				tgt = strings.TrimSpace(tgt)
				if tgt == "" || !strings.HasPrefix(tgt, "/") {
					continue
				}
				// Only bind mounts and named volumes carry host data worth
				// backing up. tmpfs/cluster/npipe and friends do not, and a
				// sourceless entry (anonymous volume, tmpfs without source)
				// is not addressable — previously it produced a malformed
				// ":/data" candidate spec.
				if t != "" && t != "bind" && t != "volume" {
					continue
				}
				if src == "" {
					continue
				}
				if isIgnoredInternalMount(tgt) {
					continue
				}
				dst = tgt
				isRO = ro
				if t == "bind" || volumeIsBindMount(src) {
					volType = "bind"
					spec = src + ":" + tgt
					if isRO {
						spec += ":ro"
					}
					if strings.HasPrefix(src, "/") || (len(src) >= 3 && src[1] == ':' && (src[2] == '/' || src[2] == '\\')) {
						hostPath = src
					} else {
						hp, contained := resolveComposeBindHostPath(app.ID, src)
						if !contained {
							continue
						}
						hostPath = hp
					}
				} else {
					volType = "volume"
					spec = src + ":" + tgt
					if isRO {
						spec += ":ro"
					}
					hostPath = ""
				}

			default:
				continue
			}

			key := mountKey{spec: spec, service: svcName}
			if seen[key] {
				continue
			}
			seen[key] = true

			out = append(out, DiscoveredVolume{
				VolumeSpec:    spec,
				VolumeType:    volType,
				HostPath:      hostPath,
				ContainerPath: dst,
				Service:       svcName,
				ReadOnly:      isRO,
			})
		}
	}
	return out
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
// For Compose apps, it queries docker volume and container mounts on the agent, falling back
// to static parsing of app.ComposeContent when the agent is offline or containers are not running.
func (e *Engine) DiscoverAppVolumes(app *Application) ([]DiscoveredVolume, error) {
	if app == nil {
		return nil, fmt.Errorf("app is nil")
	}

	hub := e.volumeHub(app)
	if isComposeApp(app) {
		var disc []DiscoveredVolume
		var dynErr error
		if hub != nil {
			proj := composeProjectName(app.ID)
			res, err := execOnAgent(hub, "voldisc-"+randomToken(4), composeDiscoverScript(proj), 30)
			if err == nil && res.GetExitCode() == 0 {
				disc = parseComposeDiscoverOutput(res.GetStdout())
			} else if err != nil {
				dynErr = err
			} else {
				dynErr = fmt.Errorf("挂载发现脚本退出码 %d", res.GetExitCode())
			}
		}
		if len(disc) == 0 {
			disc = staticComposeVolumes(app)
		}
		if len(disc) == 0 {
			// A failed dynamic discovery with no static fallback is a
			// failure, not an empty volume list: returning nil here made
			// the volumes endpoint render "no mounts" and the backup
			// path report "未配置任何挂载卷", hiding the real error.
			// Genuinely empty results (successful discovery, or a compose
			// file that declares no volumes) still return empty, nil.
			if dynErr != nil {
				return nil, fmt.Errorf("Compose 挂载卷动态发现失败且静态解析无结果: %v", dynErr)
			}
			if hub == nil && strings.TrimSpace(app.ComposeContent) == "" {
				return nil, fmt.Errorf("目标主机 Agent 不在线，无法动态发现 Compose 挂载卷")
			}
		}
		return disc, nil
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
