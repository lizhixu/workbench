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
	// runCmd runs the docker CLI and is swappable in unit tests so concurrency,
	// singleflight, and ordering invariants can be verified cross-platform.
	runCmd func(ctx context.Context, combineStderr bool, args ...string) ([]byte, error)

	// readSem serializes heavy read-only inventory commands (ps, ps_all, images,
	// system_df) to at most one running CLI subprocess at a time. This prevents
	// concurrent metadata scans and lock/RSS spikes under both native docker-ce
	// (slow storage/overlay2 contention) and daemonless runtimes like podman-docker.
	readSemMu sync.Mutex
	readSem   chan struct{}

	// flightMu coalesces concurrent identical read-only DockerOps so overlapping
	// dashboard/tab refreshes share one CLI execution. Mutating ops bump
	// readGen and clear inflight entries so post-mutation reads never observe
	// stale pre-mutation snapshots.
	flightMu sync.Mutex
	readGen  uint64
	inflight map[string]*inflightRead
}

type inflightRead struct {
	gen     uint64
	done    chan struct{}
	payload []byte
	err     error
}

// negativeLookupTTL is how long a "docker is missing" answer stays cached.
const negativeLookupTTL = 20 * time.Second

func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		log:      log,
		lookPath: exec.LookPath,
		readSem:  make(chan struct{}, 1),
		inflight: make(map[string]*inflightRead),
	}
}

func (m *Manager) getReadSem() chan struct{} {
	m.readSemMu.Lock()
	if m.readSem == nil {
		m.readSem = make(chan struct{}, 1)
	}
	sem := m.readSem
	m.readSemMu.Unlock()
	return sem
}

