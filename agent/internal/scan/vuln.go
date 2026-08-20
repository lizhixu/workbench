package scan

import (
	"strings"
	"time"

	"watchman/proto/agentpb"
)

// scanVuln performs vulnerability scanning by checking installed software
// versions against known vulnerable versions.
func (m *Manager) scanVuln(req *agentpb.ScanRequest) []Finding {
	var findings []Finding

	m.sendProgress(req.GetScanId(), 0.1, false, nil, "")

	// --- OpenSSH Version ---
	sshVersion := runBash(2*time.Second, "ssh -V 2>&1")
	sshVer := extractVerNum(sshVersion)
	if sshVer != "" {
		// OpenSSH < 8.9 has multiple CVEs (e.g. CVE-2023-38408, regreSSHion CVE-2024-6387)
		if verLessThan(sshVer, 8, 9) {
			addIf(&findings, true, "ssh", "high", "OpenSSH 版本存在已知漏洞",
				"当前 OpenSSH 版本: "+sshVersion+"，低于 8.9p1，存在 CVE-2023-38408（SSH agent 注入）等已知漏洞。",
				"更新 OpenSSH: apt update && apt upgrade openssh-server; 或 yum update openssh")
		}
		// CVE-2024-6387 (regreSSHion) affects 8.5p1 - 9.7p1
		if verInRange(sshVer, 8, 5, 9, 7) {
			addIf(&findings, true, "ssh", "critical", "OpenSSH regreSSHion 漏洞 (CVE-2024-6387)",
				"当前 OpenSSH 版本 "+sshVersion+" 受 CVE-2024-6387 (regreSSHion) 影响，远程攻击者可能通过条件竞争实现 RCE。",
				"立即升级到 OpenSSH 9.8p1 或更高版本；或设置 LoginGraceTime 0 作为临时缓解")
		}
	}

	m.sendProgress(req.GetScanId(), 0.25, false, nil, "")

	// --- Kernel Version ---
	kernelVersion := runBash(2*time.Second, "uname -r")
	if kernelVersion != "" {
		// Known vulnerable kernel ranges (simplified)
		if kernelOutOfDate(kernelVersion) {
			addIf(&findings, true, "kernel", "medium", "内核版本可能存在已知漏洞",
				"当前内核 "+kernelVersion+" 版本较低，可能存在 Dirty Pipe (CVE-2022-0847) 等已知漏洞。",
				"更新内核到最新版本: apt update && apt upgrade linux-image-amd64")
		}
	}

	m.sendProgress(req.GetScanId(), 0.4, false, nil, "")

	// --- Docker Version ---
	dockerVersion := runBash(3*time.Second, "docker --version 2>/dev/null")
	if dockerVersion != "" {
		dockerVer := extractVerNum(dockerVersion)
		if dockerVer != "" && verLessThan(dockerVer, 24, 0) {
			addIf(&findings, true, "docker", "medium", "Docker 版本较低",
				"当前 Docker 版本: "+dockerVersion+"，低于 24.0，可能存在已知安全漏洞。",
				"更新 Docker: curl -fsSL https://get.docker.com | sh")
		}
	}

	m.sendProgress(req.GetScanId(), 0.55, false, nil, "")

	// --- Nginx Version ---
	nginxVersion := runBash(2*time.Second, "nginx -v 2>&1")
	if nginxVersion != "" {
		nginxVer := extractVerNum(nginxVersion)
		if nginxVer != "" && verLessThan(nginxVer, 1, 22) {
			addIf(&findings, true, "nginx", "medium", "Nginx 版本较低",
				"当前 Nginx 版本: "+nginxVersion+"，低于 1.22，可能存在已知漏洞。",
				"更新 Nginx: apt update && apt upgrade nginx")
		}
	}

	// --- MySQL/MariaDB Version ---
	mysqlVersion := runBash(3*time.Second, "mysql --version 2>/dev/null")
	if mysqlVersion != "" {
		mysqlVer := extractVerNum(mysqlVersion)
		if mysqlVer != "" && verLessThan(mysqlVer, 8, 0) {
			addIf(&findings, true, "database", "medium", "MySQL/MariaDB 版本较低",
				"当前版本: "+mysqlVersion+"，MySQL 5.7 以下已停止安全更新，存在已知漏洞。",
				"升级到 MySQL 8.0+ 或 MariaDB 10.5+")
		}
	}

	m.sendProgress(req.GetScanId(), 0.7, false, nil, "")

	// --- Redis Version ---
	redisVersion := runBash(2*time.Second, "redis-server --version 2>/dev/null")
	if redisVersion != "" {
		redisVer := extractVerNum(redisVersion)
		if redisVer != "" && verLessThan(redisVer, 6, 2) {
			addIf(&findings, true, "redis", "medium", "Redis 版本较低",
				"当前 Redis 版本: "+redisVersion+"，低于 6.2，可能存在已知漏洞。",
				"升级 Redis 到 6.2+ 或 7.x")
		}
		// Check if Redis is bound to all interfaces without auth
		redisConf := runBash(3*time.Second, "redis-cli CONFIG GET bind 2>/dev/null; redis-cli CONFIG GET requirepass 2>/dev/null")
		if containsAny(redisConf, "0.0.0.0") && !strings.Contains(redisConf, "requirepass") {
			addIf(&findings, true, "redis", "high", "Redis 绑定所有接口且无密码",
				"Redis 绑定 0.0.0.0 且未设置密码，可被远程未授权访问。",
				"在 redis.conf 中设置 bind 127.0.0.1 和 requirepass <strong-password>")
		}
	}

	m.sendProgress(req.GetScanId(), 0.85, false, nil, "")

	// --- OpenSSL Version ---
	opensslVersion := runBash(2*time.Second, "openssl version 2>/dev/null")
	if opensslVersion != "" {
		opensslVer := extractVerNum(opensslVersion)
		if opensslVer != "" && verLessThan(opensslVer, 1, 1) {
			addIf(&findings, true, "openssl", "high", "OpenSSL 版本较低",
				"当前 OpenSSL 版本: "+opensslVersion+"，低于 1.1.0，存在 Heartbleed 等已知漏洞。",
				"升级 OpenSSL 到 1.1.1 或 3.0+")
		}
	}

	// --- Package manager pending updates ---
	aptUpdates := runBash(5*time.Second, "apt list --upgradable 2>/dev/null | wc -l")
	if aptUpdates != "" {
		count := parseIntSafe(strings.TrimSpace(aptUpdates))
		if count > 50 {
			addIf(&findings, true, "packages", "medium", "大量软件包待更新",
				"有 "+itoa(count)+" 个软件包可升级，可能包含安全补丁。",
				"执行 apt update && apt upgrade 更新所有软件包")
		}
	}

	return findings
}

