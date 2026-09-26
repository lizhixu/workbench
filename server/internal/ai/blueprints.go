package ai

import "strings"

// ---- Built-in operational blueprints -----------------------------------
//
// These blueprints let the Terminal AI Copilot produce accurate, distro-aware
// multi-step plans for the most common operations with zero LLM configuration.
// Each blueprint is a matcher (keyword set) + a builder that emits steps
// tailored to the resolved host context (package manager, shell, arch).

type blueprint struct {
	// any of these keyword groups matching the prompt selects this blueprint;
	// within a group ALL tokens must be present (AND), across groups it's OR.
	keywordGroups [][]string
	build         func(hc hostPlanContext) *TaskPlanResponse
}

// matchBlueprint returns a tailored plan when the prompt matches a known
// operation, or nil to fall through to the LLM.
func matchBlueprint(prompt string, hc hostPlanContext) *TaskPlanResponse {
	p := strings.ToLower(prompt)
	for _, bp := range blueprints() {
		for _, group := range bp.keywordGroups {
			all := true
			for _, tok := range group {
				if !strings.Contains(p, tok) {
					all = false
					break
				}
			}
			if all {
				return bp.build(hc)
			}
		}
	}
	return nil
}

func blueprints() []blueprint {
	// Order matters: more specific diagnostic / cleanup blueprints come first,
	// because e.g. "清理 Docker 缓存" also contains "docker" and must not fall
	// into the docker-install blueprint below.
	return []blueprint{
		{
			keywordGroups: [][]string{
				{"docker", "清理"}, {"docker", "清除"}, {"docker", "prune"},
				{"docker", "缓存"}, {"镜像", "清理"}, {"docker", "clean"},
			},
			build: buildDockerCleanup,
		},
		{
			keywordGroups: [][]string{
				{"端口"}, {"port"}, {"占用端口"}, {"端口占用"},
			},
			build: buildPortInspect,
		},
		{
			keywordGroups: [][]string{
				{"cpu"}, {"负载"}, {"load"},
			},
			build: buildCPUTop,
		},
		{
			keywordGroups: [][]string{
				{"内存"}, {"memory", "泄漏"}, {"mem"}, {"oom"}, {"内存泄漏"},
			},
			build: buildMemInspect,
		},
		{
			keywordGroups: [][]string{
				{"大文件"}, {"大于"}, {"磁盘", "占用"}, {"空间", "满"}, {"large", "file"},
			},
			build: buildLargeFiles,
		},
		{
			keywordGroups: [][]string{{"docker"}, {"容器引擎"}},
			build:         buildDockerInstall,
		},
		{
			keywordGroups: [][]string{{"nginx"}},
			build:         buildNginxInstall,
		},
		{
			keywordGroups: [][]string{{"redis"}},
			build:         buildRedisInstall,
		},
	}
}

// buildDockerInstall builds the Docker CE install + verify plan.
func buildDockerInstall(hc hostPlanContext) *TaskPlanResponse {
	if hc.IsWindows {
		return &TaskPlanResponse{
			Title:   "在 Windows 上安装 Docker",
			Goal:    "安装 Docker 引擎",
			Summary: "Windows 主机建议手动安装 Docker Desktop；此处提供检测与提示步骤。",
			Steps: []TaskPlanStep{
				{Title: "检查 Docker 是否已安装", Description: "查询 docker 版本", Command: "docker --version", Probe: true},
				{Title: "提示", Description: "Windows 需安装 Docker Desktop（图形化），无法用脚本静默安装", Command: "echo '请从 https://www.docker.com/products/docker-desktop 下载 Docker Desktop 手动安装'", Probe: true},
			},
		}
	}

	// Linux: prefer the official convenience script (get.docker.com) which
	// auto-detects the distro. Fall back note handled by the model otherwise.
	startCmds := "systemctl enable --now docker"
	steps := []TaskPlanStep{
		{
			Title:       "检查是否已安装 Docker",
			Description: "若已安装则后续步骤会跳过重复安装（脚本自身幂等）",
			Command:     "docker --version 2>/dev/null || echo 'docker 未安装，将开始安装'",
			Probe:       true,
		},
		{
			Title:       "更新软件源索引",
			Description: "确保包管理器索引最新，避免依赖下载失败",
			Command:     pkgUpdateCmd(hc),
			ContinueOnError: true,
		},
		{
			Title:       "下载并执行 Docker 官方安装脚本",
			Description: "get.docker.com 会自动识别发行版并安装 Docker CE、containerd、CLI 插件",
			Command:     "curl -fsSL https://get.docker.com | sh",
		},
		{
			Title:       "启动并设置开机自启",
			Description: "启动 docker 服务并注册为开机自启",
			Command:     startCmds,
		},
		{
			Title:       "验证 Docker 引擎",
			Description: "查看版本与服务状态，确认安装成功",
			Command:     "docker --version && systemctl is-active docker",
			Probe:       true,
		},
		{
			Title:       "运行 hello-world 验证",
			Description: "拉取并运行官方测试镜像，确认容器可正常创建",
			Command:     "docker run --rm hello-world",
			Probe:       true,
			ContinueOnError: true,
		},
	}
	return &TaskPlanResponse{
		Title:   "安装 Docker 引擎",
		Goal:    "在目标主机上安装并启动 Docker",
		Summary: distroNote(hc, "使用 Docker 官方安装脚本（get.docker.com），自动适配发行版"),
		Steps:   steps,
	}
}

