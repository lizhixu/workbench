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
	"regexp"
	"strconv"
	"strings"
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

	// The docker CLI lookup is cached so it does not run on every op. A
	// positive result is kept for good; a negative one expires, because the
	// console can install Docker on a running host (AGENTS.md 3.4) and a
	// permanently cached "missing" would keep reporting it absent until the
	// agent restarted.
	dockerOk        bool
	dockerCheckedAt time.Time

	// lookPath is swappable in tests.
	lookPath func(string) (string, error)
}

// negativeLookupTTL is how long a "docker is missing" answer stays cached.
const negativeLookupTTL = 20 * time.Second

func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log, lookPath: exec.LookPath}
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

// dockerAvailable reports whether the docker CLI is on PATH. A positive answer
// is cached permanently; a negative one is re-probed after negativeLookupTTL so
// a one-click Docker install becomes usable without restarting the agent.
func (m *Manager) dockerAvailable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dockerOk {
		return true
	}
	if !m.dockerCheckedAt.IsZero() && time.Since(m.dockerCheckedAt) < negativeLookupTTL {
		return false
	}
	look := m.lookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look("docker")
	m.dockerOk = err == nil
	m.dockerCheckedAt = time.Now()
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

	// "run" takes op-specific args (name/ports/volumes/env/restart policy)
	// instead of the plain container/image pairs every other op uses, so it
	// gets its own path. A missing image is pulled by `docker run` itself,
	// hence the long budget on both sides.
	if op.GetOp() == "run" {
		m.runRun(ctx, op, result)
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
		cOut, iOut []byte
		cErr, iErr error
		wg         sync.WaitGroup
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

// runRunArgs is the args_json payload of op "run". Empty entries are skipped
// so the console can submit dynamic form rows that were never filled in.
type runRunArgs struct {
	Name          string   `json:"name"`
	Ports         []string `json:"ports"`          // "hostPort:containerPort"
	Volumes       []string `json:"volumes"`        // "/hostPath:/containerPath"
	Env           []string `json:"env"`            // "KEY=VALUE"
	RestartPolicy string   `json:"restart_policy"` // no / on-failure / always / unless-stopped
	Command       []string `json:"command"`
}

// Run-argument whitelists. exec.CommandContext never spawns a shell, but the
// values still flow into `docker` flags, where a crafted token could smuggle
// extra flags (--privileged, -v /, ...) into the run. Every args_json field is
// validated against these before it may join the argv.
var (
	runNameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	runPortRe   = regexp.MustCompile(`^[0-9]{1,5}:[0-9]{1,5}$`)
	runVolumeRe = regexp.MustCompile(`^[^\s:]+:[^\s:]+$`)
	runEnvRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=\S.*$`)
	runPolicyRe = regexp.MustCompile(`^(no|on-failure|always|unless-stopped)$`)
	// Image references: optional host[:port]/repo path segments then an
	// optional :tag or @digest. Rejects leading dashes so a payload cannot
	// masquerade as a docker flag. An env *value* may contain any bytes, but
	// env entries only ever land behind -e, and the KEY charset is locked
	// down, so `A=-v /:/host` is safe: docker treats it as a literal value.
	// Image reference grammar: body (:tag)? (@digest)? where the body is
	// either `host[:port]/path/...` (registry form, must contain a slash)
	// or a plain `name[/path...]`. Rejects leading dashes so a payload
	// cannot masquerade as a docker flag.
	runImageRe = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9._-]*(:[0-9]+)?(/[a-zA-Z0-9._-]+)+|[a-zA-Z0-9][a-zA-Z0-9._-]*(/[a-zA-Z0-9._-]+)*)(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)
)

// runRun executes `docker run -d ...` per the args_json payload and reports
// the created container's ID as the payload. Every field is whitelist-checked
// (see the regexes above) so a hostile payload cannot inject docker flags.
func (m *Manager) runRun(ctx context.Context, op *agentpb.DockerOp, result *agentpb.DockerEvent) {
	var args runRunArgs
	if len(op.GetArgsJson()) > 0 {
		if err := json.Unmarshal(op.GetArgsJson(), &args); err != nil {
			result.Ok = false
			result.Error = "invalid run args_json: " + err.Error()
			return
		}
	}
	image := strings.TrimSpace(op.GetImage())
	if image == "" {
		result.Ok = false
		result.Error = "run requires an image"
		return
	}

	policy := strings.TrimSpace(args.RestartPolicy)
	if policy == "" {
		policy = "no"
	}
	if !runPolicyRe.MatchString(policy) {
		result.Ok = false
		result.Error = "invalid restart_policy: " + policy
		return
	}

	argv := []string{"run", "-d", "--restart", policy}
	if name := strings.TrimSpace(args.Name); name != "" {
		if !runNameRe.MatchString(name) {
			result.Ok = false
			result.Error = "invalid container name: " + name
			return
		}
		argv = append(argv, "--name", name)
	}
	for _, p := range args.Ports {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !runPortRe.MatchString(p) {
			result.Ok = false
			result.Error = "invalid port mapping: " + p
			return
		}
		argv = append(argv, "-p", p)
	}
	for _, v := range args.Volumes {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !runVolumeRe.MatchString(v) {
			result.Ok = false
			result.Error = "invalid volume: " + v
			return
		}
		argv = append(argv, "-v", v)
	}
	for _, e := range args.Env {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !runEnvRe.MatchString(e) {
			result.Ok = false
			result.Error = "invalid env entry: " + e
			return
		}
		argv = append(argv, "-e", e)
	}
	if !runImageRe.MatchString(image) {
		result.Ok = false
		result.Error = "invalid image reference: " + image
		return
	}
	argv = append(argv, image)
	for _, c := range args.Command {
		if strings.TrimSpace(c) == "" {
			continue
		}
		argv = append(argv, c)
	}

	out, err := exec.CommandContext(ctx, "docker", argv...).Output()
	if err != nil {
		errOut := ""
		if ee, ok := err.(*exec.ExitError); ok {
			errOut = string(ee.Stderr)
		}
		result.Ok = false
		result.Error = err.Error() + ": " + errOut
		return
	}
	result.Ok = true
	result.PayloadJson = []byte(`{"container_id":` + strconv.Quote(strings.TrimSpace(string(out))) + `}`)
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
// take minutes — and `docker run` pulls a missing image before creating the
// container — so both get a long budget like the server side.
func opTimeout(op string) time.Duration {
	if op == "pull" || op == "run" {
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
