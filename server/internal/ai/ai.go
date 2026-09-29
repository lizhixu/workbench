// Package ai implements an AI-assisted diagnostics feature for the control
// server. It connects to an LLM endpoint (Ollama, DeepSeek, OpenAI, vLLM, etc.)
// and lets operators ask questions about a host's state, generate shell commands,
// and analyze command execution errors.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"watchman/proto/agentpb"
	"watchman/server/internal/rpc"
)

// Config holds the LLM endpoint settings.
type Config struct {
	BaseURL  string `json:"base_url"` // e.g. https://api.deepseek.com or http://localhost:11434
	Model    string `json:"model"`    // e.g. "deepseek-chat" or "qwen2.5:14b"
	APIKey   string `json:"api_key"`  // for OpenAI/DeepSeek/vLLM servers; empty for Ollama
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider,omitempty"` // "deepseek", "openai", "ollama", "custom"
	// Headers are user-defined HTTP headers sent with every LLM request
	// (e.g. for gateways/proxies that require extra auth headers).
	Headers map[string]string `json:"headers,omitempty"`
}

// Assistant wraps the LLM endpoint and provides host diagnostic helpers.
type Assistant struct {
	mu           sync.RWMutex
	cfg          Config
	dataDir      string
	reg          *rpc.Registry
	log          *slog.Logger
	metricsStore metricsStoreRef
	alertStore   alertStoreRef
	reports      *reportStore
	policy       policyChecker
}

// policyChecker is a minimal interface over the command policy store, used to
// flag high-risk steps in generated plans without importing the policy package
// (which would otherwise pull the audit/regex machinery into ai). It mirrors
// policy.Store.Check without the concrete return type.
type policyChecker interface {
	CheckRisk(command string) (riskLevel string, blocked bool, needsConfirm bool, matchedPattern string)
}

// SetPolicyChecker injects the command policy so PlanTask can annotate step
// risk. Safe to call with nil (risk annotation falls back to a built-in
// heuristic).
func (a *Assistant) SetPolicyChecker(p policyChecker) {
	a.mu.Lock()
	a.policy = p
	a.mu.Unlock()
}

func (a *Assistant) policyCheckerRef() policyChecker {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.policy
}

// metricsStoreRef is a minimal interface to avoid a circular import with the
// metrics package (which doesn't depend on ai, but we keep the boundary clean).
type metricsStoreRef interface {
	QueryHistory(hostID string, from, to int64, stepSec int) []metricsPoint
}

// alertStoreRef is a minimal interface for reading alert events without
// importing the alert package (which imports ai for interpretation).
type alertStoreRef interface {
	ListEvents(limit int) []AlertEventProxy
}

// metricsPoint mirrors metrics.Point (re-declared to avoid import cycle).
type metricsPoint struct {
	Timestamp int64
	CPUUsage  float64
	MemUsage  float64
	MemTotal  int64
	MemUsed   int64
	NetRx     float64
	NetTx     float64
	DiskRead  float64
	DiskWrite float64
}

// AlertEventProxy mirrors alert.Event (exported so main can build adapters).
type AlertEventProxy struct {
	ID        string
	RuleName  string
	Severity  string
	HostID    string
	Hostname  string
	Message   string
	FiredAt   time.Time
	Resolved  bool
}

// SetMetricsStore injects the metrics store for ops reports / tool calling.
func (a *Assistant) SetMetricsStore(s metricsStoreRef) {
	a.mu.Lock()
	a.metricsStore = s
	a.mu.Unlock()
}

// SetAlertStore injects the alert store for ops reports / tool calling.
func (a *Assistant) SetAlertStore(s alertStoreRef) {
	a.mu.Lock()
	a.alertStore = s
	a.mu.Unlock()
}

// NewAssistant creates an AI assistant, loading persisted config if present.
func NewAssistant(dataDir string, fallback Config, reg *rpc.Registry, log *slog.Logger) *Assistant {
	if log == nil {
		log = slog.Default()
	}

	asst := &Assistant{
		cfg:     fallback,
		dataDir: dataDir,
		reg:     reg,
		log:     log,
		reports: newReportStore(dataDir, log),
	}

	if dataDir != "" {
		cfgPath := filepath.Join(dataDir, "ai_config.json")
		if data, err := os.ReadFile(cfgPath); err == nil {
			var saved Config
			if err := json.Unmarshal(data, &saved); err == nil {
				asst.cfg = saved
				log.Info("loaded persistent AI config", "url", saved.BaseURL, "model", saved.Model, "enabled", saved.Enabled)
			}
		}
	}

	return asst
}