func buildNginxInstall(hc hostPlanContext) *TaskPlanResponse {
	if hc.IsWindows {
		return genericWindowsInstall("Nginx", "nginx")
	}
	installCmd := pkgInstallCmd(hc, "nginx")
	steps := []TaskPlanStep{
		{Title: "检查是否已安装 Nginx", Description: "查询 nginx 版本", Command: "nginx -v 2>&1 || echo 'nginx 未安装'", Probe: true},
		{Title: "更新软件源索引", Description: "刷新包索引", Command: pkgUpdateCmd(hc), ContinueOnError: true},
		{Title: "安装 Nginx", Description: "通过系统包管理器安装", Command: installCmd},
		{Title: "启动并设置开机自启", Description: "启动 nginx 服务", Command: "systemctl enable --now nginx"},
		{Title: "验证服务状态", Description: "确认 nginx 正在监听并可访问", Command: "systemctl is-active nginx && curl -fsS -o /dev/null -w '%{http_code}\\n' http://127.0.0.1/ || true", Probe: true},
	}
	return &TaskPlanResponse{
		Title:   "安装 Nginx",
		Goal:    "安装并启动 Nginx Web 服务器",
		Summary: distroNote(hc, "使用系统包管理器安装 Nginx"),
		Steps:   steps,
	}
}

func buildRedisInstall(hc hostPlanContext) *TaskPlanResponse {
	if hc.IsWindows {
		return genericWindowsInstall("Redis", "redis")
	}
	pkg := "redis-server"
	if hc.PkgMgr == "dnf" || hc.PkgMgr == "yum" || hc.PkgMgr == "apk" {
		pkg = "redis"
	}
	svc := "redis-server"
	if hc.PkgMgr == "dnf" || hc.PkgMgr == "yum" || hc.PkgMgr == "apk" {
		svc = "redis"
	}
	steps := []TaskPlanStep{
		{Title: "检查是否已安装 Redis", Description: "查询 redis 版本", Command: "redis-server --version 2>/dev/null || echo 'redis 未安装'", Probe: true},
		{Title: "更新软件源索引", Description: "刷新包索引", Command: pkgUpdateCmd(hc), ContinueOnError: true},
		{Title: "安装 Redis", Description: "通过系统包管理器安装", Command: pkgInstallCmd(hc, pkg)},
		{Title: "启动并设置开机自启", Description: "启动 redis 服务", Command: "systemctl enable --now " + svc},
		{Title: "验证 Redis", Description: "PING 应返回 PONG", Command: "redis-cli ping", Probe: true},
	}
	return &TaskPlanResponse{
		Title:   "安装 Redis",
		Goal:    "安装并启动 Redis 内存数据库",
		Summary: distroNote(hc, "使用系统包管理器安装 Redis"),
		Steps:   steps,
	}
}

func genericWindowsInstall(name, _ string) *TaskPlanResponse {
	return &TaskPlanResponse{
		Title:   "在 Windows 上安装 " + name,
		Goal:    "安装 " + name,
		Summary: "Windows 主机建议使用包管理器（winget/choco）或官方安装包手动安装。",
		Steps: []TaskPlanStep{
			{Title: "检查 winget 可用性", Description: "确认可用的包管理器", Command: "winget --version", Probe: true},
			{Title: "提示", Description: "请使用 winget install 或官方安装包安装 " + name, Command: "echo '请使用 winget install " + name + " 或下载官方安装包'", Probe: true},
		},
	}
}

