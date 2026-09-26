package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ---- Multi-step task planning (Terminal AI Copilot) --------------------
//
// PlanTask turns a high-level operational intent (e.g. "安装 docker") into an
// ordered, executable multi-step plan tailored to the target host's OS and
// distribution. Built-in offline blueprints cover the most common operations
// so the feature works with zero external LLM configuration; when an LLM is
// configured and no blueprint matches, the model generates the plan under a
// strict JSON schema. Every generated command is run through the command
// policy so high-risk steps are flagged for confirmation before execution.

// TaskPlanRequest is the input for a plan request.
type TaskPlanRequest struct {
	Prompt string `json:"prompt"`
	HostID string `json:"host_id"`
}

// TaskPlanStep is one ordered step in an automation plan.
type TaskPlanStep struct {
	Index           int    `json:"index"`
	Title           string `json:"title"`                      // short label (e.g. "安装 Docker")
	Description     string `json:"description"`                // what this step does / why
	Command         string `json:"command"`                    // shell command to run
	RiskLevel       string `json:"risk_level"`                 // low / medium / high
	NeedsConfirm    bool   `json:"needs_confirm"`              // true => pause for human confirm
	Probe           bool   `json:"probe,omitempty"`            // read-only check / verification step
	ContinueOnError bool   `json:"continue_on_error,omitempty"` // best-effort; don't abort plan on failure
	MatchedPattern  string `json:"matched_pattern,omitempty"`  // policy pattern that raised the risk
}

// TaskPlanResponse is the full generated plan.
type TaskPlanResponse struct {
	Title     string         `json:"title"`
	Goal      string         `json:"goal"`
	OS        string         `json:"os"`
	Distro    string         `json:"distro"`
	Arch      string         `json:"arch"`
	Shell     string         `json:"shell"`
	Summary   string         `json:"summary"`    // adaptation note for the target host
	RiskLevel string         `json:"risk_level"` // overall risk (max of steps)
	Steps     []TaskPlanStep `json:"steps"`
	Source    string         `json:"source"` // blueprint / llm
	Model     string         `json:"model,omitempty"`
}

// hostPlanContext captures everything a blueprint or the LLM needs to know
// about the target host to tailor commands.
type hostPlanContext struct {
	Hostname string
	OS       string // linux / windows / darwin
	Distro   string // debian / ubuntu / centos / rhel / alpine / windows ...
	Arch     string // amd64 / arm64
	Shell    string // bash / sh / powershell
	PkgMgr   string // apt / dnf / yum / apk / (empty on windows)
	IsWindows bool
}

// resolveHostContext builds a plan context from the live registry. When the
// host is unknown (offline / not enrolled), it returns a Linux/bash default so
// blueprints still produce a reasonable generic plan.
func (a *Assistant) resolveHostContext(hostID string) hostPlanContext {
	hc := hostPlanContext{OS: "linux", Arch: "amd64", Shell: "bash", Distro: "linux", PkgMgr: "apt"}
	host := a.reg.GetAgent(hostID)
	if host == nil {
		return hc
	}
	hc.Hostname = host.Hostname
	if host.OS != "" {
		hc.OS = strings.ToLower(host.OS)
	}
	if host.Arch != "" {
		hc.Arch = strings.ToLower(host.Arch)
	}
	hc.Distro = strings.ToLower(strings.TrimSpace(host.Distro))
	lower := hc.OS + " " + hc.Distro
	switch {
	case strings.Contains(lower, "windows"):
		hc.IsWindows = true
		hc.OS = "windows"
		hc.Shell = "powershell"
		hc.PkgMgr = ""
	case strings.Contains(lower, "alpine"):
		hc.PkgMgr = "apk"
	case strings.Contains(lower, "centos"), strings.Contains(lower, "rhel"),
		strings.Contains(lower, "red hat"), strings.Contains(lower, "rocky"),
		strings.Contains(lower, "alma"), strings.Contains(lower, "fedora"):
		hc.PkgMgr = "dnf"
	case strings.Contains(lower, "debian"), strings.Contains(lower, "ubuntu"),
		strings.Contains(lower, "mint"), strings.Contains(lower, "kali"):
		hc.PkgMgr = "apt"
	}
	if hc.Distro == "" {
		hc.Distro = hc.OS
	}
	return hc
}

// PlanTask produces an executable multi-step plan for the given intent.
func (a *Assistant) PlanTask(ctx context.Context, req TaskPlanRequest) (*TaskPlanResponse, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("请描述你想完成的运维目标")
	}
	hc := a.resolveHostContext(req.HostID)

	// 1. Built-in offline blueprints handle the most common operations without
	// needing any external LLM. This keeps the feature working out of the box.
	if plan := matchBlueprint(prompt, hc); plan != nil {
		a.finalizePlan(plan, hc, "blueprint", "")
		return plan, nil
	}

	// 2. No blueprint matched: fall back to the LLM if configured.
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("该任务未匹配到内置运维蓝图，且 AI 大模型未配置。请在「系统设置」-「AI 大模型配置」中启用后重试，或换用内置支持的任务（如：安装 Docker、排查端口占用、清理 Docker 缓存、排查大文件、排查内存占用）。")
	}
	plan, err := a.planWithLLM(ctx, prompt, hc)
	if err != nil {
		return nil, err
	}
	a.finalizePlan(plan, hc, "llm", cfg.Model)
	return plan, nil
}