// extractVerNum extracts a version number like "8.4" or "1.25.3" from a string.
func extractVerNum(s string) string {
	// Find the first digit sequence that looks like x.y
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			start = i
			break
		}
	}
	if start == -1 {
		return ""
	}
	// Collect digits and dots, stop at first non-version char
	end := start
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' {
			end++
		} else {
			break
		}
	}
	return strings.Trim(s[start:end], ".")
}

// verLessThan returns true if version string s is less than major.minor.
func verLessThan(s string, major, minor int) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	m := parseIntSafe(parts[0])
	mi := parseIntSafe(parts[1])
	if m < major {
		return true
	}
	if m == major && mi < minor {
		return true
	}
	return false
}

// verInRange returns true if version string s is >= major.minor and <= major2.minor2.
func verInRange(s string, major, minor, major2, minor2 int) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	m := parseIntSafe(parts[0])
	mi := parseIntSafe(parts[1])
	if m < major || m > major2 {
		return false
	}
	if m == major && mi < minor {
		return false
	}
	if m == major2 && mi > minor2 {
		return false
	}
	return true
}

// scanWindows provides a reduced scan for Windows hosts.
func (m *Manager) scanWindows(req *agentpb.ScanRequest) []Finding {
	var findings []Finding

	m.sendProgress(req.GetScanId(), 0.1, false, nil, "")

	// Check Windows Firewall status
	firewallStatus := runBash(3*time.Second, "powershell -Command \"Get-NetFirewallProfile | Select-Object Name,Enabled | Format-Table -HideTableHeaders\"")
	for _, line := range strings.Split(firewallStatus, "\n") {
		lower := strings.ToLower(line)
		if containsAny(lower, "domain", "public", "private") && strings.Contains(lower, "false") {
			addIf(&findings, true, "firewall", "high", "Windows 防火墙配置文件未启用",
				"检测到防火墙配置文件未启用: "+strings.TrimSpace(line),
				"启用防火墙: Netsh advfirewall set allprofiles state on")
		}
	}

	m.sendProgress(req.GetScanId(), 0.4, false, nil, "")

	// Check for RDP enabled
	rdpStatus := runBash(3*time.Second, "powershell -Command \"(Get-ItemProperty -Path 'HKLM:\\System\\CurrentControlSet\\Control\\Terminal Server' -Name fDenyTSConnections).fDenyTSConnections\"")
	if rdpStatus == "0" {
		addIf(&findings, true, "rdp", "medium", "RDP 远程桌面已启用",
			"远程桌面协议(RDP)已启用，确保使用强密码+网络级认证(NLA)。",
			"确保 NLA 已启用，考虑限制 RDP 可访问的 IP 范围")
	}

	m.sendProgress(req.GetScanId(), 0.6, false, nil, "")

	// Check Windows Defender
	defenderStatus := runBash(3*time.Second, "powershell -Command \"Get-MpComputerStatus | Select-Object AMRunningMode | Format-Table -HideTableHeaders\"")
	if containsAny(defenderStatus, "not running", "disabled") {
		addIf(&findings, true, "antivirus", "medium", "Windows Defender 未运行",
			"Windows Defender 防病毒未运行或已禁用。",
			"启用 Windows Defender: powershell -Command Set-MpPreference -DisableRealtimeMonitoring $false")
	}

	// Check for recent failed logins (Event Log)
	failedLogins := runBash(5*time.Second, "powershell -Command \"Get-WinEvent -FilterHashtable @{LogName='Security';Id=4625} -MaxEvents 20 2>$null | Measure-Object | Select-Object -ExpandProperty Count\"")
	count := parseIntSafe(strings.TrimSpace(failedLogins))
	if count > 10 {
		addIf(&findings, true, "login", "medium", "检测到多次登录失败",
			"最近有 "+itoa(count)+" 次失败的登录尝试（事件ID 4625）。",
			"检查失败登录的来源IP，考虑启用账户锁定策略")
	}

	return findings
}
