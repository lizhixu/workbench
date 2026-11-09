package docker

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
