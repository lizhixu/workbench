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
