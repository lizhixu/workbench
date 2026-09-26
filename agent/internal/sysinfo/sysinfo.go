// Package sysinfo implements system information collection on the agent:
// process list, listening ports, users, login history.
package sysinfo

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"watchman/proto/agentpb"
)

// Sender pushes agent->server messages.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Manager handles system info queries.
type Manager struct {
	mu     sync.Mutex
	sender Sender
	log    *slog.Logger
}

func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

func (m *Manager) SetSender(s Sender) {
	m.mu.Lock()
	m.sender = s
	m.mu.Unlock()
}

// Handle processes a SysInfoQuery from the server.
func (m *Manager) Handle(q *agentpb.SysInfoQuery) {
	var payload []byte
	var err error

	switch q.GetKind() {
	case "process":
		payload, err = m.processList()
	case "port":
		payload, err = m.portList()
	case "user":
		payload, err = m.userList()
	case "login":
		payload, err = m.loginHistory()
	case "osinfo":
		payload, err = m.osInfo()
	case "kill":
		payload, err = m.killProcess(q.GetPid(), q.GetForce())
	default:
		err = fmt.Errorf("unknown sysinfo kind: %s", q.GetKind())
	}

	if err != nil {
		m.send(q.GetKind(), []byte(fmt.Sprintf(`{"error":%q}`, err.Error())))
		return
	}
	m.send(q.GetKind(), payload)
}

func (m *Manager) send(kind string, payload []byte) {
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s != nil {
		s.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_Sysinfo{
				Sysinfo: &agentpb.SysInfoSnapshot{Kind: kind, JsonPayload: payload},
			},
		})
	}
}

