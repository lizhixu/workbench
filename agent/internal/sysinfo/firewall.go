// Firewall control on the agent (Linux v1): detect the active backend in
// ufw -> firewalld -> iptables order, list rules, and apply allow/deny/
// delete with fixed argv only — never through a shell. Windows returns
// supported:false; enabling a firewall is out of scope for v1 (ufw enable
// with a default-deny policy would cut the operator's own SSH session), so
// a detected-but-inactive backend only reports its state plus a manual
// hint and refuses mutations.
package sysinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// FirewallRule is one backend rule: a port/protocol pair with an action.
type FirewallRule struct {
	Port     int32  `json:"port"`
	Protocol string `json:"protocol"` // tcp | udp
	Action   string `json:"action"`   // allow | deny
}

// FirewallStatus is the kind="firewall" payload.
type FirewallStatus struct {
	Supported bool           `json:"supported"`
	Backend   string         `json:"backend"` // ufw | firewalld | iptables | none
	Enabled   bool           `json:"enabled"`
	Rules     []FirewallRule `json:"rules"`
	Hint      string         `json:"hint,omitempty"`
}

const firewallCmdTimeout = 10 * time.Second

// firewallOpTimeout bounds one whole firewall operation (detect + list +
// apply/delete) on the agent. The server gives up on a sysinfo call after
// 10 seconds, so the agent must answer inside that window, leaving ~2s for
// the round-trip: on a hung backend command the operation fails fast with
// a clear timeout instead of landing the rule after the panel already
// reported a timeout.
const firewallOpTimeout = 8 * time.Second

// firewallOpErr rewrites an error raised while the operation deadline has
// passed into a plain timeout message; anything else passes through.
func firewallOpErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("防火墙操作超时（8 秒），请稍后重试")
	}
	return err
}

// runFirewall runs one fixed argv with a timeout and returns its stdout.
// stderr is kept in the error so a failed backend command explains itself.
func runFirewall(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return string(out), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

func validFirewallPort(port int32) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("端口须在 1~65535 之间: %d", port)
	}
	return nil
}

func validFirewallProtocol(protocol string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "tcp":
		return "tcp", nil
	case "udp":
		return "udp", nil
	default:
		return "", fmt.Errorf("未知协议: %s（仅支持 tcp/udp）", protocol)
	}
}