// Config returns the current config (for the settings page).
func (a *Assistant) Config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg
}

// SetConfig updates the LLM endpoint settings and persists to disk.
func (a *Assistant) SetConfig(cfg Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg = cfg

	if a.dataDir != "" {
		cfgPath := filepath.Join(a.dataDir, "ai_config.json")
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
			return fmt.Errorf("save ai config: %w", err)
		}
	}
	return nil
}

// TestConnection performs a lightweight query to test LLM availability.
func (a *Assistant) TestConnection(ctx context.Context, testCfg Config) (string, error) {
	if testCfg.BaseURL == "" {
		return "", fmt.Errorf("Base URL 不能为空")
	}
	if testCfg.Model == "" {
		return "", fmt.Errorf("模型名称不能为空")
	}

	start := time.Now()
	testPrompt := "Say 'AI service connected successfully' in Chinese concisely."
	answer, err := callLLMWithConfig(ctx, testCfg, testPrompt)
	if err != nil {
		return "", err
	}
	latency := time.Since(start).Milliseconds()
	return fmt.Sprintf("连接成功 (%dms): %s", latency, strings.TrimSpace(answer)), nil
}

// DiagnoseRequest is the input for a diagnostic query.
type DiagnoseRequest struct {
	HostID  string `json:"host_id"`
	Query   string `json:"query"`   // operator's question
	Context string `json:"context"` // optional extra context (e.g., error text)
}

// DiagnoseResponse is the AI's answer.
type DiagnoseResponse struct {
	Answer     string `json:"answer"`
	Model      string `json:"model"`
	TokensUsed int    `json:"tokens_used,omitempty"`
}

// Diagnose gathers host context and asks the LLM for a diagnosis.
func (a *Assistant) Diagnose(ctx context.Context, req DiagnoseRequest) (*DiagnoseResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 诊断服务未启用或未配置端点，请前往「系统设置」-「AI 大模型配置」中配置。")
	}

	// Gather context from the host.
	hub := a.reg.Hub(req.HostID)
	if hub == nil {
		return nil, fmt.Errorf("agent offline")
	}

	hostInfo := a.reg.GetAgent(req.HostID)
	hostname := ""
	if hostInfo != nil {
		hostname = hostInfo.Hostname
	}

	metrics := hub.LastMetrics()
	systemContext := buildSystemContext(hostname, hostInfo, metrics)

	prompt := fmt.Sprintf("你是一个专业的 Linux/Windows 服务器运维专家助手。以下是被管主机的实时系统状态信息：\n\n%s\n\n", systemContext)
	if req.Context != "" {
		prompt += fmt.Sprintf("附加上下文信息：\n%s\n\n", req.Context)
	}
	prompt += fmt.Sprintf("运维人员的问题：%s\n\n请给出清晰、专业、结构化的分析和排查解决建议（使用中文回答）：", req.Query)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}

	return &DiagnoseResponse{
		Answer: answer,
		Model:  cfg.Model,
	}, nil
}

// AnalyzeExecResult asks the LLM to analyze a command's output.
func (a *Assistant) AnalyzeExecResult(ctx context.Context, command, stdout, stderr string, exitCode int32) (*DiagnoseResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 诊断服务未启用或未配置端点")
	}

	prompt := fmt.Sprintf("你是一个运维专家助手。请分析以下命令执行结果并给出诊断与修复建议：\n\n"+
		"命令: %s\n退出码: %d\n标准输出:\n%s\n标准错误:\n%s\n\n"+
		"请分析可能的问题原因和推荐解决方案（中文回答）：", command, exitCode, stdout, stderr)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return &DiagnoseResponse{Answer: answer, Model: cfg.Model}, nil
}

// Nl2CommandResponse is the output of a natural-language-to-command request.
type Nl2CommandResponse struct {
	Command      string `json:"command"`
	Explanation  string `json:"explanation"`
	RiskLevel    string `json:"risk_level"`    // low / medium / high
	NeedsConfirm bool   `json:"needs_confirm"` // true for high-risk commands
}

