package ai

import (
	"fmt"
	"strings"
	"time"
)

// CallStep is one stage of an AI call, rendered in the UI as a Codex-style
// timeline row. Durations are measured server-side around the real work —
// nothing here is simulated, so a step only appears once it actually ran.
type CallStep struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Status     string `json:"status"` // done | failed
	DurationMs int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

// CallTrace is the ordered step list of one AI call plus its wall-clock total.
type CallTrace struct {
	Steps   []CallStep `json:"steps"`
	TotalMs int64      `json:"total_ms"`
}

// LLMUsage carries token counts when the provider reports them (OpenAI
// `usage`, Ollama eval counts). Nil means the provider reported nothing —
// the UI then shows no usage line instead of a fabricated zero.
type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// summary renders a short Chinese usage line, or "" when nothing was reported.
func (u *LLMUsage) summary() string {
	if u == nil {
		return ""
	}
	if u.PromptTokens > 0 || u.CompletionTokens > 0 {
		return fmt.Sprintf("输入 %d tokens / 输出 %d tokens", u.PromptTokens, u.CompletionTokens)
	}
	if u.TotalTokens > 0 {
		return fmt.Sprintf("共 %d tokens", u.TotalTokens)
	}
	return ""
}

// callTracer records steps for one AI call.
type callTracer struct {
	steps []CallStep
	start time.Time
}

func newCallTracer() *callTracer {
	return &callTracer{start: time.Now()}
}

// run executes fn, appends a done/failed step with its real duration, and
// returns fn's results. On failure the step detail falls back to the error
// so the timeline shows why the call stopped.
func (t *callTracer) run(key, label string, fn func() (string, error)) (string, error) {
	started := time.Now()
	detail, err := fn()
	status := "done"
	if err != nil {
		status = "failed"
		if detail == "" {
			detail = err.Error()
		}
	}
	t.steps = append(t.steps, CallStep{
		Key:        key,
		Label:      label,
		Status:     status,
		DurationMs: time.Since(started).Milliseconds(),
		Detail:     detail,
	})
	return detail, err
}

func (t *callTracer) trace() *CallTrace {
	return &CallTrace{Steps: t.steps, TotalMs: time.Since(t.start).Milliseconds()}
}

// TracedError wraps a failed AI call together with the partial trace of the
// steps that ran before the failure, so handlers can return it and the UI
// can still render which stage failed instead of a bare error string.
type TracedError struct {
	Trace *CallTrace
	Err   error
}

func (e *TracedError) Error() string { return e.Err.Error() }
func (e *TracedError) Unwrap() error { return e.Err }

// summarizeFacts renders one short line describing what probeAppFacts
// actually found, for the facts step detail. Probe failures are reported
// as-is; nothing is invented when no image or repo was recognized.
func summarizeFacts(facts *appFacts) string {
	if facts == nil {
		return "未执行事实探测"
	}
	var parts []string
	if facts.image != nil {
		ref := facts.image.Reference
		if ref == "" {
			ref = facts.imageRef
		}
		if facts.image.Err != "" {
			parts = append(parts, fmt.Sprintf("镜像 %s 探测失败：%s", ref, facts.image.Err))
		} else {
			parts = append(parts, fmt.Sprintf("镜像 %s：%d 个端口、%d 个环境变量、%d 个挂载卷",
				ref, len(facts.image.Ports), len(facts.image.EnvVars), len(facts.image.Volumes)))
		}
	}
	if facts.repo != nil {
		url := facts.repo.RepoURL
		if url == "" {
			url = facts.repoURL
		}
		if facts.repo.Err != "" {
			parts = append(parts, fmt.Sprintf("仓库 %s 探测失败：%s", url, facts.repo.Err))
		} else {
			kind := "仓库"
			if facts.repo.HasDockerfile {
				kind += "（Dockerfile）"
			}
			if facts.repo.HasCompose {
				kind += "（Compose）"
			}
			parts = append(parts, fmt.Sprintf("%s %s（分支 %s）", kind, url, facts.repo.Branch))
		}
	}
	if len(parts) == 0 {
		return "表单与需求中未识别到镜像或仓库，无事实可探测"
	}
	return strings.Join(parts, "；")
}
