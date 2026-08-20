package scan

import (
	"strings"
	"time"

	"watchman/proto/agentpb"
)

// scanIntrusion detects potential intrusion traces on Linux.
func (m *Manager) scanIntrusion(req *agentpb.ScanRequest) []Finding {
	var findings []Finding

	m.sendProgress(req.GetScanId(), 0.1, false, nil, "")

	// --- Suspicious Listening Ports ---
	listeningPorts := runBash(3*time.Second, "ss -tlnp 2>/dev/null || netstat -tlnp 2>/dev/null")
	for _, line := range strings.Split(listeningPorts, "\n") {
		if line == "" {
			continue
		}
		// Flag ports outside common ranges (non-standard high ports)
		if containsAny(line, ":6667", ":6666", ":4444", ":31337", ":12345", ":99", ":65500") {
			addIf(&findings, true, "port", "high", "检测到可疑监听端口",
				"发现可能是后门/木马常用端口: "+line,
				"检查该端口对应进程并关闭: 使用 lsof -i:<port> 查看进程")
		}
	}

	m.sendProgress(req.GetScanId(), 0.25, false, nil, "")

	// --- Suspicious Crontab Entries ---
	for _, user := range []string{"root", "www", "nginx", "nobody"} {
		cronEntries := runBash(2*time.Second, "crontab -u "+user+" -l 2>/dev/null")
		if containsAny(cronEntries, "wget", "curl http", "nc ", "ncat", "/dev/tcp", "bash -i", "python -c", "perl -e") {
			addIf(&findings, true, "cron", "critical", "用户 "+user+" crontab 含可疑条目",
				"用户 "+user+" 的 crontab 中包含可疑命令（反向shell/下载执行）: "+truncate(cronEntries, 200),
				"立即审查并清除: crontab -u "+user+" -e")
		}
	}

	// --- /tmp Suspicious Files ---
	tmpFiles := runBash(3*time.Second, "find /tmp /var/tmp -type f -executable 2>/dev/null | head -20")
	if tmpFiles != "" {
		addIf(&findings, true, "tmpfile", "medium", "/tmp 下存在可执行文件",
			"在 /tmp 或 /var/tmp 下发现可执行文件，可能是恶意程序:\n"+truncate(tmpFiles, 300),
			"检查这些文件来源，确认非必要则删除")
	}

	m.sendProgress(req.GetScanId(), 0.4, false, nil, "")

	// --- SUID Files (unusual locations) ---
	suidFiles := runBash(5*time.Second, "find / -perm -4000 -type f 2>/dev/null")
	suspiciousSUID := []string{}
	for _, f := range strings.Split(suidFiles, "\n") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// Standard SUID locations
		standardPaths := []string{"/usr/bin/su", "/usr/bin/sudo", "/usr/bin/passwd", "/usr/bin/chsh",
			"/usr/bin/chfn", "/usr/bin/newgrp", "/usr/bin/gpasswd", "/usr/bin/mount", "/usr/bin/umount",
			"/usr/lib", "/usr/sbin", "/bin/su", "/bin/mount", "/bin/umount"}
		isStandard := false
		for _, sp := range standardPaths {
			if strings.HasPrefix(f, sp) {
				isStandard = true
				break
			}
		}
		if !isStandard {
			suspiciousSUID = append(suspiciousSUID, f)
		}
	}
	if len(suspiciousSUID) > 0 {
		addIf(&findings, true, "suid", "high", "发现非标准 SUID 文件",
			"以下文件设置了 SUID 权限但不在标准路径，可能是权限提升后门:\n"+strings.Join(suspiciousSUID[:min(len(suspiciousSUID), 10)], "\n"),
			"审查这些文件来源，如非必要移除 SUID: chmod u-s <file>")
	}

	m.sendProgress(req.GetScanId(), 0.55, false, nil, "")

	// --- Login History (failed logins) ---
	authLog := runBash(3*time.Second, "grep 'Failed password' /var/log/auth.log 2>/dev/null | tail -20 || journalctl -u ssh 2>/dev/null | grep 'Failed' | tail -20")
	if authLog != "" {
		lineCount := strings.Count(authLog, "\n") + 1
		addIf(&findings, lineCount > 10, "login", "medium", "检测到多次 SSH 登录失败",
			"最近有 "+itoa(lineCount)+" 次失败的 SSH 登录尝试，可能为暴力破解:\n"+truncate(authLog, 300),
			"安装 fail2ban: apt install fail2ban; 检查攻击源IP并封禁")
	}

	// --- Recently Modified System Files ---
	recentMods := runBash(3*time.Second, "find /etc /usr/sbin /usr/bin -mtime -1 -type f 2>/dev/null | head -20")
	if recentMods != "" {
		addIf(&findings, true, "filemod", "medium", "24小时内系统目录文件被修改",
			"过去24小时内系统关键目录有文件被修改:\n"+truncate(recentMods, 300),
			"检查这些修改是否为预期操作，排查是否有未授权的系统文件篡改")
	}

	m.sendProgress(req.GetScanId(), 0.7, false, nil, "")

	// --- Suspicious Processes ---
	psOut := runBash(3*time.Second, "ps aux 2>/dev/null")
	for _, line := range strings.Split(psOut, "\n") {
		lower := strings.ToLower(line)
		if containsAny(lower, "nc -l", "ncat -l", "/dev/tcp", "bash -i", "python -c import", "perl -e socket") {
			addIf(&findings, true, "process", "critical", "检测到可疑进程（疑似反向Shell）",
				"发现疑似反向Shell进程: "+truncate(line, 150),
				"立即调查该进程: ps aux | grep <pid>; 如确认恶意则 kill -9 <pid>")
			break
		}
	}

	// --- .bash_history with dangerous commands ---
	bashHistory := runBash(2*time.Second, "cat /root/.bash_history 2>/dev/null | grep -iE 'rm -rf|wget|curl.*\\|.*sh|nc |nmap|masscan|hydra' | tail -10")
	if bashHistory != "" {
		addIf(&findings, true, "history", "low", "root 历史命令含危险操作",
			"root 的 .bash_history 中包含危险命令:\n"+truncate(bashHistory, 200),
			"审查这些历史命令是否为预期操作")
	}

	// --- Rootkit quick check (RKHunter-style heuristic) ---
	hiddenFiles := runBash(3*time.Second, "find / -name '.*' -type f 2>/dev/null | grep -vE '^/(proc|sys|dev)' | head -15")
	if containsAny(hiddenFiles, "/usr/.", "/tmp/.", "/var/.") {
		// Check for unusual hidden executables in system dirs
		hiddenExec := runBash(3*time.Second, "find /usr /opt -name '.*' -type f -executable 2>/dev/null | head -10")
		if hiddenExec != "" {
			addIf(&findings, true, "rootkit", "high", "发现隐藏的可执行文件",
				"在系统目录中发现隐藏的可执行文件，可能是 rootkit:\n"+hiddenExec,
				"使用 rkhunter 或 chkrootkit 进行完整扫描，确认后删除")
		}
	}

	return findings
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