// Nl2Command turns a natural-language intent into a shell command.
func (a *Assistant) Nl2Command(ctx context.Context, prompt string, hostID string) (*Nl2CommandResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 辅助功能未启用或未配置端点，请在后台设置中启用。")
	}

	// Build host context for better command generation.
	hostCtx := ""
	if host := a.reg.GetAgent(hostID); host != nil {
		shell := "bash"
		if strings.Contains(strings.ToLower(host.OS+" "+host.Distro), "windows") {
			shell = "powershell"
		}
		hostCtx = fmt.Sprintf("目标主机: %s\n操作系统: %s %s\n架构: %s\n默认Shell: %s\n",
			host.Hostname, host.OS, host.Distro, host.Arch, shell)
	}

	fullPrompt := fmt.Sprintf(`你是一个运维命令生成助手。请将用户的自然语言描述转换为一条可直接在目标主机终端执行的 shell 命令。
%s
用户意图: %s

要求：
1. 只生成 1 条命令，不要包含 markdown 格式以外的废话。
2. 如果命令有破坏性（如 rm -rf, drop table, reboot, shutdown, mkfs 等），在 risk_level 中标注 "high" 并设 needs_confirm=true。
3. 严格以 JSON 格式返回，格式如下：
{"command":"具体命令","explanation":"命令简短解释","risk_level":"low|medium|high","needs_confirm":true|false}`, hostCtx, prompt)

	answer, err := a.callLLM(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	// Clean code fence blocks if returned by model
	cleanAnswer := strings.TrimSpace(answer)
	if strings.HasPrefix(cleanAnswer, "```json") {
		cleanAnswer = strings.TrimPrefix(cleanAnswer, "```json")
		cleanAnswer = strings.TrimSuffix(cleanAnswer, "```")
	} else if strings.HasPrefix(cleanAnswer, "```") {
		cleanAnswer = strings.TrimPrefix(cleanAnswer, "```")
		cleanAnswer = strings.TrimSuffix(cleanAnswer, "```")
	}
	cleanAnswer = strings.TrimSpace(cleanAnswer)

	var resp Nl2CommandResponse
	if err := json.Unmarshal([]byte(cleanAnswer), &resp); err == nil && resp.Command != "" {
		if resp.RiskLevel == "high" {
			resp.NeedsConfirm = true
		}
		return &resp, nil
	}

	// Fallback: treat the whole answer as the command text.
	return &Nl2CommandResponse{
		Command:      cleanAnswer,
		Explanation:  "",
		RiskLevel:    "low",
		NeedsConfirm: false,
	}, nil
}

// ReportResponse is the AI-generated structured analysis of a scan report.
type ReportResponse struct {
	Summary          string               `json:"summary"`
	Priorities       []PriorityItem       `json:"priorities"`
	Recommendations  []RecommendationItem `json:"recommendations"`
	Model            string               `json:"model"`
}

// PriorityItem ranks a finding by remediation urgency.
type PriorityItem struct {
	Level   string `json:"level"`   // critical / high / medium / low
	Title   string `json:"title"`
	Reason  string `json:"reason"`
}

// RecommendationItem is an actionable fix suggestion.
type RecommendationItem struct {
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Steps    string `json:"steps"`
	Command  string `json:"command,omitempty"` // optional ready-to-run fix command
}

