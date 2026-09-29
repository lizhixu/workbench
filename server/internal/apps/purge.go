package apps

import (
	"fmt"
	"strings"
)

// purgeAppResources removes the runtime resources an application created on
// its host: the single container, the compose project (containers, networks,
// named volumes), built images, named volumes declared in the app spec, the
// app working directory, and the reverse-proxy vhost configs.
//
// It is best-effort: individual failures are collected as warnings instead
// of aborting the whole cleanup. A nil hub (agent offline) is a hard error —
// the caller should refuse the purge so the user can retry when the agent is
// back instead of silently leaving resources behind.
func (h *AppHandlers) purgeAppResources(app *Application) ([]string, error) {
	hub := h.reg.Hub(app.HostID)
	if hub == nil {
		return nil, fmt.Errorf("Agent 离线，无法清理应用「%s」的容器/数据卷等内容；可先只删除记录，或等待 Agent 上线后重试", app.Name)
	}

	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	// exec runs a command on the agent and returns trimmed stdout.
	// ok=false means the command failed; the caller decides whether to warn.
	exec := func(opID, cmd string, timeoutSec int32) (string, bool) {
		res, err := execOnAgent(hub, opID, cmd, timeoutSec)
		if err != nil {
			return "", false
		}
		out := strings.TrimSpace(string(res.GetStdout()))
		if res.GetExitCode() != 0 {
			if stderr := strings.TrimSpace(string(res.GetStderr())); stderr != "" {
				return out, false
			}
			return out, false
		}
		return out, true
	}

	// rmIDs removes docker objects (containers/volumes/networks/images) whose
	// IDs are listed in ids, one exec per object kind.
	rmIDs := func(what, listCmd, rmCmd string) {
		out, ok := exec("purge-list-"+randomToken(4), listCmd, 30)
		if !ok || out == "" {
			return
		}
		for _, id := range strings.Fields(out) {
			if _, ok := exec("purge-rm-"+randomToken(4), rmCmd+" "+shellQuote(id), 60); !ok {
				warn("删除%s %s 失败", what, id)
			}
		}
	}

	project := "watchman-" + app.ID

	// 1. Compose project resources (raw_compose and git compose branches).
	//    Label filters make this work even if the compose file is gone.
	rmIDs("容器",
		fmt.Sprintf(`docker ps -aq --filter %s`, shellQuote("label=com.docker.compose.project="+project)),
		"docker rm -f")
	rmIDs("数据卷",
		fmt.Sprintf(`docker volume ls -q --filter %s`, shellQuote("label=com.docker.compose.project="+project)),
		"docker volume rm")
	rmIDs("网络",
		fmt.Sprintf(`docker network ls -q --filter %s`, shellQuote("label=com.docker.compose.project="+project)),
		"docker network rm")

	// 2. Single-container deployments.
	container := app.ContainerName
	if container == "" {
		container = "watchman-app-" + app.ID
	}
	rmIDs("容器",
		fmt.Sprintf(`docker ps -aq --filter %s`, shellQuote("name=^/"+container+"$")),
		"docker rm -f")

	// 3. Named volumes declared in the app's volume specs ("name:/path").
	//    Bind mounts (host paths) are never touched.
	for _, spec := range app.Volumes {
		src := strings.TrimSpace(strings.SplitN(spec, ":", 2)[0])
		if src == "" || strings.Contains(src, "/") || strings.HasPrefix(src, ".") {
			continue
		}
		if _, ok := exec("purge-vinspect-"+randomToken(4), "docker volume inspect "+shellQuote(src)+" >/dev/null 2>&1", 15); !ok {
			continue
		}
		if _, ok := exec("purge-vrm-"+randomToken(4), "docker volume rm "+shellQuote(src), 30); !ok {
			warn("删除数据卷 %s 失败（可能仍被其他容器使用）", src)
		}
	}

	// 4. Images built for this app (any tag of watchman-app-<id>).
	rmIDs("镜像",
		"docker images -q "+shellQuote("watchman-app-"+app.ID),
		"docker rmi")

	// 5. App working directory (compose.yaml etc.).
	if _, ok := exec("purge-dir-"+randomToken(4),
		"rm -rf "+shellQuote("/var/lib/watchman/apps/"+app.ID), 15); !ok {
		warn("删除应用工作目录失败")
	}

	// 6. Reverse-proxy vhost configs on the app host (+ gateway host).
	if app.Domain != "" {
		targets := []string{app.HostID}
		if app.ProxyMode == "gateway" && app.ProxyGatewayHostID != "" {
			targets = append(targets, app.ProxyGatewayHostID)
		}
		confPath := fmt.Sprintf("%s/watchman-%s.conf", proxyConfigDir, app.ID)
		for _, hostID := range dedupe(targets) {
			phub := h.reg.Hub(hostID)
			if phub == nil {
				warn("主机 %s 离线，代理配置未清理", hostID)
				continue
			}
			_, _ = execOnAgent(phub, "purge-proxy-"+randomToken(4),
				fmt.Sprintf("rm -f %s && docker exec %s nginx -s reload 2>/dev/null || true",
					confPath, nginxContainer), 30)
		}
	}

	return warnings, nil
}
