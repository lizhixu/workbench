package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watchman/proto/agentpb"
)

// The console can install Docker on a running host, so a cached "missing"
// answer must expire. Otherwise Docker management stays broken until the agent
// restarts, which the one-click install flow does not do.
func TestNegativeDockerLookupIsRechecked(t *testing.T) {
	m := NewManager(nil)
	calls := 0
	present := false
	m.lookPath = func(string) (string, error) {
		calls++
		if present {
			return "/usr/bin/docker", nil
		}
		return "", errors.New("not found")
	}

	if m.dockerAvailable() {
		t.Fatal("docker 不在 PATH 时应返回 false")
	}
	if m.dockerAvailable() {
		t.Fatal("缓存期内仍应返回 false")
	}
	if calls != 1 {
		t.Errorf("缓存期内应只探测 1 次，实际 %d 次", calls)
	}

	// Simulate the install finishing, then let the negative cache expire.
	present = true
	m.mu.Lock()
	m.dockerCheckedAt = time.Now().Add(-negativeLookupTTL - time.Second)
	m.mu.Unlock()

	if !m.dockerAvailable() {
		t.Error("缓存过期后应重新探测并发现 docker 已安装")
	}
	if calls != 2 {
		t.Errorf("应重新探测一次，实际累计 %d 次", calls)
	}
}

// A positive answer must not be re-probed on every op.
func TestPositiveDockerLookupIsCached(t *testing.T) {
	m := NewManager(nil)
	calls := 0
	m.lookPath = func(string) (string, error) {
		calls++
		return "/usr/bin/docker", nil
	}
	for i := 0; i < 5; i++ {
		if !m.dockerAvailable() {
			t.Fatal("docker 存在时应返回 true")
		}
	}
	if calls != 1 {
		t.Errorf("正向结果应只探测 1 次，实际 %d 次", calls)
	}
}

// The run whitelists must reject payloads that would smuggle docker flags into
// the argv (e.g. a container named "--privileged") while accepting the normal
// values the console sends.
func TestRunWhitelists(t *testing.T) {
	cases := []struct {
		name string
		re   *regexp.Regexp
		ok   string
		bad  string
	}{
		{"name", runNameRe, "my-app_1.2", "--privileged"},
		{"port", runPortRe, "8080:80", "8080:80 --privileged"},
		{"volume", runVolumeRe, "/data:/data", "/etc:/etc -v /:/:ro"},
		{"env", runEnvRe, "TZ=Asia/Shanghai", "=start"},
		{"policy", runPolicyRe, "unless-stopped", "always --privileged"},
		{"image", runImageRe, "nginx:alpine", "--pull=always"},
	}
	for _, c := range cases {
		if !c.re.MatchString(c.ok) {
			t.Errorf("%s 白名单应放行 %q", c.name, c.ok)
		}
		if c.re.MatchString(c.bad) {
			t.Errorf("%s 白名单应拒绝 %q", c.name, c.bad)
		}
	}
	// digest / registry forms are valid image references
	for _, ref := range []string{"nginx", "redis:7.2-alpine", "registry.example.com:5000/app/web:1.0", "app@sha256:" + strings.Repeat("a", 64)} {
		if !runImageRe.MatchString(ref) {
			t.Errorf("镜像引用 %q 应放行", ref)
		}
	}
}