// ---- distro-aware command helpers ----

func pkgUpdateCmd(hc hostPlanContext) string {
	switch hc.PkgMgr {
	case "apt":
		return "DEBIAN_FRONTEND=noninteractive apt-get update -y"
	case "dnf":
		return "dnf makecache -y"
	case "yum":
		return "yum makecache -y"
	case "apk":
		return "apk update"
	default:
		return "echo '未知包管理器，跳过更新'"
	}
}

func pkgInstallCmd(hc hostPlanContext, pkg string) string {
	switch hc.PkgMgr {
	case "apt":
		return "DEBIAN_FRONTEND=noninteractive apt-get install -y " + pkg
	case "dnf":
		return "dnf install -y " + pkg
	case "yum":
		return "yum install -y " + pkg
	case "apk":
		return "apk add --no-cache " + pkg
	default:
		return "echo '未知包管理器，无法自动安装 " + pkg + "'"
	}
}

func distroNote(hc hostPlanContext, base string) string {
	d := hc.Distro
	if d == "" {
		d = hc.OS
	}
	return base + "（目标发行版: " + d + " " + hc.Arch + "）。"
}

// ---- diagnostic / cleanup blueprints (mostly read-only probes) ----

func buildDockerCleanup(hc hostPlanContext) *TaskPlanResponse {
	steps := []TaskPlanStep{
		{Title: "检查 Docker 可用性", Description: "确认 docker 已安装并可访问", Command: "docker version --format '{{.Server.Version}}' 2>/dev/null || echo 'docker 不可用'", Probe: true},
		{Title: "查看清理前磁盘占用", Description: "docker system df 展示镜像/容器/卷占用", Command: "docker system df", Probe: true},
		{Title: "清理已停止容器/悬空镜像/网络/构建缓存", Description: "docker system prune 回收未使用的资源（保留在用镜像与命名卷）", Command: "docker system prune -af", NeedsConfirm: true, RiskLevel: "medium"},
		{Title: "查看清理后磁盘占用", Description: "对比回收效果", Command: "docker system df", Probe: true},
	}
	return &TaskPlanResponse{
		Title:   "清理未使用的 Docker 镜像与缓存",
		Goal:    "回收 Docker 占用的磁盘空间",
		Summary: "清理已停止容器、悬空/未使用镜像、未使用网络与构建缓存；正在使用的镜像和命名卷不受影响。",
		Steps:   steps,
	}
}

func buildPortInspect(hc hostPlanContext) *TaskPlanResponse {
	if hc.IsWindows {
		return &TaskPlanResponse{
			Title:   "排查端口占用",
			Goal:    "定位监听端口的进程",
			Summary: "使用 netstat 定位端口对应的 PID 与进程。",
			Steps: []TaskPlanStep{
				{Title: "列出监听端口与 PID", Description: "查看所有 TCP 监听端口", Command: "netstat -ano | findstr LISTENING", Probe: true},
				{Title: "查看占用进程", Description: "按 PID 反查进程名", Command: "tasklist", Probe: true},
			},
		}
	}
	steps := []TaskPlanStep{
		{Title: "列出所有监听端口及进程", Description: "ss 展示 TCP/UDP 监听端口对应的进程与 PID", Command: "ss -tulnp", Probe: true},
		{Title: "统计端口占用 Top", Description: "按监听端口聚合，快速定位异常监听", Command: "ss -tulnp | awk 'NR>1{print $5}' | sed 's/.*://' | sort | uniq -c | sort -rn | head", Probe: true},
	}
	return &TaskPlanResponse{
		Title:   "排查端口占用",
		Goal:    "定位监听端口及其对应进程",
		Summary: "只读排查：列出监听端口、对应 PID 与进程名，不做任何变更。",
		Steps:   steps,
	}
}