// AnalyzeScanReport sends scan findings to the LLM and returns a structured
// Chinese report: risk overview, affected scope, remediation priorities, and
// per-finding fix commands/steps. hostID is used to enrich the prompt with
// host context (OS, hostname); it may be empty.
func (a *Assistant) AnalyzeScanReport(ctx context.Context, scanType, findingsJSON, hostID string) (*ReportResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 报告解读服务未启用或未配置端点，请前往「系统设置」-「AI 大模型配置」中配置。")
	}

	// Pretty-print findings for the model if possible.
	findingsText := findingsJSON
	var findings []map[string]any
	if json.Unmarshal([]byte(findingsJSON), &findings) == nil && len(findings) > 0 {
		if pretty, err := json.MarshalIndent(findings, "", "  "); err == nil {
			findingsText = string(pretty)
		}
	} else if len(findings) == 0 {
		findingsText = "[]（未发现风险项）"
	}

	hostCtx := ""
	if host := a.reg.GetAgent(hostID); host != nil {
		hostCtx = fmt.Sprintf("目标主机: %s\n操作系统: %s %s\n架构: %s\n", host.Hostname, host.OS, host.Distro, host.Arch)
	}

	typeName := map[string]string{
		"baseline":  "安全基线扫描",
		"intrusion": "入侵痕迹排查",
		"vuln":      "漏洞扫描",
	}[scanType]
	if typeName == "" {
		typeName = "安全扫描"
	}

	prompt := fmt.Sprintf(`你是一名资深的安全运维专家。请对以下%s结果进行解读，生成结构化的中文安全报告。

%s
扫描发现（JSON 数组，每条含 category/severity/title/detail/suggestion 字段）：
%s

请严格按如下 JSON 格式输出（不要输出 JSON 以外的内容，不要使用 markdown 代码块）：
{
  "summary": "对整体安全态势的中文摘要，说明风险概述与影响范围（2-4 句）",
  "priorities": [
    {"level":"critical|high|medium|low","title":"风险项标题","reason":"为什么需要优先处置"}
  ],
  "recommendations": [
    {"title":"修复建议标题","severity":"critical|high|medium|low","steps":"具体修复步骤（中文，可分步描述）","command":"可直接执行的修复命令（如无需命令可留空）"}
  ]
}
要求：
1. priorities 按 severity 从高到低排序。
2. recommendations 给出可执行的修复步骤，命令需与目标主机操作系统匹配。
3. 若发现项为空，summary 说明该主机在此扫描维度表现良好，priorities 与 recommendations 留空数组。`,
		typeName, hostCtx, findingsText)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}

	// Clean code fences if present.
	clean := strings.TrimSpace(answer)
	if strings.HasPrefix(clean, "```json") {
		clean = strings.TrimPrefix(clean, "```json")
		clean = strings.TrimSuffix(clean, "```")
	} else if strings.HasPrefix(clean, "```") {
		clean = strings.TrimPrefix(clean, "```")
		clean = strings.TrimSuffix(clean, "```")
	}
	clean = strings.TrimSpace(clean)

	var resp ReportResponse
	if err := json.Unmarshal([]byte(clean), &resp); err == nil && resp.Summary != "" {
		resp.Model = cfg.Model
		return &resp, nil
	}

	// Fallback: treat raw answer as summary.
	return &ReportResponse{
		Summary: clean,
		Model:   cfg.Model,
	}, nil
}

// ChatFollowup answers a follow-up question about a previously generated
// scan report. reportContext is the findings JSON (or the AI summary) that
// grounds the model's answer.
func (a *Assistant) ChatFollowup(ctx context.Context, reportContext, question string) (*DiagnoseResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 报告解读服务未启用或未配置端点")
	}

	prompt := fmt.Sprintf(`你是一名安全运维专家。以下是此前安全扫描的结果摘要，用户将针对该结果提出追问。
请基于以下上下文进行回答，给出专业、可执行的中文建议。如果上下文中信息不足以回答，请明确指出。

扫描结果上下文：
%s

用户追问：%s

请用中文回答：`, reportContext, question)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return &DiagnoseResponse{Answer: answer, Model: cfg.Model}, nil
}

// InterpretAlert generates a one-paragraph Chinese root-cause and remediation
// suggestion for a fired alert. metricsContext is a short text summary of the
// host's current metrics and recent trend (may be empty).
func (a *Assistant) InterpretAlert(ctx context.Context, alertMessage, hostID, metricsContext string) (string, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return "", nil // silent skip when AI not configured
	}

	hostCtx := ""
	if host := a.reg.GetAgent(hostID); host != nil {
		hostCtx = fmt.Sprintf("主机: %s (%s %s)\n", host.Hostname, host.OS, host.Distro)
	}

	prompt := fmt.Sprintf(`你是一名运维监控专家。请对以下告警进行一句话解读：给出可能原因与处置建议，简洁专业，使用中文，不超过 3 句话。

%s告警信息：%s
%s
请用中文给出解读：`, hostCtx, alertMessage, metricsContext)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(answer), nil
}

// ---- Ops report & natural language Q&A ----