func (m *Manager) invalidateReads() {
	m.flightMu.Lock()
	m.readGen++
	clear(m.inflight)
	m.flightMu.Unlock()
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

	// ps_images is a compound op that runs `docker ps` then `docker images`
	// sequentially (via readSem) and coalesces concurrent callers via
	// runReadCoalesced, collapsing two HTTP round trips into one without
	// spawning parallel cold-start CLI processes against Podman/overlay2.
	if op.GetOp() == "ps_images" {
		out, err := m.runReadCoalesced(ctx, "ps_images", m.collectPsImages)
		if err != nil {
			result.Ok = false
			result.Error = err.Error()
		} else {
			result.Ok = true
			result.PayloadJson = out
		}
		m.send(result)
		return
	}

	// Mutating ops invalidate any cached/in-flight read snapshots both before
	// starting and on completion so subsequent dashboard refreshes never join a
	// stale pre-mutation scan.
	if !isReadOnlyOp(op.GetOp()) {
		m.invalidateReads()
		defer m.invalidateReads()
	}

	// "system_prune" (and its "prune" alias, which the server's dockerOp
	// whitelist already advertises) is a compound op: it runs the four
	// targeted prunes in dependency order and returns one JSON document with
	// the reclaimed space per step. Containers go first so images freed by
	// deleted containers are caught by the image prune in the same run; the
	// container prune carries a label!=watchman.app filter so
	// application-centre containers — including stopped -prev rollback
	// containers — are never deleted.
	if op.GetOp() == "system_prune" || op.GetOp() == "prune" {
		m.runSystemPrune(ctx, result)
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

	// "logs" likewise takes op-specific args (tail/since/timestamps) via
	// args_json, so it gets its own argv builder instead of dockerArgs.
	var args []string
	if op.GetOp() == "logs" {
		var err error
		args, err = logsArgs(op.GetContainer(), op.GetArgsJson())
		if err != nil {
			result.Ok = false
			result.Error = err.Error()
			m.send(result)
			return
		}
	} else {
		var ok bool
		args, ok = dockerArgs(op.GetOp(), op.GetContainer(), op.GetImage())
		if !ok {
			result.Ok = false
			result.Error = fmt.Sprintf("unknown docker op: %s", op.GetOp())
			m.send(result)
			return
		}
	}

	if isReadInventoryOp(op.GetOp()) {
		out, err := m.runReadCoalesced(ctx, op.GetOp(), func(c context.Context) ([]byte, error) {
			raw, runErr := m.execReadDocker(c, args...)
			if runErr != nil {
				return nil, fmt.Errorf("%s", formatExecError(runErr, raw))
			}
			return raw, nil
		})
		if err != nil {
			result.Ok = false
			result.Error = err.Error()
		} else {
			result.Ok = true
			result.PayloadJson = out
		}
		m.send(result)
		return
	}

	// `docker logs` 的容器输出分流：CLI 把容器 stdout 写到自身 stdout、
	// 容器 stderr 写到自身 stderr；只抓 stdout 会丢掉写 stderr 的应用日志
	// （如 Go 默认 log，certimate 即全量 stderr），故 logs 单独走
	// CombinedOutput 合并两流。ps/images 的输出是结构化 NDJSON，stderr
	// 混入会污染解析，保持只抓 stdout。
	var out []byte
	var err error
	if op.GetOp() == "logs" {
		out, err = m.execDocker(ctx, true, args...)
	} else {
		out, err = m.execDocker(ctx, false, args...)
	}
	if err != nil {
		result.Ok = false
		result.Error = formatExecError(err, out)
	} else {
		result.Ok = true
		result.PayloadJson = out
	}

	m.send(result)
}

func isReadInventoryOp(op string) bool {
	return op == "ps" || op == "ps_all" || op == "images" || op == "ps_images" || op == "system_df"
}

func isReadOnlyOp(op string) bool {
	return isReadInventoryOp(op) || op == "logs" || op == "inspect"
}

func formatExecError(err error, out []byte) string {
	errOut := ""
	if ee, ok := err.(*exec.ExitError); ok {
		errOut = string(ee.Stderr)
	}
	if errOut == "" {
		errOut = string(out)
	}
	return err.Error() + ": " + errOut
}

func (m *Manager) execDocker(ctx context.Context, combineStderr bool, args ...string) ([]byte, error) {
	if m.runCmd != nil {
		return m.runCmd(ctx, combineStderr, args...)
	}
	cmd := dockerCommand(ctx, args...)
	if combineStderr {
		return cmd.CombinedOutput()
	}
	return cmd.Output()
}

func (m *Manager) execReadDocker(ctx context.Context, args ...string) ([]byte, error) {
	sem := m.getReadSem()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.execDocker(ctx, false, args...)
}

func (m *Manager) runReadCoalesced(ctx context.Context, key string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	m.flightMu.Lock()
	if m.inflight == nil {
		m.inflight = make(map[string]*inflightRead)
	}
	if call, ok := m.inflight[key]; ok && call.gen == m.readGen {
		m.flightMu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			return bytes.Clone(call.payload), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	gen := m.readGen
	call := &inflightRead{gen: gen, done: make(chan struct{})}
	m.inflight[key] = call
	m.flightMu.Unlock()

	execCtx, cancel := context.WithTimeout(context.Background(), opTimeout(key))
	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				call.err = fmt.Errorf("docker read %s panic: %v", key, r)
			}
			m.flightMu.Lock()
			if m.inflight[key] == call {
				delete(m.inflight, key)
			}
			m.flightMu.Unlock()
			close(call.done)
		}()
		call.payload, call.err = fn(execCtx)
	}()

	select {
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		return bytes.Clone(call.payload), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// collectPsImages runs `docker ps` then `docker images` sequentially via readSem
// and merges their NDJSON output into a single JSON document.
func (m *Manager) collectPsImages(ctx context.Context) ([]byte, error) {
	type psImages struct {
		Containers []any `json:"containers"`
		Images     []any `json:"images"`
	}

	cOut, cErr := m.execReadDocker(ctx, "ps", "--format", psJSONFormat)
	if cErr != nil {
		return nil, fmt.Errorf("docker ps: %s", formatExecError(cErr, cOut))
	}
	iOut, iErr := m.execReadDocker(ctx, "images", "--format", "{{json .}}")
	if iErr != nil {
		return nil, fmt.Errorf("docker images: %s", formatExecError(iErr, iOut))
	}

	merged := psImages{
		Containers: parseNDJSONAny(cOut),
		Images:     parseNDJSONAny(iOut),
	}
	payload, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal ps_images: %w", err)
	}
	return payload, nil
}

// systemPruneStep is one targeted prune inside the system_prune compound op.
// Argv is the docker CLI invocation; Reclaimed is the "Total reclaimed
// space" value parsed from that step's stdout ("" when the line is absent).
type systemPruneStep struct {
	Name      string   `json:"name"` // containers | images | networks | builder
	Argv      []string `json:"-"`
	Reclaimed string   `json:"reclaimed"`
}

var reclaimedSpaceRe = regexp.MustCompile(`(?m)^Total reclaimed space:\s*(.+?)\s*$`)

