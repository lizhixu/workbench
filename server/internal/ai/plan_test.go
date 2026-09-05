package ai

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"watchman/server/internal/rpc"
)

// fakePolicy is a stub policyChecker that flags any command containing a
// configured substring as high-risk, so we can assert plan risk annotation
// without depending on the real regex policy.
type fakePolicy struct {
	highRiskSub string
	blockSub    string
}

func (f fakePolicy) CheckRisk(cmd string) (string, bool, bool, string) {
	if f.blockSub != "" && strings.Contains(cmd, f.blockSub) {
		return "blocked", true, false, "blk:" + f.blockSub
	}
	if f.highRiskSub != "" && strings.Contains(cmd, f.highRiskSub) {
		return "high", false, true, "hr:" + f.highRiskSub
	}
	return "low", false, false, ""
}

func newTestAssistant() *Assistant {
	reg := rpc.NewRegistry("", slog.Default())
	return NewAssistant("", Config{}, reg, slog.Default())
}

func TestMatchBlueprintDockerInstall(t *testing.T) {
	hc := hostPlanContext{OS: "linux", Distro: "debian", Arch: "amd64", Shell: "bash", PkgMgr: "apt"}
	plan := matchBlueprint("安装 docker", hc)
	if plan == nil {
		t.Fatal("expected docker install blueprint, got nil")
	}
	if len(plan.Steps) < 4 {
		t.Fatalf("docker install plan should have a full lifecycle, got %d steps", len(plan.Steps))
	}
	joined := ""
	for _, s := range plan.Steps {
		joined += s.Command + "\n"
	}
	if !strings.Contains(joined, "get.docker.com") {
		t.Errorf("docker install should use the official script, commands:\n%s", joined)
	}
	if !strings.Contains(joined, "docker --version") {
		t.Errorf("docker install should verify with docker --version, commands:\n%s", joined)
	}
	// Must have at least one probe step (verification).
	hasProbe := false
	for _, s := range plan.Steps {
		if s.Probe {
			hasProbe = true
		}
	}
	if !hasProbe {
		t.Error("docker install plan should contain a probe/verification step")
	}
}

func TestMatchBlueprintDistroAware(t *testing.T) {
	// CentOS/RHEL should use dnf, not apt.
	hc := hostPlanContext{OS: "linux", Distro: "centos", Arch: "amd64", Shell: "bash", PkgMgr: "dnf"}
	plan := matchBlueprint("安装 nginx", hc)
	if plan == nil {
		t.Fatal("expected nginx blueprint")
	}
	joined := ""
	for _, s := range plan.Steps {
		joined += s.Command + "\n"
	}
	if !strings.Contains(joined, "dnf install -y nginx") {
		t.Errorf("centos nginx install should use dnf, commands:\n%s", joined)
	}
	if strings.Contains(joined, "apt-get install") {
		t.Errorf("centos plan must not use apt, commands:\n%s", joined)
	}
}

func TestMatchBlueprintCleanupBeforeInstall(t *testing.T) {
	// "清理 docker 缓存" contains "docker" but must map to cleanup, not install.
	hc := hostPlanContext{OS: "linux", Distro: "ubuntu", Arch: "amd64", Shell: "bash", PkgMgr: "apt"}
	plan := matchBlueprint("清理未使用的 docker 镜像与缓存", hc)
	if plan == nil {
		t.Fatal("expected docker cleanup blueprint")
	}
	if !strings.Contains(plan.Title, "清理") {
		t.Errorf("expected cleanup plan, got title %q", plan.Title)
	}
	joined := ""
	for _, s := range plan.Steps {
		joined += s.Command + "\n"
	}
	if !strings.Contains(joined, "prune") {
		t.Errorf("cleanup plan should use docker prune, commands:\n%s", joined)
	}
	if strings.Contains(joined, "get.docker.com") {
		t.Errorf("cleanup must not be the install blueprint, commands:\n%s", joined)
	}
}

func TestMatchBlueprintProbesAreReadOnly(t *testing.T) {
	hc := hostPlanContext{OS: "linux", Distro: "debian", Arch: "amd64", Shell: "bash", PkgMgr: "apt"}
	for _, prompt := range []string{"排查 80 端口占用", "排查 CPU 负载最高的进程", "排查大于 100M 的大文件", "排查内存占用"} {
		plan := matchBlueprint(prompt, hc)
		if plan == nil {
			t.Fatalf("expected a diagnostic blueprint for %q", prompt)
		}
		for _, s := range plan.Steps {
			if !s.Probe && !s.ContinueOnError {
				t.Errorf("diagnostic %q step %q should be a read-only probe, command=%q", prompt, s.Title, s.Command)
			}
		}
	}
}

