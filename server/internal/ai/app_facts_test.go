package ai

import (
	"strings"
	"testing"
)

func TestFindImageRefInText(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"帮我安装 ghcr.io/xerxes-2/clewdr 这台服务", "ghcr.io/xerxes-2/clewdr"},
		{"部署 registry.example.com:5000/team/app:v2 到生产", "registry.example.com:5000/team/app:v2"},
		{"我想装个 nginx", ""},         // single unqualified name: not probed from prose
		{"每天 12:30 重启", ""},          // time-like token must not match
		{"看一下 https://github.com/a/b", ""}, // URLs excluded
		{"", ""},
	}
	for _, c := range cases {
		if got := findImageRefInText(c.text); got != c.want {
			t.Errorf("findImageRefInText(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestFindRepoURLInText(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"部署 https://github.com/xerxes-2/clewdr 这个项目", "https://github.com/xerxes-2/clewdr"},
		{"repo: https://github.com/a/b.git ok", "https://github.com/a/b"},
		{"https://gitlab.com/a/b 支持吗", ""},
		{"没有仓库", ""},
	}
	for _, c := range cases {
		if got := findRepoURLInText(c.text); got != c.want {
			t.Errorf("findRepoURLInText(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func testImageFacts() *appFacts {
	return &appFacts{
		imageRef: "ghcr.io/xerxes-2/clewdr",
		image: &AppImageProbe{
			Reference: "ghcr.io/xerxes-2/clewdr:latest",
			Ports:     []int{8484},
			EnvVars:   map[string]string{"CLEWDR_PORT": "8484"},
		},
	}
}

func warnCount(list []string, sub string) int {
	n := 0
	for _, w := range list {
		if strings.Contains(w, sub) {
			n++
		}
	}
	return n
}

func TestAlignDraftWithFactsImagePrefill(t *testing.T) {
	d := &AppDraft{SourceType: strPtr("image"), Image: strPtr("ghcr.io/xerxes-2/clewdr")}
	_, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates(), testImageFacts())
	if len(d.Ports) != 1 || d.Ports[0].Container != 8484 || d.Ports[0].Host != 8484 {
		t.Fatalf("ports not prefilled from facts: %+v", d.Ports)
	}
	if warnCount(warnings, "预填") != 1 {
		t.Fatalf("expected prefill warning, got %v", warnings)
	}
}

func TestAlignDraftWithFactsImageCorrectsHallucination(t *testing.T) {
	// Model invented 80:8080 while the image only exposes 8484.
	d := &AppDraft{
		SourceType: strPtr("image"),
		Image:      strPtr("ghcr.io/xerxes-2/clewdr"),
		Ports:      []AppPortMapping{{Host: 80, Container: 8080, BindScope: "public"}},
	}
	_, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates(), testImageFacts())
	if len(d.Ports) != 1 || d.Ports[0].Container != 8484 {
		t.Fatalf("hallucinated port was not aligned: %+v", d.Ports)
	}
	if warnCount(warnings, "不符") != 1 {
		t.Fatalf("expected alignment warning, got %v", warnings)
	}
}

func TestAlignDraftWithFactsImageKeepsCorrect(t *testing.T) {
	d := &AppDraft{
		SourceType: strPtr("image"),
		Image:      strPtr("ghcr.io/xerxes-2/clewdr"),
		Ports:      []AppPortMapping{{Host: 80, Container: 8484, BindScope: "public"}},
	}
	_, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates(), testImageFacts())
	// User-chosen host port 80 mapping onto the real container port 8484 is
	// legitimate and must be preserved.
	if len(d.Ports) != 1 || d.Ports[0].Host != 80 || d.Ports[0].Container != 8484 {
		t.Fatalf("correct mapping was tampered with: %+v", d.Ports)
	}
	if warnCount(warnings, "不符") != 0 {
		t.Fatalf("unexpected alignment warning: %v", warnings)
	}
}

func TestAlignDraftWithFactsImageSkipsDifferentImage(t *testing.T) {
	// Facts belong to a different image than the one the draft ends up with:
	// no enforcement may happen.
	d := &AppDraft{
		SourceType: strPtr("image"),
		Image:      strPtr("ghcr.io/other/svc"),
		Ports:      []AppPortMapping{{Host: 8080, Container: 8080, BindScope: "public"}},
	}
	_, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates(), testImageFacts())
	if len(d.Ports) != 1 || d.Ports[0].Container != 8080 {
		t.Fatalf("unrelated draft ports were tampered with: %+v", d.Ports)
	}
	if warnCount(warnings, "不符") != 0 {
		t.Fatalf("unexpected alignment warning: %v", warnings)
	}
}