func buildCPUTop(hc hostPlanContext) *TaskPlanResponse {
	if hc.IsWindows {
		return &TaskPlanResponse{
			Title:   "排查 CPU 占用最高的进程",
			Goal:    "定位高 CPU 进程",
			Summary: "使用 PowerShell 按 CPU 排序进程。",
			Steps: []TaskPlanStep{
				{Title: "CPU Top5 进程", Description: "按 CPU 时间排序取前 5", Command: "Get-Process | Sort-Object CPU -Descending | Select-Object -First 5 Name,Id,CPU", Probe: true},
			},
		}
	}
	steps := []TaskPlanStep{
		{Title: "查看整体负载", Description: "uptime 显示 1/5/15 分钟平均负载", Command: "uptime", Probe: true},
		{Title: "CPU 占用 Top5 进程", Description: "按 CPU 使用率排序取前 5 个进程", Command: "ps -eo pid,ppid,user,%cpu,%mem,comm --sort=-%cpu | head -6", Probe: true},
	}
	return &TaskPlanResponse{
		Title:   "排查 CPU 负载最高的 5 个进程",
		Goal:    "定位高 CPU 进程",
		Summary: "只读排查：显示系统负载与 CPU 占用最高的进程，不做任何变更。",
		Steps:   steps,
	}
}

func buildMemInspect(hc hostPlanContext) *TaskPlanResponse {
	if hc.IsWindows {
		return &TaskPlanResponse{
			Title:   "排查内存占用",
			Goal:    "定位高内存进程",
			Summary: "使用 PowerShell 按内存排序进程。",
			Steps: []TaskPlanStep{
				{Title: "内存 Top5 进程", Description: "按工作集排序取前 5", Command: "Get-Process | Sort-Object WS -Descending | Select-Object -First 5 Name,Id,@{N='WS(MB)';E={[math]::Round($_.WS/1MB,1)}}", Probe: true},
			},
		}
	}
	steps := []TaskPlanStep{
		{Title: "查看内存总览", Description: "free 显示内存与 swap 使用情况", Command: "free -h", Probe: true},
		{Title: "内存占用 Top5 进程", Description: "按内存使用率排序取前 5 个进程", Command: "ps -eo pid,ppid,user,%mem,%cpu,rss,comm --sort=-%mem | head -6", Probe: true},
	}
	return &TaskPlanResponse{
		Title:   "排查内存占用最高的进程",
		Goal:    "定位高内存 / 疑似内存泄漏进程",
		Summary: "只读排查：显示内存总览与占用最高的进程，不做任何变更。",
		Steps:   steps,
	}
}

func buildLargeFiles(hc hostPlanContext) *TaskPlanResponse {
	if hc.IsWindows {
		return &TaskPlanResponse{
			Title:   "排查大文件",
			Goal:    "定位占用磁盘的大文件",
			Summary: "使用 PowerShell 查找大文件。",
			Steps: []TaskPlanStep{
				{Title: "查看磁盘占用", Description: "列出各分区剩余空间", Command: "Get-PSDrive -PSProvider FileSystem", Probe: true},
				{Title: "查找 C 盘大文件 Top10", Description: "按大小排序查找大文件", Command: "Get-ChildItem C:\\ -Recurse -File -ErrorAction SilentlyContinue | Sort-Object Length -Descending | Select-Object -First 10 FullName,@{N='Size(MB)';E={[math]::Round($_.Length/1MB,1)}}", Probe: true},
			},
		}
	}
	steps := []TaskPlanStep{
		{Title: "查看各挂载点磁盘占用", Description: "df 展示各文件系统使用率", Command: "df -h", Probe: true},
		{Title: "查找大于 100M 的大文件 Top20", Description: "从根目录扫描大文件（跳过虚拟文件系统），按大小排序", Command: "find / -xdev -type f -size +100M -printf '%s\\t%p\\n' 2>/dev/null | sort -rn | head -20 | awk '{printf \"%.1fMB\\t%s\\n\", $1/1048576, $2}'", Probe: true},
		{Title: "查看占用空间最大的目录 Top10", Description: "统计根目录下各目录体积", Command: "du -xh / 2>/dev/null | sort -rh | head -10", Probe: true, ContinueOnError: true},
	}
	return &TaskPlanResponse{
		Title:   "排查大于 100M 的大文件",
		Goal:    "定位占用磁盘空间的大文件与大目录",
		Summary: "只读排查：显示磁盘使用率、最大的文件与目录，不删除任何内容。",
		Steps:   steps,
	}
}

