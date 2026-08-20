// Package docker implements Docker container/image management on the agent
// by shelling out to the docker CLI.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"watchman/proto/agentpb"
)

// Sender pushes agent->server messages.
type Sender interface {
	Send(msg *agentpb.AgentMessage) bool
}

// Manager handles Docker operations.
type Manager struct {
	mu     sync.Mutex
	sender Sender
	log    *slog.Logger

	// dockerAvailable is cached to avoid re-running LookPath on every op.
	dockerChecked bool
	dockerOk      bool
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

// Handle processes a DockerOp from the server.
func (m *Manager) Handle(op *agentpb.DockerOp) {
	go m.run(op)
}

// dockerAvailable checks once (cached) whether the docker CLI is on PATH.
func (m *Manager) dockerAvailable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dockerChecked {
		_, err := exec.LookPath("docker")
		m.dockerOk = err == nil
		m.dockerChecked = true
	}
	return m.dockerOk
}

func (m *Manager) run(op *agentpb.DockerOp) {
	result := &agentpb.DockerEvent{OpId: op.GetOpId()}

	if !m.dockerAvailable() {
		result.Ok = false
		result.Error = "docker not installed or not in PATH"
		m.send(result)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout(op.GetOp()))
	defer cancel()

	// ps_images is a compound op that runs `docker ps` and `docker images` in
	// parallel and returns a single JSON object {containers:[...], images:[...]}.
	// This collapses two round trips into one for the dashboard.
	if op.GetOp() == "ps_images" {
		m.runPsImages(ctx, result)
		m.send(result)
		return
	}

	args, ok := dockerArgs(op.GetOp(), op.GetContainer(), op.GetImage())
	if !ok {
		result.Ok = false
		result.Error = fmt.Sprintf("unknown docker op: %s", op.GetOp())
		m.send(result)
		return
	}

	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		// `Output()` only captures stdout; pull the stderr separately for the
		// error message so the user sees what the CLI complained about.
		errOut := ""
		if ee, ok := err.(*exec.ExitError); ok {
			errOut = string(ee.Stderr)
		}
		result.Ok = false
		result.Error = err.Error() + ": " + errOut
	} else {
		result.Ok = true
		result.PayloadJson = out
	}

	m.send(result)
}

// runPsImages runs `docker ps` and `docker images` concurrently and merges
// their NDJSON output into a single JSON document.
func (m *Manager) runPsImages(ctx context.Context, result *agentpb.DockerEvent) {
	type psImages struct {
		Containers []any `json:"containers"`
		Images     []any `json:"images"`
	}

	var (
		cOut, iOut   []byte
		cErr, iErr   error
		wg           sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		cOut, cErr = exec.CommandContext(ctx, "docker", "ps", "--format", "{{json .}}").Output()
	}()
	go func() {
		defer wg.Done()
		iOut, iErr = exec.CommandContext(ctx, "docker", "images", "--format", "{{json .}}").Output()
	}()
	wg.Wait()

	if cErr != nil {
		result.Ok = false
		result.Error = "docker ps: " + cErr.Error()
		return
	}
	if iErr != nil {
		result.Ok = false
		result.Error = "docker images: " + iErr.Error()
		return
	}

	merged := psImages{
		Containers: parseNDJSONAny(cOut),
		Images:     parseNDJSONAny(iOut),
	}
	payload, err := json.Marshal(merged)
	if err != nil {
		result.Ok = false
		result.Error = "marshal ps_images: " + err.Error()
		return
	}
	result.Ok = true
	result.PayloadJson = payload
}

// dockerArgs maps an op to its `docker` CLI argv.
func dockerArgs(op, container, image string) ([]string, bool) {
	switch op {
	case "ps":
		return []string{"ps", "--format", "{{json .}}"}, true
	case "images":
		return []string{"images", "--format", "{{json .}}"}, true
	case "start":
		return []string{"start", container}, true
	case "stop":
		return []string{"stop", container}, true
	case "restart":
		return []string{"restart", container}, true
	case "rm":
		return []string{"rm", "-f", container}, true
	case "logs":
		return []string{"logs", "--tail", "200", container}, true
	case "inspect":
		return []string{"inspect", container}, true
	case "remove_image":
		return []string{"rmi", image}, true
	case "prune_images":
		return []string{"image", "prune", "-f"}, true
	case "pull":
		return []string{"pull", image}, true
	}
	return nil, false
}

func (m *Manager) send(ev *agentpb.DockerEvent) {
	m.mu.Lock()
	s := m.sender
	m.mu.Unlock()
	if s != nil {
		s.Send(&agentpb.AgentMessage{
			Payload: &agentpb.AgentMessage_Docker{Docker: ev},
		})
	}
}

// opTimeout returns the command timeout for a given docker op. Image pulls can
// take minutes, so they get a longer budget than the default.
func opTimeout(op string) time.Duration {
	if op == "pull" {
		return 5 * time.Minute
	}
	if op == "ps_images" {
		return 30 * time.Second
	}
	return 30 * time.Second
}

// parseNDJSONAny parses newline-delimited JSON into []any, skipping blank or
// unparseable lines (unlike the server-side all-or-nothing parser, this one
// is tolerant so a stray warning line doesn't nuke the whole list).
func parseNDJSONAny(raw []byte) []any {
	var rows []any
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var v any
		if json.Unmarshal(line, &v) == nil {
			rows = append(rows, v)
		}
	}
	return rows
}

// ContainerInfo is a typed subset of `docker ps --format {{json .}}`.
type ContainerInfo struct {
	ID     string `json:"ID"`
	Image  string `json:"Image"`
	Name   string `json:"Names"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
}

// ParseContainers parses `docker ps --format {{json .}}` output lines.
func ParseContainers(raw []byte) []ContainerInfo {
	var list []ContainerInfo
	for _, line := range splitLines(raw) {
		if len(line) == 0 {
			continue
		}
		var c ContainerInfo
		if err := json.Unmarshal(line, &c); err == nil {
			list = append(list, c)
		}
	}
	return list
}

func splitLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}