func TestAlignDraftWithFactsRepoBuildType(t *testing.T) {
	facts := &appFacts{
		repoURL: "https://github.com/acme/stack",
		repo: &AppRepoProbe{
			RepoURL:   "https://github.com/acme/stack",
			Branch:    "main",
			HasCompose: true,
			ComposeFile: "compose.yaml",
		},
	}
	// Defaults to dockerfile while only compose exists -> flip to compose.
	d := &AppDraft{SourceType: strPtr("git"), RepoURL: strPtr("https://github.com/acme/stack")}
	_, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates(), facts)
	if d.BuildType == nil || *d.BuildType != "compose" {
		t.Fatalf("build_type not flipped to compose: %+v", d.BuildType)
	}
	if warnCount(warnings, "构建方式") != 1 {
		t.Fatalf("expected build_type warning, got %v", warnings)
	}
}

func TestAlignDraftWithFactsRepoDockerfilePathAndPorts(t *testing.T) {
	facts := &appFacts{
		repoURL: "https://github.com/acme/svc",
		repo: &AppRepoProbe{
			RepoURL:        "https://github.com/acme/svc",
			Branch:         "main",
			HasDockerfile:  true,
			DockerfilePath: "docker/Dockerfile",
			ExposedPorts:   []int{9000},
		},
	}
	d := &AppDraft{SourceType: strPtr("git"), RepoURL: strPtr("https://github.com/Acme/svc")}
	_, warnings := sanitizeAppDraft(d, AppFormSnapshot{}, testHosts(), testTemplates(), facts)
	if d.Dockerfile == nil || *d.Dockerfile != "docker/Dockerfile" {
		t.Fatalf("dockerfile path not adopted: %+v", d.Dockerfile)
	}
	if len(d.Ports) != 1 || d.Ports[0].Container != 9000 {
		t.Fatalf("dockerfile EXPOSE ports not prefilled: %+v", d.Ports)
	}
	if warnCount(warnings, "预填") != 1 {
		t.Fatalf("expected prefill warning, got %v", warnings)
	}
}

func TestFactsContextMasksSecrets(t *testing.T) {
	facts := &appFacts{
		imageRef: "registry.example.com/app",
		image: &AppImageProbe{
			Reference: "registry.example.com/app:latest",
			Ports:     []int{8080},
			EnvVars: map[string]string{
				"APP_PASSWORD": "hunter2",
				"APP_MODE":     "prod",
			},
		},
	}
	ctx := factsContext(facts)
	if ctx == "" {
		t.Fatal("empty facts context")
	}
	if strings.Contains(ctx, "hunter2") {
		t.Fatalf("secret env value leaked into prompt: %s", ctx)
	}
	if !strings.Contains(ctx, "APP_PASSWORD=***") || !strings.Contains(ctx, "APP_MODE=prod") {
		t.Fatalf("env summary wrong: %s", ctx)
	}
	if !strings.Contains(ctx, "8080") {
		t.Fatalf("port missing from context: %s", ctx)
	}
}

func TestFactsContextProbeFailure(t *testing.T) {
	facts := &appFacts{
		imageRef: "registry.example.com/private/app",
		image:    &AppImageProbe{Err: "镜像仓库拒绝匿名读取（可能为私有镜像）"},
	}
	ctx := factsContext(facts)
	if !strings.Contains(ctx, "探测失败") {
		t.Fatalf("failure not surfaced: %s", ctx)
	}
}