// runSystemPrune runs the four targeted prunes in dependency order and
// returns {"steps":[...]} with the reclaimed space per step. A step failure
// aborts the remaining steps (fail-fast) so a half-finished cleanup is
// reported as a failure, not a success.
func (m *Manager) runSystemPrune(ctx context.Context, result *agentpb.DockerEvent) {
	steps := []systemPruneStep{
		{Name: "containers", Argv: []string{"container", "prune", "-f", "--filter", "label!=watchman.app"}},
		{Name: "images", Argv: []string{"image", "prune", "-af"}},
		{Name: "networks", Argv: []string{"network", "prune", "-f"}},
		{Name: "builder", Argv: []string{"builder", "prune", "-af"}},
	}
	for i := range steps {
		out, err := m.execDocker(ctx, false, steps[i].Argv...)
		if err != nil {
			result.Ok = false
			result.Error = steps[i].Name + " prune failed: " + formatExecError(err, out)
			return
		}
		if match := reclaimedSpaceRe.FindSubmatch(out); match != nil {
			steps[i].Reclaimed = string(match[1])
		}
	}
	payload, err := json.Marshal(struct {
		Steps []systemPruneStep `json:"steps"`
	}{Steps: steps})
	if err != nil {
		result.Ok = false
		result.Error = "marshal system_prune: " + err.Error()
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

	out, err := m.execDocker(ctx, false, argv...)
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

// logSinceRe is the allowlist for the logs op's since value: a docker
// duration like 24h. Anything else (timestamps, free text, leading dashes)
// fails closed in logsArgs so a crafted value can never smuggle extra
// docker flags — argv form means no shell is involved either way.
var logSinceRe = regexp.MustCompile(`^[0-9]+[smhd]$`)

// logsArgs builds `docker logs` argv from args_json:
// {"tail":200,"since":"24h","timestamps":true} — all fields optional.
// tail defaults to 200 and must be 1..10000; since "" or "all" means no
// --since flag. Old agents ignore args_json for logs and keep returning
// --tail 200, so a new panel talking to an old agent degrades to the
// default tail without breaking.
func logsArgs(container string, argsJSON []byte) ([]string, error) {
	if container == "" {
		return nil, fmt.Errorf("logs: container is required")
	}
	if strings.HasPrefix(container, "-") {
		return nil, fmt.Errorf("logs: invalid container name %q", container)
	}
	tail := 200
	since := ""
	timestamps := false
	if len(argsJSON) > 0 {
		var args struct {
			Tail       *int   `json:"tail"`
			Since      string `json:"since"`
			Timestamps bool   `json:"timestamps"`
		}
		if err := json.Unmarshal(argsJSON, &args); err != nil {
			return nil, fmt.Errorf("logs: invalid args_json: %w", err)
		}
		if args.Tail != nil {
			tail = *args.Tail
		}
		since = args.Since
		timestamps = args.Timestamps
	}
	if tail < 1 || tail > 10000 {
		return nil, fmt.Errorf("logs: tail out of range: %d", tail)
	}
	if since != "" && since != "all" && !logSinceRe.MatchString(since) {
		return nil, fmt.Errorf("logs: invalid since: %q", since)
	}
	argv := []string{"logs", "--tail", strconv.Itoa(tail)}
	if since != "" && since != "all" {
		argv = append(argv, "--since", since)
	}
	if timestamps {
		argv = append(argv, "--timestamps")
	}
	argv = append(argv, container)
	return argv, nil
}

// psJSONFormat selects only the container fields consumed by the console and
// explicitly omits .Size. On Docker CE 24.x, `--format {{json .}}` causes the
// CLI to evaluate the full ContainerContext and request writable-layer size
// calculation (`size=1`), forcing dockerd to walk every overlay2 tree on disk
// (tens of seconds + heavy slab/RSS spikes), whereas selecting fields without
// .Size keeps `docker ps` around ~20ms. Each field uses `{{json ...}}` so
// special characters (and podman-docker slices/maps) stay valid JSON.
const psJSONFormat = `{"ID":{{json .ID}},"Names":{{json .Names}},"Image":{{json .Image}},"Status":{{json .Status}},"State":{{json .State}},"Ports":{{json .Ports}},"Labels":{{json .Labels}}}`

// dockerArgs maps an op to its `docker` CLI argv.
func dockerArgs(op, container, image string) ([]string, bool) {
	switch op {
	case "ps":
		return []string{"ps", "--format", psJSONFormat}, true
	case "ps_all":
		// ps_all 跑 `docker ps -a`：镜像表「使用状态」列按 Docker 口径判断
		// 镜像是否被任意容器引用（含已停止容器）。只用运行中容器算会把
		// -prev 回滚容器等已停止容器引用的镜像错标成未使用。
		return []string{"ps", "-a", "--format", psJSONFormat}, true
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
	case "inspect":
		return []string{"inspect", container}, true
	case "remove_image":
		return []string{"rmi", image}, true
	case "prune_images":
		return []string{"image", "prune", "-f"}, true
	case "system_df":
		return []string{"system", "df", "--format", "{{json .}}"}, true
	case "volume_prune":
		return []string{"volume", "prune", "-f"}, true
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
// container — so both get a long budget like the server side. Prune ops share
// the long budget: deleting gigabytes of images and build cache routinely
// exceeds 30 seconds.
func opTimeout(op string) time.Duration {
	if op == "pull" || op == "run" || op == "system_prune" || op == "prune" || op == "volume_prune" {
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

// ContainerInfo is a typed subset of `docker ps --format` JSON lines.
type ContainerInfo struct {
	ID     string `json:"ID"`
	Image  string `json:"Image"`
	Name   string `json:"Names"`
	Status string `json:"Status"`
	Ports  string `json:"Ports"`
}

// ParseContainers parses `docker ps --format` NDJSON output lines.
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