// Process represents a single process entry.
type Process struct {
	PID     int32  `json:"pid"`
	Name    string `json:"name"`
	User    string `json:"user"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	Cmdline string `json:"cmdline"`
}

func (m *Manager) processList() ([]byte, error) {
	// Use gopsutil for cross-platform process info.
	return processListGopsutil()
}

// Port represents a listening port.
type Port struct {
	Proto   string `json:"proto"`
	Address string `json:"address"`
	Port    uint32 `json:"port"`
	State   string `json:"state"`
	PID     int32  `json:"pid"`
	Process string `json:"process"`
}

func (m *Manager) portList() ([]byte, error) {
	return portListGopsutil()
}

// killProcess terminates the given PID. It is the only mutating sysinfo
// operation; the server gates it by role and audits it before dispatch.
func (m *Manager) killProcess(pid int32, force bool) ([]byte, error) {
	m.log.Info("kill process requested", "pid", pid, "force", force)
	return killProcessGopsutil(pid, force)
}

// User represents a system user.
type User struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	GID      string `json:"gid"`
	Home     string `json:"home"`
	Shell    string `json:"shell"`
	LoginOK  bool   `json:"login_ok"`           // false if UID < 1000 and shell == nologin/false
	IsSystem bool   `json:"is_system"`          // UID < 1000 (service/system accounts)
}

func (m *Manager) userList() ([]byte, error) {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("net", "user").Output()
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"raw": string(out), "entries": []any{}})
	}
	var raw []byte
	out, err := exec.Command("getent", "passwd").Output()
	if err != nil {
		raw, err = exec.Command("cat", "/etc/passwd").Output()
		if err != nil {
			return nil, err
		}
	} else {
		raw = out
	}
	users := parsePasswd(string(raw))
	return json.Marshal(map[string]any{
		"entries": users,
		"raw":     string(raw),
	})
}

// parsePasswd extracts the structured columns of /etc/passwd (or getent
// passwd output). Lines starting with '#' (e.g. shadow placeholders) are
// ignored.
func parsePasswd(s string) []User {
	var out []User
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		uid := fields[2]
		var uidN int
		_, _ = fmt.Sscanf(uid, "%d", &uidN)
		shell := fields[6]
		loginOK := shell != "" && shell != "/usr/sbin/nologin" && shell != "/bin/false" && shell != "/sbin/nologin"
		out = append(out, User{
			Name:     fields[0],
			UID:      uid,
			GID:      fields[3],
			Home:     fields[5],
			Shell:    shell,
			LoginOK:  loginOK,
			IsSystem: uidN < 1000,
		})
	}
	return out
}

func (m *Manager) loginHistory() ([]byte, error) {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("query", "user").Output()
		if err != nil {
			return json.Marshal(map[string]string{"raw": ""})
		}
		return json.Marshal(map[string]string{"raw": string(out)})
	}
	out, err := exec.Command("last", "-F", "-n", "30").Output()
	if err != nil {
		// Fallback to non-F variant (older distributions without -F).
		out, err = exec.Command("last", "-n", "30").Output()
		if err != nil {
			return json.Marshal(map[string]any{"entries": []any{}, "raw": ""})
		}
	}
	return json.Marshal(map[string]any{
		"entries": parseLastOutput(string(out)),
		"raw":     string(out),
	})
}

// loginEntry is one parsed line from `last`. The `Type` distinguishes user
// sessions (User/Tty/From/Duration) from system events (reboot/shutdown).
type loginEntry struct {
	Type     string `json:"type"`     // "session" | "reboot" | "shutdown" | "crash" | "wtmp"
	User     string `json:"user,omitempty"`
	Tty      string `json:"tty,omitempty"`
	From     string `json:"from,omitempty"`     // remote host / "system boot"
	Started  string `json:"started,omitempty"`  // raw time string from `last`
	Ended    string `json:"ended,omitempty"`    // when logged out (or "- down"/"still logged in")
	Duration string `json:"duration,omitempty"` // like "(00:38)"
	Kernel   string `json:"kernel,omitempty"`   // kernel version for reboots
	Detail   string `json:"detail,omitempty"`   // trailing notes like "still logged in"
}

// parseLastOutput splits the textual output of `last` into structured rows.
// The format is fixed-width-ish but tolerates the variations `last` emits
// across distros (some columns can be absent for reboots/crashes).
func parseLastOutput(s string) []loginEntry {
	var out []loginEntry
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "wtmp begins") {
			out = append(out, loginEntry{Type: "wtmp", Detail: line})
			continue
		}
		if strings.HasPrefix(line, "btmp begins") {
			continue
		}
		// Tokens are whitespace-separated; first column is user (or
		// "reboot"/"shutdown"/"system boot").
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		first := fields[0]
		// Strip a trailing "still logged in" / "still running" / "gone - no logout"
		// note from the line so we can keep the timestamp pure. Only match when
		// the suffix is the last whitespace-delimited token (avoid eating the
		// trailing "down" of the literal word "shutdown").
		mainLine := line
		tailNote := ""
		tokens := strings.Fields(line)
		for _, suffix := range []string{"still logged in", "still running", "gone - no logout"} {
			if idx := strings.LastIndex(line, " "+suffix); idx > 0 && strings.HasSuffix(strings.TrimRight(line, " "), suffix) {
				mainLine = strings.TrimRight(line[:idx], " ")
				tailNote = suffix
				break
			}
		}
		// Bare trailing "down" (without "still"/"gone") is also a `last` status
		// token. It only counts if it's the very last field and the line doesn't
		// contain "shutdown" earlier.
		if tailNote == "" && len(tokens) > 1 && tokens[len(tokens)-1] == "down" {
			before := strings.TrimRight(line[:len(line)-len("down")], " ")
			if !strings.Contains(before, "shutdown") {
				mainLine = before
				tailNote = "down"
			}
		}
		mainFields := strings.Fields(mainLine)
		if len(mainFields) == 0 {
			continue
		}
		first = mainFields[0]

		switch first {
		case "reboot", "shutdown":
			entry := loginEntry{Type: first, Detail: tailNote}
			i := 1
			if i < len(mainFields) && mainFields[i] == "system" && i+1 < len(mainFields) && mainFields[i+1] == "boot" {
				i += 2
			}
			if i < len(mainFields) {
				entry.Kernel = mainFields[i]
				i++
			}
			entry.Started = joinFrom(mainFields, i)
			if tailNote == "" {
				entry.Detail = trailingNote(mainLine)
			}
			out = append(out, entry)
			continue
		}
		// User session.
		entry := loginEntry{Type: "session", User: first, Detail: tailNote}
		if len(mainFields) > 1 {
			entry.Tty = mainFields[1]
		}
		i := 2
		if i < len(mainFields) {
			entry.From = mainFields[i]
			i++
		}
		// Recognize "(HH:MM)" duration anywhere in the main line.
		for _, f := range mainFields {
			if strings.HasPrefix(f, "(") && strings.HasSuffix(f, ")") {
				entry.Duration = f
				break
			}
		}
		// The "started" timestamp is the weekday..time slice at the end of
		// the main line (after we strip the from/login columns).
		if len(mainFields) >= 5 {
			entry.Started = joinFrom(mainFields, 4)
		}
		out = append(out, entry)
	}
	return out
}

func isDayName(s string) bool {
	switch s {
	case "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun":
		return true
	}
	return false
}

// joinFrom rebuilds the original substring starting at index i
// (whitespace-joined).
func joinFrom(parts []string, i int) string {
	if i >= len(parts) {
		return ""
	}
	return strings.Join(parts[i:], " ")
}

// trailingNote returns the part of the line that follows the duration
// parens, if any. Used as a fallback when no known suffix was found.
func trailingNote(line string) string {
	if i := strings.LastIndex(line, ")"); i >= 0 {
		return strings.TrimSpace(line[i+1:])
	}
	return strings.TrimSpace(line)
}

func (m *Manager) osInfo() ([]byte, error) {
	return osInfoGopsutil()
}