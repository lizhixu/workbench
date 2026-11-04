package docker

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
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