// OpsReportRequest parameters for report generation.
type OpsReportRequest struct {
	HostIDs []string `json:"host_ids"` // empty = all hosts
	Period  string   `json:"period"`   // "24h", "7d", "1h"
}

// GenerateOpsReport aggregates host metrics, alerts, and session data, then
// asks the LLM to write a Chinese health report. The report is persisted.
func (a *Assistant) GenerateOpsReport(ctx context.Context, req OpsReportRequest) (*OpsReport, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 服务未启用或未配置端点")
	}

	agents := a.reg.ListAgents()
	if len(req.HostIDs) > 0 {
		// Filter to requested hosts.
		want := make(map[string]bool, len(req.HostIDs))
		for _, id := range req.HostIDs {
			want[id] = true
		}
		filtered := make([]*rpc.Agent, 0, len(req.HostIDs))
		for _, ag := range agents {
			if want[ag.ID] {
				filtered = append(filtered, ag)
			}
		}
		agents = filtered
	}

	period := req.Period
	if period == "" {
		period = "24h"
	}
	periodSecs := parsePeriodSeconds(period)

	// Aggregate data for each host.
	var hostSections []string
	hostReports := make([]HostReport, 0, len(agents))
	now := time.Now().Unix()
	totalScore := 0.0
	alertCount := 0

	for _, ag := range agents {
		section := fmt.Sprintf("## 主机: %s (%s %s)\n- 状态: %s\n- 架构: %s\n- 运行时长: %d秒\n",
			ag.Hostname, ag.OS, ag.Distro, ag.Status, ag.Arch, ag.Uptime)

		score := 100.0
		if ag.Status != "online" {
			score -= 40
		}

		// Metrics summary.
		hub := a.reg.Hub(ag.ID)
		if hub != nil {
			m := hub.LastMetrics()
			if m != nil {
				cpu := m.GetCpuUsage()
				memPct := 0.0
				if m.GetMemTotal() > 0 {
					memPct = float64(m.GetMemUsed()) / float64(m.GetMemTotal()) * 100
				}
				section += fmt.Sprintf("- CPU: %.1f%%\n- 内存: %.1f%%\n", cpu, memPct)
				if cpu > 90 {
					score -= 15
				}
				if memPct > 90 {
					score -= 15
				}
				for _, mt := range m.GetMounts() {
					pct := 0.0
					if mt.GetTotal() > 0 {
						pct = float64(mt.GetUsed()) / float64(mt.GetTotal()) * 100
					}
					section += fmt.Sprintf("- 磁盘 %s: %.1f%%\n", mt.GetPath(), pct)
					if pct > 90 {
						score -= 10
					}
				}
			}
		}

		// Recent metrics history summary (if store available).
		if a.metricsStore != nil {
			pts := a.metricsStore.QueryHistory(ag.ID, now-periodSecs, now, 60)
			if len(pts) > 0 {
				var cpuSum, memSum float64
				var cpuMax, memMax float64
				for _, p := range pts {
					cpuSum += p.CPUUsage
					memSum += p.MemUsage
					if p.CPUUsage > cpuMax {
						cpuMax = p.CPUUsage
					}
					if p.MemUsage > memMax {
						memMax = p.MemUsage
					}
				}
				section += fmt.Sprintf("- 过去%s平均CPU: %.1f%%, 峰值: %.1f%%\n- 过去%s平均内存: %.1f%%, 峰值: %.1f%%\n",
					period, cpuSum/float64(len(pts)), cpuMax, period, memSum/float64(len(pts)), memMax)
			}
		}

		// Alerts for this host.
		hostAlerts := 0
		if a.alertStore != nil {
			events := a.alertStore.ListEvents(200)
			for _, e := range events {
				if e.HostID == ag.ID && !e.Resolved {
					hostAlerts++
				}
			}
		}
		if hostAlerts > 0 {
			section += fmt.Sprintf("- 未处理告警: %d 条\n", hostAlerts)
			score -= float64(hostAlerts) * 5
			alertCount += hostAlerts
		}

		if score < 0 {
			score = 0
		}
		totalScore += score

		hostReports = append(hostReports, HostReport{
			HostID:   ag.ID,
			Hostname: ag.Hostname,
			Score:    score,
			Alerts:   hostAlerts,
		})
		hostSections = append(hostSections, section)
	}

	overallScore := 100.0
	if len(agents) > 0 {
		overallScore = totalScore / float64(len(agents))
	}

	// Ask LLM for a narrative report.
	prompt := fmt.Sprintf(`你是一名资深运维专家。请根据以下主机集群数据生成一份中文运维健康报告。

时间范围: 过去 %s
主机数量: %d
整体健康度评分: %.1f/100
未处理告警总数: %d

各主机数据:
%s

请严格按如下 JSON 格式输出（不要输出 JSON 以外的内容，不要使用 markdown 代码块）：
{
  "summary": "整体健康度概述，说明集群状态与主要风险（2-4 句）",
  "suggestions": ["改进建议1", "改进建议2", "改进建议3"]
}`,
		period, len(agents), overallScore, alertCount, strings.Join(hostSections, "\n"))

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}

	summary := answer
	var suggestions []string
	clean := strings.TrimSpace(answer)
	if strings.HasPrefix(clean, "```json") {
		clean = strings.TrimPrefix(clean, "```json")
		clean = strings.TrimSuffix(clean, "```")
	} else if strings.HasPrefix(clean, "```") {
		clean = strings.TrimPrefix(clean, "```")
		clean = strings.TrimSuffix(clean, "```")
	}
	clean = strings.TrimSpace(clean)
	var llmResp struct {
		Summary     string   `json:"summary"`
		Suggestions []string `json:"suggestions"`
	}
	if json.Unmarshal([]byte(clean), &llmResp) == nil && llmResp.Summary != "" {
		summary = llmResp.Summary
		suggestions = llmResp.Suggestions
	}
	if suggestions == nil {
		suggestions = []string{}
	}

	report := &OpsReport{
		ID:          fmt.Sprintf("%d", time.Now().UnixNano()),
		GeneratedAt: time.Now(),
		Period:      period,
		HostIDs:     req.HostIDs,
		Summary:     summary,
		HealthScore: overallScore,
		HostReports: hostReports,
		Suggestions: suggestions,
		Model:       cfg.Model,
	}
	_ = a.reports.Save(report)
	return report, nil
}