// firewallStatus detects the backend and lists its rules. Non-Linux hosts
// report supported:false instead of erroring, so the panel can render the
// "not supported" state.
func (m *Manager) firewallStatus() ([]byte, error) {
	if runtime.GOOS == "windows" {
		return json.Marshal(FirewallStatus{Supported: false, Backend: "none", Hint: "防火墙端口控制仅支持 Linux 主机"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), firewallOpTimeout)
	defer cancel()
	backend, enabled, hint := detectFirewallBackend(ctx)
	if backend == "none" {
		return json.Marshal(FirewallStatus{Supported: false, Backend: "none", Hint: hint})
	}
	rules, err := listFirewallRules(ctx, backend)
	if err != nil {
		return nil, firewallOpErr(ctx, err)
	}
	if rules == nil {
		rules = []FirewallRule{}
	}
	return json.Marshal(FirewallStatus{Supported: true, Backend: backend, Enabled: enabled, Rules: rules, Hint: hint})
}

// detectFirewallBackend probes ufw, then firewalld, then iptables. It returns
// the backend name, whether it is currently enforcing, and a hint for the
// manual-enable path when a backend exists but is inactive.
func detectFirewallBackend(ctx context.Context) (backend string, enabled bool, hint string) {
	ctx, cancel := context.WithTimeout(ctx, firewallCmdTimeout)
	defer cancel()
	if out, err := runFirewall(ctx, "ufw", "status"); err == nil {
		if strings.Contains(out, "Status: active") {
			return "ufw", true, ""
		}
		return "ufw", false, "ufw 已安装但未启用，请在主机上手动执行 ufw enable（注意先放行 SSH 端口）"
	}
	if out, err := runFirewall(ctx, "firewall-cmd", "--state"); err == nil && strings.TrimSpace(out) == "running" {
		return "firewalld", true, ""
	}
	if _, err := exec.LookPath("firewall-cmd"); err == nil {
		return "firewalld", false, "firewalld 已安装但未运行，请在主机上手动执行 systemctl start firewalld"
	}
	if _, err := runFirewall(ctx, "iptables", "-S", "INPUT"); err == nil {
		return "iptables", true, ""
	}
	return "none", false, "未检测到防火墙后端（ufw/firewalld/iptables），请先在主机上安装并启用防火墙"
}

func listFirewallRules(ctx context.Context, backend string) ([]FirewallRule, error) {
	ctx, cancel := context.WithTimeout(ctx, firewallCmdTimeout)
	defer cancel()
	switch backend {
	case "ufw":
		out, err := runFirewall(ctx, "ufw", "status")
		if err != nil {
			return nil, err
		}
		return parseUfwStatus(out), nil
	case "firewalld":
		rules := []FirewallRule{}
		if out, err := runFirewall(ctx, "firewall-cmd", "--list-ports"); err == nil {
			rules = append(rules, parseFirewalldPorts(out)...)
		} else {
			return nil, err
		}
		if out, err := runFirewall(ctx, "firewall-cmd", "--list-rich-rules"); err == nil {
			rules = append(rules, parseFirewalldRichRules(out)...)
		}
		return dedupeFirewallRules(rules), nil
	case "iptables":
		out, err := runFirewall(ctx, "iptables", "-S", "INPUT")
		if err != nil {
			return nil, err
		}
		return parseIptablesRules(out), nil
	default:
		return nil, fmt.Errorf("未知防火墙后端: %s", backend)
	}
}

// parseUfwStatus parses `ufw status` output. Lines look like
// "22/tcp  ALLOW IN  Anywhere" (v6 duplicates carry a "(v6)" suffix and are
// deduped). Header lines and the Status line are skipped. Outbound rules
// ("ALLOW OUT") are out of scope for v1 — the panel only manages inbound
// rules, and `ufw delete allow` would not match an OUT rule anyway.
func parseUfwStatus(out string) []FirewallRule {
	rules := []FirewallRule{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		port, proto, ok := splitPortProto(fields[0])
		if !ok {
			continue
		}
		var action string
		switch fields[1] {
		case "ALLOW":
			action = "allow"
		case "DENY", "REJECT":
			action = "deny"
		default:
			continue
		}
		if len(fields) > 2 && fields[2] == "OUT" {
			continue
		}
		rules = append(rules, FirewallRule{Port: port, Protocol: proto, Action: action})
	}
	return dedupeFirewallRules(rules)
}

// parseFirewalldPorts parses `firewall-cmd --list-ports` output
// ("8080/tcp 9090/tcp"): listed ports are allowed.
func parseFirewalldPorts(out string) []FirewallRule {
	rules := []FirewallRule{}
	for _, f := range strings.Fields(out) {
		port, proto, ok := splitPortProto(f)
		if !ok {
			continue
		}
		rules = append(rules, FirewallRule{Port: port, Protocol: proto, Action: "allow"})
	}
	return rules
}

// parseFirewalldRichRules parses `firewall-cmd --list-rich-rules` output.
// Only port rules with an accept/reject/drop verdict are understood, e.g.
// `rule family="ipv4" port port="8080" protocol="tcp" reject`.
func parseFirewalldRichRules(out string) []FirewallRule {
	rules := []FirewallRule{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "port ") {
			continue
		}
		fields := strings.Fields(line)
		var portStr, proto, action string
		for _, f := range fields {
			if strings.HasPrefix(f, "port=") {
				portStr = strings.Trim(strings.TrimPrefix(f, "port="), `"`)
			}
			if strings.HasPrefix(f, "protocol=") {
				proto = strings.Trim(strings.TrimPrefix(f, "protocol="), `"`)
			}
			switch f {
			case "accept":
				action = "allow"
			case "reject", "drop":
				action = "deny"
			}
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 || proto == "" || action == "" {
			continue
		}
		rules = append(rules, FirewallRule{Port: int32(port), Protocol: proto, Action: action})
	}
	return rules
}

// parseIptablesRules parses `iptables -S INPUT` output. Only simple
// per-port rules are understood, e.g.
// "-A INPUT -p tcp -m tcp --dport 8080 -j ACCEPT".
func parseIptablesRules(out string) []FirewallRule {
	rules := []FirewallRule{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "-A" {
			continue
		}
		var proto, dport, verdict string
		for i := 0; i < len(fields); i++ {
			switch fields[i] {
			case "-p":
				if i+1 < len(fields) {
					proto = fields[i+1]
				}
			case "--dport":
				if i+1 < len(fields) {
					dport = fields[i+1]
				}
			case "-j":
				if i+1 < len(fields) {
					verdict = fields[i+1]
				}
			}
		}
		var action string
		switch verdict {
		case "ACCEPT":
			action = "allow"
		case "DROP", "REJECT":
			action = "deny"
		default:
			continue
		}
		port, err := strconv.Atoi(dport)
		if err != nil || port < 1 || port > 65535 || (proto != "tcp" && proto != "udp") {
			continue
		}
		rules = append(rules, FirewallRule{Port: int32(port), Protocol: proto, Action: action})
	}
	return dedupeFirewallRules(rules)
}

// splitPortProto splits "8080/tcp" into port 8080 and protocol "tcp".
func splitPortProto(s string) (int32, string, bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	port, err := strconv.Atoi(parts[0])
	if err != nil || port < 1 || port > 65535 {
		return 0, "", false
	}
	if parts[1] != "tcp" && parts[1] != "udp" {
		return 0, "", false
	}
	return int32(port), parts[1], true
}

func dedupeFirewallRules(rules []FirewallRule) []FirewallRule {
	seen := map[FirewallRule]bool{}
	out := []FirewallRule{}
	for _, r := range rules {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// pickFirewallTargets splits the stored rules for one port/protocol pair
// into same-action and opposite-action rules. Pure, so the conflict logic
// is unit-testable without a firewall backend.
func pickFirewallTargets(rules []FirewallRule, port int32, proto, action string) (same, opposite []FirewallRule) {
	for _, r := range rules {
		if r.Port != port || r.Protocol != proto {
			continue
		}
		if r.Action == action {
			same = append(same, r)
		} else {
			opposite = append(opposite, r)
		}
	}
	return same, opposite
}

// firewallChange applies an allow/deny rule. Re-applying the same rule is a
// success (idempotent): the backend either says so itself or the pre-check
// below short-circuits. Mutations are refused while the detected backend is
// inactive — a rule recorded against a stopped firewall would not be
// enforced, and v1 deliberately has no "enable firewall" step.
//
// Allow and deny are mutually exclusive per port/protocol: ufw and
// firewalld both evaluate first match, so leaving an opposite rule in place
// and appending the new one would report success while the port stays in
// its old state. The opposite rule is therefore removed first, and the end
// state is exactly one rule carrying the requested action.
func (m *Manager) firewallChange(action string, port int32, protocol string) ([]byte, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("防火墙端口控制仅支持 Linux 主机")
	}
	if err := validFirewallPort(port); err != nil {
		return nil, err
	}
	if port == 9090 {
		return nil, fmt.Errorf("9090 为 Agent 回连端口，禁止操作")
	}
	proto, err := validFirewallProtocol(protocol)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), firewallOpTimeout)
	defer cancel()
	backend, enabled, _ := detectFirewallBackend(ctx)
	if backend == "none" {
		return nil, fmt.Errorf("未检测到防火墙后端（ufw/firewalld/iptables）")
	}
	if !enabled {
		return nil, fmt.Errorf("防火墙后端 %s 未启用，请先在主机上手动启用", backend)
	}
	rules, err := listFirewallRules(ctx, backend)
	if err != nil {
		return nil, firewallOpErr(ctx, err)
	}
	same, opposite := pickFirewallTargets(rules, port, proto, action)
	for _, r := range opposite {
		if err := deleteFirewallRule(ctx, backend, r.Action, port, proto); err != nil {
			return nil, firewallOpErr(ctx, err)
		}
	}
	already := len(same) > 0
	if !already {
		if err := applyFirewallRule(ctx, backend, action, port, proto); err != nil {
			return nil, firewallOpErr(ctx, err)
		}
	}
	m.log.Info("firewall rule applied", "backend", backend, "action", action, "port", port, "protocol", proto, "already", already)
	return json.Marshal(map[string]any{
		"ok": true, "action": action, "port": port, "protocol": proto, "already": already, "backend": backend,
	})
}