// logsArgs: tail/since/timestamps 走 allowlist 校验，范围外的值直接失败，
// 绝不拼进 docker argv；空 args 保持老行为 --tail 200（老 Agent 忽略
// args_json 时同样返回 200 行，新面板对老 Agent 自然降级）。
func TestLogsArgs(t *testing.T) {
	cases := []struct {
		name      string
		container string
		args      string
		want      string
	}{
		{"空 args 默认 200 行", "web", "", "logs --tail 200 web"},
		{"全部参数", "web", `{"tail":500,"since":"24h","timestamps":true}`, "logs --tail 500 --since 24h --timestamps web"},
		{"since all 不传 flag", "web", `{"since":"all"}`, "logs --tail 200 web"},
		{"仅 timestamps", "web", `{"timestamps":true}`, "logs --tail 200 --timestamps web"},
	}
	for _, tc := range cases {
		got, err := logsArgs(tc.container, []byte(tc.args))
		if err != nil {
			t.Errorf("%s: 不应报错: %v", tc.name, err)
			continue
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s: 得到 %q, 想要 %q", tc.name, strings.Join(got, " "), tc.want)
		}
	}

	invalid := []struct {
		name      string
		container string
		args      string
	}{
		{"容器为空", "", ""},
		{"容器以 - 开头（flag 注入）", "-x", ""},
		{"tail 为 0", "web", `{"tail":0}`},
		{"tail 超上限", "web", `{"tail":10001}`},
		{"tail 非整数", "web", `{"tail":1.5}`},
		{"since 非 duration", "web", `{"since":"yesterday"}`},
		{"since 带前导 -", "web", `{"since":"-1h"}`},
		{"since 带空格", "web", `{"since":"1h; rm"}`},
		{"非法 JSON", "web", `{bad`},
	}
	for _, tc := range invalid {
		if _, err := logsArgs(tc.container, []byte(tc.args)); err == nil {
			t.Errorf("%s: 应报错却通过", tc.name)
		}
	}
}

func TestLogsArgsBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		container   string
		args        string
		want        string
		wantErr     bool
		errContains string
	}{
		{"默认无 args：200 行且不加 --timestamps（老 Agent 兼容）", "web", "", "logs --tail 200 web", false, ""},
		{"tail 下边界 1", "web", `{"tail":1}`, "logs --tail 1 web", false, ""},
		{"tail 上边界 10000", "web", `{"tail":10000}`, "logs --tail 10000 web", false, ""},
		{"since 合法单位 s", "web", `{"since":"30s"}`, "logs --tail 200 --since 30s web", false, ""},
		{"since 合法单位 m", "web", `{"since":"5m"}`, "logs --tail 200 --since 5m web", false, ""},
		{"since 合法单位 h", "web", `{"since":"1h"}`, "logs --tail 200 --since 1h web", false, ""},
		{"since 合法单位 d", "web", `{"since":"7d"}`, "logs --tail 200 --since 7d web", false, ""},
		{"未知字段被忽略", "web", `{"tail":10,"unknown_field":"ignored"}`, "logs --tail 10 web", false, ""},
		{"args_json 为 null 回落默认 200 行", "web", `null`, "logs --tail 200 web", false, ""},
		{"空对象", "web", `{}`, "logs --tail 200 web", false, ""},

		{"tail 为 0", "web", `{"tail":0}`, "", true, "tail out of range"},
		{"tail 为负数", "web", `{"tail":-5}`, "", true, "tail out of range"},
		{"tail 10001 超上限", "web", `{"tail":10001}`, "", true, "tail out of range"},
		{"tail 非整数 1.5", "web", `{"tail":1.5}`, "", true, "invalid args_json"},
		{"tail 溢出 int64", "web", `{"tail":99999999999999999999}`, "", true, "invalid args_json"},

		{"since 复合时长 1h30m", "web", `{"since":"1h30m"}`, "", true, "invalid since"},
		{"since RFC3339 时间戳", "web", `{"since":"2024-01-01T00:00:00Z"}`, "", true, "invalid since"},
		{"since 带前导 -", "web", `{"since":"-1h"}`, "", true, "invalid since"},
		{"since 非法单位 1x", "web", `{"since":"1x"}`, "", true, "invalid since"},
		{"since 纯数字", "web", `{"since":"123"}`, "", true, "invalid since"},

		{"timestamps 为字符串", "web", `{"timestamps":"true"}`, "", true, "invalid args_json"},
		{"timestamps 为数字", "web", `{"timestamps":1}`, "", true, "invalid args_json"},

		{"args_json 为数组", "web", `[1,2,3]`, "", true, "invalid args_json"},
		{"args_json 为字符串", "web", `"hello"`, "", true, "invalid args_json"},
		{"args_json 非法 JSON", "web", `{bad`, "", true, "invalid args_json"},

		{"容器名为空", "", "", "", true, "container is required"},
		{"容器名以 - 开头（flag 注入）", "-x", "", "", true, "invalid container name"},
	}
	for _, tc := range cases {
		got, err := logsArgs(tc.container, []byte(tc.args))
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: 应 fail-closed 报错，实际通过并得到 %q", tc.name, strings.Join(got, " "))
				continue
			}
			if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
				t.Errorf("%s: 错误信息 %q 不含 %q", tc.name, err.Error(), tc.errContains)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 不应报错: %v", tc.name, err)
			continue
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s: 得到 %q, 想要 %q", tc.name, strings.Join(got, " "), tc.want)
		}
	}
}

