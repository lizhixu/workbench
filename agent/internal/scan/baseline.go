package scan

import (
	"strings"
	"time"

	"watchman/proto/agentpb"
)

// scanBaseline performs security baseline checks on Linux.
func (m *Manager) scanBaseline(req *agentpb.ScanRequest) []Finding {
	var findings []Finding

	m.sendProgress(req.GetScanId(), 0.1, false, nil, "")

	// --- SSH Configuration ---
	if fileExists("/etc/ssh/sshd_config") {
		sshdConfig := runBash(3*time.Second, "cat /etc/ssh/sshd_config 2>/dev/null")

		addIf(&findings, strings.Contains(sshdConfig, "PermitRootLogin yes") || containsAny(sshdConfig, "PermitRootLogin without-password", "PermitRootLogin prohibit-password"),
			"ssh", "high", "SSH 允许 root 登录",
			"sshd_config 中 PermitRootLogin 配置允许 root 直接登录，增加被暴力破解的风险。",
			"编辑 /etc/ssh/sshd_config，设置 PermitRootLogin no，然后 systemctl restart sshd")

		addIf(&findings, !strings.Contains(sshdConfig, "PasswordAuthentication no"),
			"ssh", "medium", "SSH 允许密码认证",
			"sshd_config 未禁用密码认证，存在暴力破解风险。",
			"设置 PasswordAuthentication no 并配置密钥认证，然后 systemctl restart sshd")

		addIf(&findings, strings.Contains(sshdConfig, "PermitEmptyPasswords yes"),
			"ssh", "critical", "SSH 允许空密码登录",
			"sshd_config 中 PermitEmptyPasswords yes，极其危险。",
			"立即设置 PermitEmptyPasswords no 并重启 sshd")
	}

	m.sendProgress(req.GetScanId(), 0.25, false, nil, "")

	// --- Firewall ---
	ufwStatus := runBash(3*time.Second, "ufw status 2>/dev/null")
	firewalldStatus := runBash(3*time.Second, "firewall-cmd --state 2>/dev/null")
	iptablesRules := runBash(3*time.Second, "iptables -L -n 2>/dev/null | wc -l")

	addIf(&findings, !strings.Contains(ufwStatus, "active") && firewalldStatus == "" && (iptablesRules == "0" || iptablesRules == "" || iptablesRules == "3"),
		"firewall", "high", "防火墙未启用",
			"系统未检测到活跃的防火墙（ufw/firewalld/iptables 均未配置规则），所有端口对外开放。",
			"启用 ufw: ufw default deny incoming && ufw enable; 或启用 firewalld: systemctl start firewalld")

	m.sendProgress(req.GetScanId(), 0.4, false, nil, "")

	// --- File Permissions ---
	passwdPerms := runBash(2*time.Second, "stat -c '%a' /etc/passwd 2>/dev/null")
	shadowPerms := runBash(2*time.Second, "stat -c '%a' /etc/shadow 2>/dev/null")

	addIf(&findings, passwdPerms != "644" && passwdPerms != "",
		"fileperm", "medium", "/etc/passwd 权限异常",
			"/etc/passwd 权限为 "+passwdPerms+"，建议为 644。",
			"chmod 644 /etc/passwd")

	addIf(&findings, shadowPerms != "640" && shadowPerms != "000" && shadowPerms != "" && shadowPerms != "640",
		"fileperm", "high", "/etc/shadow 权限异常",
			"/etc/shadow 权限为 "+shadowPerms+"，应为 640 或 000。",
			"chmod 640 /etc/shadow")

	m.sendProgress(req.GetScanId(), 0.55, false, nil, "")

	// --- Password Policy ---
	loginDefs := runBash(2*time.Second, "cat /etc/login.defs 2>/dev/null")
	addIf(&findings, !containsMinLen(loginDefs, 8),
		"password", "medium", "密码最小长度未设置或过短",
		"/etc/login.defs 中 PASS_MIN_LEN 未设置或小于 8。",
		"设置 PASS_MIN_LEN 8 或更高，编辑 /etc/login.defs")

	addIf(&findings, !strings.Contains(loginDefs, "PASS_MIN_DAYS") || hasMinDaysZero(loginDefs),
		"password", "low", "密码最短使用期限未限制",
		"/etc/login.defs 中 PASS_MIN_DAYS 为 0，允许随时修改密码。",
		"设置 PASS_MIN_DAYS 1 或更高")

	m.sendProgress(req.GetScanId(), 0.7, false, nil, "")

	// --- Sudo Configuration ---
	sudoers := runBash(3*time.Second, "cat /etc/sudoers 2>/dev/null")
	sudoersD := runBash(3*time.Second, "ls /etc/sudoers.d/ 2>/dev/null")

	addIf(&findings, strings.Contains(sudoers, "NOPASSWD"),
		"sudo", "medium", "sudo 配置中存在 NOPASSWD",
		"检测到 sudoers 配置中有 NOPASSWD 选项，可能导致权限提升。",
		"移除 NOPASSWD 配置，使用 sudo 时应要求密码验证")

	addIf(&findings, strings.Contains(sudoers, "ALL=(ALL) ALL") && strings.Contains(sudoersD, ""),
		"sudo", "low", "存在多个 sudo 授权",
		"检测到 sudoers 和 /etc/sudoers.d 中均有配置，建议定期审查。",
		"审查 /etc/sudoers 和 /etc/sudoers.d/ 中的授权")

	// --- Cron ---
	cronRoot := runBash(2*time.Second, "crontab -l 2>/dev/null")
	addIf(&findings, containsAny(cronRoot, "wget", "curl", "rm -rf", "chmod 777"),
		"cron", "high", "root crontab 包含可疑命令",
		"root 的 crontab 中包含可疑命令: "+truncate(cronRoot, 200),
		"审查 root crontab: crontab -l，移除不必要的条目")

	// --- Kernel Security ---
	kernel := runBash(2*time.Second, "uname -r")
	addIf(&findings, kernelOutOfDate(kernel),
		"kernel", "medium", "内核版本可能存在已知漏洞",
		"当前内核版本: "+kernel+"，建议保持内核更新以修复已知安全漏洞。",
		"更新内核: apt update && apt upgrade linux-image-amd64; 或 yum update kernel")

	return findings
}

func containsMinLen(loginDefs string, min int) bool {
	for _, line := range strings.Split(loginDefs, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PASS_MIN_LEN") {
			parts := strings.Fields(line)
			if len(parts) >= 2 && parseIntSafe(parts[1]) >= min {
				return true
			}
		}
	}
	return false
}

func hasMinDaysZero(loginDefs string) bool {
	for _, line := range strings.Split(loginDefs, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PASS_MIN_DAYS") {
			parts := strings.Fields(line)
			if len(parts) >= 2 && parts[1] == "0" {
				return true
			}
		}
	}
	return false
}

func parseIntSafe(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// Simple kernel version check: if kernel < 5.4, flag it.
func kernelOutOfDate(ver string) bool {
	if ver == "" {
		return false
	}
	parts := strings.Split(ver, ".")
	if len(parts) < 2 {
		return false
	}
	major := parseIntSafe(parts[0])
	minor := parseIntSafe(parts[1])
	if major < 5 {
		return true
	}
	if major == 5 && minor < 4 {
		return true
	}
	return false
}