// firewallDelete removes every rule for a port/protocol pair, whichever
// action each carries, so the port is left with no rule at all.
func (m *Manager) firewallDelete(port int32, protocol string) ([]byte, error) {
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("防火墙端口控制仅支持 Linux 主机")
	}
	if err := validFirewallPort(port); err != nil {
		return nil, err
	}
	if port == 9090 {
		return nil, fmt.Errorf("9090 为 Agent 回连端口，禁止操作")
	}
	proto, err := validFirewallProtocol(protocol)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), firewallOpTimeout)
	defer cancel()
	backend, enabled, _ := detectFirewallBackend(ctx)
	if backend == "none" {
		return nil, fmt.Errorf("未检测到防火墙后端（ufw/firewalld/iptables）")
	}
	if !enabled {
		return nil, fmt.Errorf("防火墙后端 %s 未启用，请先在主机上手动启用", backend)
	}
	rules, err := listFirewallRules(ctx, backend)
	if err != nil {
		return nil, firewallOpErr(ctx, err)
	}
	targets := []FirewallRule{}
	for _, r := range rules {
		if r.Port == port && r.Protocol == proto {
			targets = append(targets, r)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("未找到端口 %d/%s 的防火墙规则", port, proto)
	}
	for _, t := range targets {
		if err := deleteFirewallRule(ctx, backend, t.Action, port, proto); err != nil {
			return nil, firewallOpErr(ctx, err)
		}
	}
	m.log.Info("firewall rules deleted", "backend", backend, "count", len(targets), "port", port, "protocol", proto)
	return json.Marshal(map[string]any{
		"ok": true, "action": targets[0].Action, "port": port, "protocol": proto, "deleted": len(targets), "backend": backend,
	})
}