// ListOpsReports returns all persisted reports (newest first).
func (a *Assistant) ListOpsReports() []*OpsReport {
	if a.reports == nil {
		return nil
	}
	return a.reports.List()
}

// GetOpsReport returns a single report by ID.
func (a *Assistant) GetOpsReport(id string) (*OpsReport, bool) {
	if a.reports == nil {
		return nil, false
	}
	return a.reports.Get(id)
}

// ChatMessage is a single message in a conversation.
type ChatMessage struct {
	Role    string `json:"role"`    // user / assistant
	Content string `json:"content"`
}

// ChatResponse is the result of a natural language Q&A.
type ChatResponse struct {
	Answer  string `json:"answer"`
	Model   string `json:"model"`
	ToolsUsed []string `json:"tools_used,omitempty"`
}

// Chat answers a natural language question using tool calling. The assistant
// first checks if the question can be answered with platform data (hosts,
// metrics, alerts), fetches that data, then asks the LLM to synthesize an
// answer. For models without tool calling, we pre-fetch relevant context and
// embed it in the prompt.
func (a *Assistant) Chat(ctx context.Context, question string, history []ChatMessage) (*ChatResponse, error) {
	cfg := a.Config()
	if !cfg.Enabled || cfg.BaseURL == "" {
		return nil, fmt.Errorf("AI 问答服务未启用或未配置端点")
	}

	// Gather platform context based on the question (poor-man's tool calling:
	// pre-fetch data that's likely relevant, then let the LLM answer).
	platformContext := a.gatherPlatformContext(question)

	// Build conversation history.
	historyStr := ""
	for _, m := range history {
		if m.Role == "user" {
			historyStr += fmt.Sprintf("用户: %s\n", m.Content)
		} else if m.Role == "assistant" {
			historyStr += fmt.Sprintf("助手: %s\n", m.Content)
		}
	}

	prompt := fmt.Sprintf(`你是一个主机运维平台的智能助手。用户可以用自然语言询问平台管理的各类信息。
你可以参考以下平台实时数据来回答用户问题。如果数据不足以回答，请明确说明。

平台数据概览：
%s

%s用户问题: %s

请用中文回答，给出准确、有用的信息。如果用户问的是某台主机的具体情况，请基于上述数据回答：`,
		platformContext, historyStr, question)

	answer, err := a.callLLM(ctx, prompt)
	if err != nil {
		return nil, err
	}

	return &ChatResponse{
		Answer:    answer,
		Model:     cfg.Model,
		ToolsUsed: []string{"platform_context"},
	}, nil
}