// TestDockerArgsPruneOps locks the deep-clean argv: system_df reports
// reclaimable space, volume_prune deletes unused volumes, and the compound
// system_prune/prune ops must NOT go through dockerArgs (Manager.run
// dispatches them to runSystemPrune instead, like logs goes to logsArgs).
func TestDockerArgsPruneOps(t *testing.T) {
	cases := []struct {
		op   string
		want string
	}{
		{"system_df", "system df --format {{json .}}"},
		{"volume_prune", "volume prune -f"},
	}
	for _, tc := range cases {
		args, ok := dockerArgs(tc.op, "", "")
		if !ok {
			t.Errorf("dockerArgs(%q) 应被接受", tc.op)
			continue
		}
		if strings.Join(args, " ") != tc.want {
			t.Errorf("dockerArgs(%q) = %q, 想要 %q", tc.op, strings.Join(args, " "), tc.want)
		}
	}
	for _, op := range []string{"system_prune", "prune"} {
		if args, ok := dockerArgs(op, "", ""); ok || args != nil {
			t.Errorf("dockerArgs(%q) 应走 runSystemPrune，实际 (%v,%v)", op, args, ok)
		}
	}
	// Prune ops share the long budget with pull/run: deleting gigabytes of
	// images and build cache routinely exceeds 30 seconds.
	for _, op := range []string{"system_prune", "prune", "volume_prune"} {
		if got := opTimeout(op); got != 5*time.Minute {
			t.Errorf("opTimeout(%q) 应为 5 分钟，实际 %v", op, got)
		}
	}
	if got := opTimeout("system_df"); got != 30*time.Second {
		t.Errorf("opTimeout(system_df) 应为 30 秒，实际 %v", got)
	}
}

// ps / ps_all 禁止使用 {{json .}} 或引用 .Size（Docker CE 24 下会触发 daemon
// 扫描全量 overlay2 可写层计算磁盘大小，导致响应超时及内存/磁盘读飙升）；
// ps_all 供镜像表「使用状态」列取全部容器（含已停止）：只读，30 秒超时。
func TestDockerArgsPsAll(t *testing.T) {
	for _, tc := range []struct {
		op   string
		want string
	}{
		{"ps", "ps --format " + psJSONFormat},
		{"ps_all", "ps -a --format " + psJSONFormat},
	} {
		args, ok := dockerArgs(tc.op, "", "")
		if !ok {
			t.Fatalf("dockerArgs(%s) 应被接受", tc.op)
		}
		if strings.Join(args, " ") != tc.want {
			t.Errorf("dockerArgs(%s) = %q, 想要 %q", tc.op, strings.Join(args, " "), tc.want)
		}
		if strings.Contains(tc.want, ".Size") || strings.Contains(tc.want, "{{json .}}") {
			t.Errorf("dockerArgs(%s) 不得含 .Size 或 {{json .}}: %q", tc.op, tc.want)
		}
	}
	if got := opTimeout("ps_all"); got != 30*time.Second {
		t.Errorf("opTimeout(ps_all) 应为 30 秒，实际 %v", got)
	}
}