func TestMatchBlueprintNoMatch(t *testing.T) {
	hc := hostPlanContext{OS: "linux", Distro: "debian", Arch: "amd64", Shell: "bash", PkgMgr: "apt"}
	if plan := matchBlueprint("给我讲个笑话", hc); plan != nil {
		t.Errorf("unrelated prompt should not match any blueprint, got %q", plan.Title)
	}
}

func TestFinalizePlanRiskAnnotation(t *testing.T) {
	a := newTestAssistant()
	a.SetPolicyChecker(fakePolicy{highRiskSub: "rm -rf", blockSub: "mkfs"})
	plan := &TaskPlanResponse{
		Goal: "test",
		Steps: []TaskPlanStep{
			{Title: "safe", Command: "ls -la"},
			{Title: "danger", Command: "rm -rf /tmp/x"},
			{Title: "check", Command: "df -h", Probe: true},
		},
	}
	hc := hostPlanContext{OS: "linux", Distro: "debian", Arch: "amd64", Shell: "bash"}
	a.finalizePlan(plan, hc, "blueprint", "")

	if plan.Steps[0].Index != 1 || plan.Steps[2].Index != 3 {
		t.Errorf("steps should be 1-indexed in order, got %d and %d", plan.Steps[0].Index, plan.Steps[2].Index)
	}
	if plan.Steps[0].RiskLevel != "low" {
		t.Errorf("safe step should be low risk, got %q", plan.Steps[0].RiskLevel)
	}
	if plan.Steps[1].RiskLevel != "high" || !plan.Steps[1].NeedsConfirm {
		t.Errorf("rm -rf step should be high risk + needs_confirm, got %q confirm=%v", plan.Steps[1].RiskLevel, plan.Steps[1].NeedsConfirm)
	}
	if plan.Steps[1].MatchedPattern == "" {
		t.Error("high-risk step should carry the matched policy pattern")
	}
	if plan.RiskLevel != "high" {
		t.Errorf("overall risk should escalate to high, got %q", plan.RiskLevel)
	}
	if plan.OS != "linux" || plan.Shell != "bash" {
		t.Errorf("plan should carry host context, got os=%q shell=%q", plan.OS, plan.Shell)
	}
}

func TestPlanTaskUsesBlueprintWithoutLLM(t *testing.T) {
	// No LLM configured; a blueprint intent should still succeed.
	a := newTestAssistant()
	plan, err := a.PlanTask(context.Background(), TaskPlanRequest{Prompt: "安装 docker"})
	if err != nil {
		t.Fatalf("blueprint plan should not require LLM: %v", err)
	}
	if plan.Source != "blueprint" {
		t.Errorf("expected blueprint source, got %q", plan.Source)
	}
	// Default (unknown) host resolves to linux/bash, so steps must be populated.
	if len(plan.Steps) == 0 {
		t.Error("plan should have steps")
	}
}

func TestPlanTaskNonBlueprintWithoutLLMFails(t *testing.T) {
	a := newTestAssistant()
	_, err := a.PlanTask(context.Background(), TaskPlanRequest{Prompt: "帮我优化一下 postgres 的查询计划"})
	if err == nil {
		t.Error("a non-blueprint intent with no LLM configured should return a helpful error")
	}
}

func TestPlanTaskEmptyPrompt(t *testing.T) {
	a := newTestAssistant()
	if _, err := a.PlanTask(context.Background(), TaskPlanRequest{Prompt: "   "}); err == nil {
		t.Error("empty prompt should error")
	}
}

func TestExtractFirstJSONObject(t *testing.T) {
	in := "这是方案：{\"title\":\"x\",\"steps\":[]} 以上。"
	got := extractFirstJSONObject(in)
	if got != "{\"title\":\"x\",\"steps\":[]}" {
		t.Errorf("extractFirstJSONObject = %q", got)
	}
	if stripCodeFence("```json\n{\"a\":1}\n```") != "{\"a\":1}" {
		t.Errorf("stripCodeFence failed: %q", stripCodeFence("```json\n{\"a\":1}\n```"))
	}
}
