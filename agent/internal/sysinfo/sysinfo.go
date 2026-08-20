// Package sysinfo implements system information collection on the agent:
// process list, listening ports, users, login history.
package sysinfo

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
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

// User represents a system user.
type User struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
	GID  string `json:"gid"`
	Home string `json:"home"`
	Shell string `json:"shell"`
}

func (m *Manager) userList() ([]byte, error) {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("net", "user").Output()
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"raw": string(out)})
	}
	out, err := exec.Command("getent", "passwd").Output()
	if err != nil {
		// fallback to /etc/passwd
		return exec.Command("cat", "/etc/passwd").Output()
	}
	return json.Marshal(map[string]string{"raw": string(out)})
}

func (m *Manager) loginHistory() ([]byte, error) {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("query", "user").Output()
		if err != nil {
			return json.Marshal(map[string]string{"raw": ""})
		}
		return json.Marshal(map[string]string{"raw": string(out)})
	}
	out, err := exec.Command("last", "-n", "20").Output()
	if err != nil {
		return json.Marshal(map[string]string{"raw": ""})
	}
	return json.Marshal(map[string]string{"raw": string(out)})
}

func (m *Manager) osInfo() ([]byte, error) {
	return osInfoGopsutil()
}