// runSystemPrune 必须按依赖顺序删除（先容器、后镜像：被删容器释放的镜像要
// 在同一次清理里被镜像清理带走），容器清理必须带 label!=watchman.app 过滤
// （应用中心纳管的已停止容器——含 -prev 回滚容器——绝不能删），每步回收量
// 从各步输出的 Total reclaimed space 行解析。
func TestRunSystemPrune(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"container\" ]; then echo 'Total reclaimed space: 100MB'; exit 0; fi\n" +
		"if [ \"$1\" = \"image\" ]; then echo 'Total reclaimed space: 1.5GB'; exit 0; fi\n" +
		"if [ \"$1\" = \"network\" ]; then echo 'Total reclaimed space: 0B'; exit 0; fi\n" +
		"if [ \"$1\" = \"builder\" ]; then echo 'Total reclaimed space: 2.3GB'; exit 0; fi\n" +
		"echo unexpected >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := NewManager(nil)
	m.lookPath = func(string) (string, error) { return filepath.Join(dir, "docker"), nil }
	s := &captureSender{}
	m.SetSender(s)
	m.run(&agentpb.DockerOp{Op: "system_prune"})
	if s.msg == nil {
		t.Fatal("Manager.run 未发送 DockerEvent")
	}
	ev := s.msg.GetDocker()
	if !ev.GetOk() {
		t.Fatalf("system_prune 应成功，实际失败：%s", ev.GetError())
	}
	var payload struct {
		Steps []struct {
			Name      string `json:"name"`
			Reclaimed string `json:"reclaimed"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(ev.GetPayloadJson(), &payload); err != nil {
		t.Fatalf("system_prune payload 应为 JSON，实际 %q: %v", string(ev.GetPayloadJson()), err)
	}
	want := []struct {
		name      string
		reclaimed string
	}{
		{"containers", "100MB"},
		{"images", "1.5GB"},
		{"networks", "0B"},
		{"builder", "2.3GB"},
	}
	if len(payload.Steps) != len(want) {
		t.Fatalf("应返回 %d 步，实际 %d 步：%v", len(want), len(payload.Steps), payload.Steps)
	}
	for i, w := range want {
		if payload.Steps[i].Name != w.name || payload.Steps[i].Reclaimed != w.reclaimed {
			t.Errorf("第 %d 步应为 %+v，实际 %+v", i, w, payload.Steps[i])
		}
	}

	// 调用顺序与容器过滤：第一条必须是带 label 过滤的容器清理。
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 4 {
		t.Fatalf("应恰好调用 4 次 docker，实际 %d 次：%v", len(lines), lines)
	}
	if lines[0] != "container prune -f --filter label!=watchman.app" {
		t.Errorf("容器清理应带 label!=watchman.app 过滤，实际 %q", lines[0])
	}
	if lines[1] != "image prune -af" || lines[2] != "network prune -f" || lines[3] != "builder prune -af" {
		t.Errorf("清理顺序应为容器/镜像/网络/构建缓存，实际 %v", lines)
	}

	// "prune" 别名走同一条复合路径。
	s2 := &captureSender{}
	m.SetSender(s2)
	m.run(&agentpb.DockerOp{Op: "prune"})
	if s2.msg == nil || !s2.msg.GetDocker().GetOk() {
		t.Error("prune 别名应与 system_prune 同样成功")
	}
}

// 某一步失败时 system_prune 整体失败、后续步骤不再执行（fail-fast），
// 而不是报成功。
func TestRunSystemPruneFailsFast(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"container\" ]; then echo 'Total reclaimed space: 100MB'; exit 0; fi\n" +
		"if [ \"$1\" = \"image\" ]; then echo 'Error: daemon busy' >&2; exit 1; fi\n" +
		"echo unexpected >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	m := NewManager(nil)
	m.lookPath = func(string) (string, error) { return filepath.Join(dir, "docker"), nil }
	s := &captureSender{}
	m.SetSender(s)
	m.run(&agentpb.DockerOp{Op: "system_prune"})
	if s.msg == nil {
		t.Fatal("Manager.run 未发送 DockerEvent")
	}
	ev := s.msg.GetDocker()
	if ev.GetOk() {
		t.Error("镜像清理失败时整体应失败")
	} else if !strings.Contains(ev.GetError(), "images prune failed") {
		t.Errorf("失败信息应指明 images 步骤，实际 %q", ev.GetError())
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 {
		t.Errorf("失败后应停在第 2 步，实际调用 %d 次：%v", len(lines), lines)
	}
}

// dockerArgs has no "logs" case: Manager.run dispatches op "logs" to logsArgs
// before ever calling dockerArgs, so dockerArgs("logs", ...) must fail closed
// as an unknown op. This locks that the two dispatch paths do not overlap.
func TestDockerArgsRejectsUnknownOps(t *testing.T) {
	cases := []struct {
		name string
		op   string
	}{
		{"logs 由 logsArgs 单独处理", "logs"},
		{"未知 op", "bogus"},
		{"空 op", ""},
	}
	for _, tc := range cases {
		args, ok := dockerArgs(tc.op, "web", "img")
		if ok || args != nil {
			t.Errorf("%s: dockerArgs(%q) 应返回 (nil,false)，实际 (%v,%v)", tc.name, tc.op, args, ok)
		}
	}
}

// captureSender records the single DockerEvent a Manager run sends.
type captureSender struct {
	msg *agentpb.AgentMessage
}

func (s *captureSender) Send(msg *agentpb.AgentMessage) bool {
	s.msg = msg
	return true
}

// logs 必须合并 stdout 与 stderr：docker CLI 把容器 stderr 分流到自身
// stderr，`Output()` 只抓 stdout 时写 stderr 的应用（如 Go 默认 log 的
// certimate）日志会全丢、面板恒显示空。ps 等结构化 op 保持只抓 stdout，
// stderr 混入会污染 NDJSON 解析。
func TestRunLogsMergesStderr(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"logs\" ]; then\n" +
		"  if [ \"$4\" = \"missing\" ]; then echo 'Error: No such container: missing' >&2; exit 1; fi\n" +
		"  echo stdout-line\n" +
		"  echo stderr-line >&2\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"ps\" ]; then\n" +
		"  echo '{\"ID\":\"abc\"}'\n" +
		"  echo ps-stderr-noise >&2\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo unexpected >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	runOp := func(op *agentpb.DockerOp) *agentpb.DockerEvent {
		m := NewManager(nil)
		m.lookPath = func(string) (string, error) { return filepath.Join(dir, "docker"), nil }
		s := &captureSender{}
		m.SetSender(s)
		m.run(op)
		if s.msg == nil {
			t.Fatal("Manager.run 未发送 DockerEvent")
		}
		return s.msg.GetDocker()
	}

	// logs：stdout 与 stderr 都要进 payload。
	ev := runOp(&agentpb.DockerOp{Op: "logs", Container: "web"})
	if !ev.GetOk() {
		t.Fatalf("logs 应成功，实际失败：%s", ev.GetError())
	}
	payload := string(ev.GetPayloadJson())
	if !strings.Contains(payload, "stdout-line") || !strings.Contains(payload, "stderr-line") {
		t.Errorf("logs payload 应同时含 stdout 与 stderr 行，实际 %q", payload)
	}

	// logs 失败时 CombinedOutput 的报错仍要带回（ExitError.Stderr 为空）。
	ev = runOp(&agentpb.DockerOp{Op: "logs", Container: "missing"})
	if ev.GetOk() {
		t.Error("logs 不存在的容器应失败")
	} else if !strings.Contains(ev.GetError(), "No such container") {
		t.Errorf("logs 失败信息应含 CLI 报错，实际 %q", ev.GetError())
	}

	// ps：保持只抓 stdout，stderr 噪声不得混入 NDJSON。
	ev = runOp(&agentpb.DockerOp{Op: "ps"})
	if !ev.GetOk() {
		t.Fatalf("ps 应成功，实际失败：%s", ev.GetError())
	}
	if strings.Contains(string(ev.GetPayloadJson()), "ps-stderr-noise") {
		t.Errorf("ps payload 不应含 stderr 内容，实际 %q", string(ev.GetPayloadJson()))
	}
}

type multiSender struct {
	mu   sync.Mutex
	evs  map[string]*agentpb.DockerEvent
	done chan struct{}
	want int
}

func newMultiSender(want int) *multiSender {
	return &multiSender{
		evs:  make(map[string]*agentpb.DockerEvent),
		done: make(chan struct{}),
		want: want,
	}
}

func (s *multiSender) Send(msg *agentpb.AgentMessage) bool {
	ev := msg.GetDocker()
	if ev == nil {
		return true
	}
	s.mu.Lock()
	s.evs[ev.GetOpId()] = ev
	if len(s.evs) >= s.want {
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
	s.mu.Unlock()
	return true
}

// runPsImages 与跨 op 只读清单必须受 readSem 串行保护，不能像此前 wg.Add(2) +
// ps_all 那样并发拉起 2~3 个 CLI 进程（在 podman-docker 上会倍增内存并锁死存储）。
func TestRunPsImagesSequentialAndSerializesReads(t *testing.T) {
	m := NewManager(nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/docker", nil }

	var active atomic.Int32
	var maxActive atomic.Int32
	var callsMu sync.Mutex
	var calls []string

	m.runCmd = func(ctx context.Context, combineStderr bool, args ...string) ([]byte, error) {
		if combineStderr {
			t.Errorf("inventory read should not combine stderr: %v", args)
		}
		cur := active.Add(1)
		for {
			prev := maxActive.Load()
			if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer active.Add(-1)

		time.Sleep(15 * time.Millisecond)

		callsMu.Lock()
		calls = append(calls, strings.Join(args, " "))
		callsMu.Unlock()

		switch args[0] {
		case "ps":
			return []byte(`{"ID":"c1","Names":"web"}` + "\n"), nil
		case "images":
			return []byte(`{"ID":"i1","Repository":"nginx","Tag":"latest"}` + "\n"), nil
		case "system":
			return []byte(`[{"Type":"Images","TotalCount":1}]`), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}

	s := newMultiSender(3)
	m.SetSender(s)
	m.Handle(&agentpb.DockerOp{OpId: "op1", Op: "ps_images"})
	m.Handle(&agentpb.DockerOp{OpId: "op2", Op: "ps_all"})
	m.Handle(&agentpb.DockerOp{OpId: "op3", Op: "system_df"})

	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for serialized read ops")
	}

	if got := maxActive.Load(); got != 1 {
		t.Errorf("concurrent read CLI count = %d, want max 1", got)
	}
	ev1 := s.evs["op1"]
	if ev1 == nil || !ev1.GetOk() {
		t.Fatalf("ps_images failed: %v", ev1)
	}
	var parsed struct {
		Containers []map[string]any `json:"containers"`
		Images     []map[string]any `json:"images"`
	}
	if err := json.Unmarshal(ev1.GetPayloadJson(), &parsed); err != nil {
		t.Fatalf("unmarshal ps_images payload: %v", err)
	}
	if len(parsed.Containers) != 1 || len(parsed.Images) != 1 {
		t.Errorf("unexpected ps_images payload: %+v", parsed)
	}
	// Verify ps preceded images within collectPsImages and uses psJSONFormat (without .Size).
	callsMu.Lock()
	psIdx, imgIdx := -1, -1
	for idx, c := range calls {
		if c == "ps --format "+psJSONFormat && psIdx == -1 {
			psIdx = idx
		}
		if c == "images --format {{json .}}" && imgIdx == -1 {
			imgIdx = idx
		}
	}
	callsMu.Unlock()
	if psIdx < 0 || imgIdx < 0 || psIdx > imgIdx {
		t.Errorf("expected ps before images, calls = %v", calls)
	}
}

// 并发相同的只读清单请求应合并为一次 CLI 执行（singleflight），而写操作之后发起的
// 读取必须重新采集最新状态，不得复用写操作前的在途请求。
func TestRunReadCoalescingAndMutationInvalidation(t *testing.T) {
	m := NewManager(nil)
	m.lookPath = func(string) (string, error) { return "/usr/bin/docker", nil }

	var gen atomic.Int32
	var psCount atomic.Int32
	psStarted := make(chan struct{}, 8)
	releasePs := make(chan struct{})
	var releaseMu sync.Mutex

	waitPsGate := func() {
		releaseMu.Lock()
		ch := releasePs
		releaseMu.Unlock()
		<-ch
	}

	m.runCmd = func(ctx context.Context, _ bool, args ...string) ([]byte, error) {
		switch args[0] {
		case "ps":
			psCount.Add(1)
			select {
			case psStarted <- struct{}{}:
			default:
			}
			waitPsGate()
			if gen.Load() == 0 {
				return []byte(`{"ID":"c1","Status":"Up"}` + "\n"), nil
			}
			return []byte(`{"ID":"c1","Status":"Exited"}` + "\n"), nil
		case "images":
			return []byte(`{"ID":"i1","Repository":"alpine","Tag":"3.20"}` + "\n"), nil
		case "stop":
			gen.Store(1)
			return []byte("c1\n"), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}

	// Phase 1: r1 and r2 run concurrently -> singleflight coalesces to 1 ps + 1 images.
	s1 := newMultiSender(2)
	m.SetSender(s1)
	m.Handle(&agentpb.DockerOp{OpId: "r1", Op: "ps_images"})
	<-psStarted
	m.Handle(&agentpb.DockerOp{OpId: "r2", Op: "ps_images"})
	time.Sleep(25 * time.Millisecond)
	close(releasePs)

	select {
	case <-s1.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for coalesced reads")
	}
	if got := psCount.Load(); got != 1 {
		t.Fatalf("coalesced ps_images calls = %d CLI executions, want 1", got)
	}
	for _, id := range []string{"r1", "r2"} {
		if ev := s1.evs[id]; ev == nil || !ev.GetOk() {
			t.Fatalf("%s failed: %v", id, ev)
		}
	}

	// Phase 2: r3 starts pre-mutation and blocks; mutating "stop" finishes while r3
	// is still running, then r4 arrives before r3 unblocks. r4 must NOT join r3's
	// stale flight and must wait on readSem to observe post-mutation "Exited".
	releaseMu.Lock()
	releasePs = make(chan struct{})
	gate2 := releasePs
	releaseMu.Unlock()

	s2 := newMultiSender(3)
	m.SetSender(s2)
	m.Handle(&agentpb.DockerOp{OpId: "r3", Op: "ps_images"})
	<-psStarted
	m.Handle(&agentpb.DockerOp{OpId: "mut", Op: "stop", Container: "c1"})
	deadline := time.Now().Add(2 * time.Second)
	for gen.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if gen.Load() == 0 {
		t.Fatal("stop op did not run in time")
	}
	m.Handle(&agentpb.DockerOp{OpId: "r4", Op: "ps_images"})
	time.Sleep(25 * time.Millisecond)
	close(gate2)

	select {
	case <-s2.done:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for post-mutation invalidation")
	}
	if got := psCount.Load(); got != 3 {
		t.Errorf("total ps executions after post-mutation read = %d, want 3", got)
	}
	if ev4 := s2.evs["r4"]; ev4 == nil || !ev4.GetOk() || !strings.Contains(string(ev4.GetPayloadJson()), `"Exited"`) {
		t.Errorf("r4 should observe post-mutation Exited state, got %v", ev4)
	}
}