// applyFirewallRule runs the backend's fixed argv for allow/deny.
func applyFirewallRule(ctx context.Context, backend, action string, port int32, proto string) error {
	ctx, cancel := context.WithTimeout(ctx, firewallCmdTimeout)
	defer cancel()
	spec := fmt.Sprintf("%d/%s", port, proto)
	switch backend {
	case "ufw":
		verb := "allow"
		if action == "deny" {
			verb = "deny"
		}
		_, err := runFirewall(ctx, "ufw", verb, spec)
		return err
	case "firewalld":
		if action == "allow" {
			if _, err := runFirewall(ctx, "firewall-cmd", "--permanent", "--add-port="+spec); err != nil {
				return err
			}
		} else {
			rule := fmt.Sprintf(`rule family="ipv4" port port="%d" protocol="%s" reject`, port, proto)
			if _, err := runFirewall(ctx, "firewall-cmd", "--permanent", "--add-rich-rule="+rule); err != nil {
				return err
			}
		}
		_, err := runFirewall(ctx, "firewall-cmd", "--reload")
		return err
	case "iptables":
		verdict := "ACCEPT"
		if action == "deny" {
			verdict = "DROP"
		}
		// -C exits 0 when the exact rule already exists (idempotent path for
		// a rule added outside the panel); otherwise insert at the head.
		if _, err := runFirewall(ctx, "iptables", "-C", "INPUT", "-p", proto, "-m", proto, "--dport", strconv.Itoa(int(port)), "-j", verdict); err == nil {
			return nil
		}
		_, err := runFirewall(ctx, "iptables", "-I", "INPUT", "-p", proto, "-m", proto, "--dport", strconv.Itoa(int(port)), "-j", verdict)
		return err
	default:
		return fmt.Errorf("未知防火墙后端: %s", backend)
	}
}

// deleteFirewallRule runs the backend's fixed argv to remove a rule.
func deleteFirewallRule(ctx context.Context, backend, action string, port int32, proto string) error {
	ctx, cancel := context.WithTimeout(ctx, firewallCmdTimeout)
	defer cancel()
	spec := fmt.Sprintf("%d/%s", port, proto)
	switch backend {
	case "ufw":
		verb := "allow"
		if action == "deny" {
			verb = "deny"
		}
		// ufw delete asks "Proceed with operation (y|n)?"; answer on stdin.
		cmd := exec.CommandContext(ctx, "ufw", "delete", verb, spec)
		cmd.Stdin = strings.NewReader("y\n")
		if out, err := cmd.Output(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				return fmt.Errorf("ufw delete %s %s: %s", verb, spec, strings.TrimSpace(string(ee.Stderr)))
			}
			return fmt.Errorf("ufw delete %s %s: %w (output: %s)", verb, spec, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "firewalld":
		var err error
		if action == "allow" {
			_, err = runFirewall(ctx, "firewall-cmd", "--permanent", "--remove-port="+spec)
		} else {
			rule := fmt.Sprintf(`rule family="ipv4" port port="%d" protocol="%s" reject`, port, proto)
			_, err = runFirewall(ctx, "firewall-cmd", "--permanent", "--remove-rich-rule="+rule)
		}
		if err != nil {
			return err
		}
		_, err = runFirewall(ctx, "firewall-cmd", "--reload")
		return err
	case "iptables":
		verdict := "ACCEPT"
		if action == "deny" {
			verdict = "DROP"
		}
		_, err := runFirewall(ctx, "iptables", "-D", "INPUT", "-p", proto, "-m", proto, "--dport", strconv.Itoa(int(port)), "-j", verdict)
		return err
	default:
		return fmt.Errorf("未知防火墙后端: %s", backend)
	}
}