// finalizePlan fills host metadata, re-indexes steps, runs every command
// through the command policy for risk annotation, and computes overall risk.
func (a *Assistant) finalizePlan(plan *TaskPlanResponse, hc hostPlanContext, source, model string) {
	plan.OS = hc.OS
	plan.Distro = hc.Distro
	plan.Arch = hc.Arch
	plan.Shell = hc.Shell
	plan.Source = source
	plan.Model = model
	if plan.Title == "" {
		plan.Title = plan.Goal
	}

	checker := a.policyCheckerRef()
	overall := "low"
	for i := range plan.Steps {
		s := &plan.Steps[i]
		s.Index = i + 1
		cmd := strings.TrimSpace(s.Command)
		// Policy is the authoritative risk source; the model's own risk_level is
		// only a hint and can be escalated (never silently downgraded) by policy.
		if cmd != "" && checker != nil {
			level, blocked, needsConfirm, pattern := checker.CheckRisk(cmd)
			if blocked {
				s.RiskLevel = "high"
				s.NeedsConfirm = true
				s.MatchedPattern = pattern
			} else if level == "high" || needsConfirm {
				s.RiskLevel = "high"
				s.NeedsConfirm = true
				s.MatchedPattern = pattern
			}
		}
		// Read-only probe steps never need confirmation.
		if s.Probe && s.RiskLevel != "high" {
			s.NeedsConfirm = false
		}
		if s.RiskLevel == "" {
			s.RiskLevel = "low"
		}
		if riskRank(s.RiskLevel) > riskRank(overall) {
			overall = s.RiskLevel
		}
	}
	if plan.RiskLevel == "" || riskRank(overall) > riskRank(plan.RiskLevel) {
		plan.RiskLevel = overall
	}
}

func riskRank(level string) int {
	switch strings.ToLower(level) {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

// planWithLLM asks the configured model to produce a plan under a strict JSON
// schema, then parses it (tolerating code fences and surrounding prose).
func (a *Assistant) planWithLLM(ctx context.Context, prompt string, hc hostPlanContext) (*TaskPlanResponse, error) {
	shellNote := "命令必须是可在 " + hc.Shell + " 中直接执行的非交互式命令"
	pkgNote := ""
	if hc.PkgMgr != "" {
		pkgNote = fmt.Sprintf("该主机的包管理器是 %s，安装类操作请使用它并加上自动确认参数（如 apt-get install -y / dnf install -y）。", hc.PkgMgr)
	}
	sys := fmt.Sprintf(`你是一名资深的 Linux/Windows 自动化运维专家。请把用户的运维目标拆解为一份【可自动化顺序执行】的多步骤方案，适配下面这台目标主机。

目标主机:
- 主机名: %s
- 操作系统: %s
- 发行版: %s
- 架构: %s
- 默认Shell: %s
%s

用户目标: %s

拆解要求:
1. 步骤要覆盖【前置检查 → 执行安装/操作 → 启动服务 → 验证探活】的完整闭环，顺序合理。
2. %s；不得使用需要交互输入的命令（一律加 -y、--noninteractive、DEBIAN_FRONTEND=noninteractive 等）。
3. 只读检查/验证类步骤把 "probe" 设为 true。
4. 破坏性或高危命令（rm -rf、reboot、mkfs、drop 等）把 risk_level 设为 "high" 且 needs_confirm 设为 true。
5. 允许失败不影响后续的步骤（如可选清理）把 "continue_on_error" 设为 true。
6. 步骤数控制在 3-8 步，命令尽量精炼可靠。

严格按如下 JSON 输出（不要输出 JSON 以外的任何内容，不要使用 markdown 代码块）:
{
  "title": "任务标题",
  "goal": "一句话目标",
  "summary": "针对该主机发行版的适配说明(1-2句)",
  "steps": [
    {"title":"步骤标题","description":"该步骤做什么","command":"要执行的命令","risk_level":"low|medium|high","needs_confirm":false,"probe":false,"continue_on_error":false}
  ]
}`, hc.Hostname, hc.OS, hc.Distro, hc.Arch, hc.Shell, pkgNote, prompt, shellNote)

	answer, err := a.callLLM(ctx, sys)
	if err != nil {
		return nil, err
	}
	clean := stripCodeFence(answer)
	var plan TaskPlanResponse
	if err := json.Unmarshal([]byte(clean), &plan); err != nil || len(plan.Steps) == 0 {
		// Some models wrap the object in prose; try to salvage the first {...}.
		if obj := extractFirstJSONObject(clean); obj != "" {
			if err2 := json.Unmarshal([]byte(obj), &plan); err2 == nil && len(plan.Steps) > 0 {
				return &plan, nil
			}
		}
		return nil, fmt.Errorf("AI 未能生成有效的任务规划，请重试或换一种描述")
	}
	return &plan, nil
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimSuffix(s, "```")
	} else if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

func extractFirstJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return ""
}

