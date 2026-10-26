package apps

import (
	"strings"
	"testing"
)

// The deploy preflight must tell "not installed" apart from "daemon down":
// the fix the user needs differs (one-click install vs start the service),
// and a bare exit code is not an actionable deployment failure.
func TestClassifyDockerPreflight(t *testing.T) {
	cases := []struct {
		code int32
		want string // "" means success; otherwise a substring of the message
	}{
		{code: 0, want: ""},
		{code: deployDockerMissing, want: "未安装 Docker"},
		{code: deployDockerMissing, want: "一键安装 Docker"},
		{code: deployDockerDaemonDown, want: "Docker 服务未运行"},
		{code: deployDockerDaemonDown, want: "启动 Docker"},
		{code: 127, want: "Docker 不可用"},
		{code: 1, want: "exit 1"},
	}
	for _, tc := range cases {
		err := classifyDockerPreflight(tc.code)
		if tc.want == "" {
			if err != nil {
				t.Errorf("code %d: expected nil, got %v", tc.code, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("code %d: expected error containing %q, got nil", tc.code, tc.want)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("code %d: error %q does not contain %q", tc.code, err.Error(), tc.want)
		}
	}
}

func TestDockerPreflightScript(t *testing.T) {
	script := dockerPreflightScript()
	// The two checks must stay in order with distinct exits: binary first
	// (missing binary must not be reported as a dead daemon).
	if !strings.Contains(script, "command -v docker") {
		t.Errorf("script %q: missing binary probe", script)
	}
	if !strings.Contains(script, "docker info") {
		t.Errorf("script %q: missing daemon probe", script)
	}
	if idxBin, idxInfo := strings.Index(script, "command -v docker"), strings.Index(script, "docker info"); idxBin < 0 || idxInfo < idxBin {
		t.Errorf("script %q: binary probe must precede daemon probe", script)
	}
}