// gatherPlatformContext fetches host, metrics, and alert data relevant to the
// question, returning a text summary the LLM can reason over.
func (a *Assistant) gatherPlatformContext(question string) string {
	q := strings.ToLower(question)
	var sb strings.Builder

	agents := a.reg.ListAgents()
	sb.WriteString(fmt.Sprintf("=== 主机列表 (%d 台) ===\n", len(agents)))
	for _, ag := range agents {
		line := fmt.Sprintf("- %s: %s/%s, 状态=%s", ag.Hostname, ag.OS, ag.Arch, ag.Status)
		hub := a.reg.Hub(ag.ID)
		if hub != nil {
			m := hub.LastMetrics()
			if m != nil {
				memPct := 0.0
				if m.GetMemTotal() > 0 {
					memPct = float64(m.GetMemUsed()) / float64(m.GetMemTotal()) * 100
				}
				line += fmt.Sprintf(", CPU=%.1f%%, 内存=%.1f%%", m.GetCpuUsage(), memPct)
				for _, mt := range m.GetMounts() {
					pct := 0.0
					if mt.GetTotal() > 0 {
						pct = float64(mt.GetUsed()) / float64(mt.GetTotal()) * 100
					}
					if pct > 80 {
						line += fmt.Sprintf(", 磁盘%s=%.1f%%", mt.GetPath(), pct)
					}
				}
			}
		}
		sb.WriteString(line + "\n")
	}

	// Alerts.
	if a.alertStore != nil {
		events := a.alertStore.ListEvents(20)
		if len(events) > 0 {
			sb.WriteString(fmt.Sprintf("\n=== 最近告警 (%d 条) ===\n", len(events)))
			for _, e := range events {
				sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", e.Severity, e.Hostname, e.Message))
			}
		}
	}

	// If question mentions disk/memory/cpu, include more metrics detail.
	if strings.Contains(q, "磁盘") || strings.Contains(q, "disk") || strings.Contains(q, "空间") ||
		strings.Contains(q, "内存") || strings.Contains(q, "memory") || strings.Contains(q, "cpu") {
		sb.WriteString("\n=== 各主机资源详情 ===\n")
		for _, ag := range agents {
			hub := a.reg.Hub(ag.ID)
			if hub == nil {
				continue
			}
			m := hub.LastMetrics()
			if m == nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("- %s: CPU=%.1f%%", ag.Hostname, m.GetCpuUsage()))
			if m.GetMemTotal() > 0 {
				pct := float64(m.GetMemUsed()) / float64(m.GetMemTotal()) * 100
				sb.WriteString(fmt.Sprintf(", 内存=%.1f%% (%d/%d bytes)", pct, m.GetMemUsed(), m.GetMemTotal()))
			}
			for _, mt := range m.GetMounts() {
				pct := 0.0
				if mt.GetTotal() > 0 {
					pct = float64(mt.GetUsed()) / float64(mt.GetTotal()) * 100
				}
				sb.WriteString(fmt.Sprintf(", %s=%.1f%%", mt.GetPath(), pct))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

func parsePeriodSeconds(period string) int64 {
	switch period {
	case "1h":
		return 3600
	case "24h":
		return 86400
	case "7d":
		return 7 * 86400
	case "30d":
		return 30 * 86400
	default:
		return 86400
	}
}

func buildSystemContext(hostname string, host *rpc.Agent, m *agentpb.MetricsSample) string {
	ctx := fmt.Sprintf("主机名: %s\n", hostname)
	if host != nil {
		ctx += fmt.Sprintf("操作系统: %s/%s\n状态: %s\nAgent版本: %s\n", host.OS, host.Arch, host.Status, host.Version)
	}
	if m != nil {
		ctx += fmt.Sprintf("CPU使用率: %.1f%%\n", m.GetCpuUsage())
		memPct := 0.0
		if m.GetMemTotal() > 0 {
			memPct = float64(m.GetMemUsed()) / float64(m.GetMemTotal()) * 100
		}
		ctx += fmt.Sprintf("内存: %.1f%% (%d/%d bytes)\n", memPct, m.GetMemUsed(), m.GetMemTotal())
		ctx += fmt.Sprintf("网络收发速率: 读 %.1f 字节/s, 写 %.1f 字节/s\n", m.GetNetRx(), m.GetNetTx())
		ctx += fmt.Sprintf("磁盘IO: 读 %.1f 字节/s, 写 %.1f 字节/s\n", m.GetDiskRead(), m.GetDiskWrite())
		for _, mt := range m.GetMounts() {
			pct := 0.0
			if mt.GetTotal() > 0 {
				pct = float64(mt.GetUsed()) / float64(mt.GetTotal()) * 100
			}
			ctx += fmt.Sprintf("挂载点 %s: %.1f%% (总 %d bytes)\n", mt.GetPath(), pct, mt.GetTotal())
		}
	}
	return ctx
}

func (a *Assistant) callLLM(ctx context.Context, prompt string) (string, error) {
	return callLLMWithConfig(ctx, a.Config(), prompt)
}

func callLLMWithConfig(ctx context.Context, cfg Config, prompt string) (string, error) {
	rawURL := strings.TrimRight(cfg.BaseURL, "/")
	if rawURL == "" {
		return "", fmt.Errorf("LLM Base URL is empty")
	}

	// Ollama detection
	if strings.Contains(rawURL, ":11434") || strings.HasSuffix(rawURL, "/api/generate") {
		return callOllama(ctx, cfg, rawURL, prompt)
	}

	return callOpenAI(ctx, cfg, rawURL, prompt)
}

// applyCustomHeaders sets user-defined HTTP headers on the outgoing LLM
// request. It runs after the built-in Authorization header so a custom
// Authorization value can override the default "Bearer <api_key>" scheme
// when a gateway or proxy requires a different auth format.
func applyCustomHeaders(req *http.Request, cfg Config) {
	for k, v := range cfg.Headers {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		req.Header.Set(k, v)
	}
}

func callOllama(ctx context.Context, cfg Config, baseURL string, prompt string) (string, error) {
	targetURL := baseURL
	if !strings.HasSuffix(targetURL, "/api/generate") {
		targetURL = targetURL + "/api/generate"
	}

	body, _ := json.Marshal(map[string]any{
		"model":  cfg.Model,
		"prompt": prompt,
		"stream": false,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	applyCustomHeaders(req, cfg)

	return doRequest(req, func(data []byte) (string, error) {
		var resp struct {
			Response string `json:"response"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return "", fmt.Errorf("解析 Ollama 响应失败: %w", err)
		}
		if resp.Error != "" {
			return "", fmt.Errorf("Ollama 返回错误: %s", resp.Error)
		}
		return resp.Response, nil
	})
}

func callOpenAI(ctx context.Context, cfg Config, baseURL string, prompt string) (string, error) {
	targetURL := baseURL
	if strings.HasSuffix(targetURL, "/chat/completions") {
		// already has endpoint
	} else if strings.HasSuffix(targetURL, "/v1") {
		targetURL = targetURL + "/chat/completions"
	} else {
		targetURL = targetURL + "/v1/chat/completions"
	}

	body, _ := json.Marshal(map[string]any{
		"model": cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": "你是一个专业的服务器运维和自动化专家助手。"},
			{"role": "user", "content": prompt},
		},
		"stream": false,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	applyCustomHeaders(req, cfg)

	return doRequest(req, func(data []byte) (string, error) {
		var resp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return "", fmt.Errorf("解析 OpenAI 兼容响应失败: %w (原始内容: %s)", err, string(data))
		}
		if resp.Error != nil && resp.Error.Message != "" {
			return "", fmt.Errorf("LLM API 返回错误: %s", resp.Error.Message)
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("LLM 返回空内容")
		}
		return resp.Choices[0].Message.Content, nil
	})
}

func doRequest(req *http.Request, parse func([]byte) (string, error)) (string, error) {
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM 接口返回 HTTP %d: %s", resp.StatusCode, string(data))
	}
	return parse(data)
}
