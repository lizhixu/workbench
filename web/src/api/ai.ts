import { http, unwrap } from './http'

export interface AIConfig {
  base_url: string
  model: string
  api_key: string
  enabled: boolean
  provider?: string
  headers?: Record<string, string>
}

export interface DiagnoseResponse {
  answer: string
  model: string
  tokens_used: number
}

export interface Nl2CommandResponse {
  command: string
  explanation: string
  risk_level: 'low' | 'medium' | 'high'
  needs_confirm: boolean
}

export function getAIConfig() {
  return unwrap<{ data: AIConfig }>(http.get('/ai/config')).then((r) => r.data)
}

export function setAIConfig(config: AIConfig) {
  return unwrap<{ ok: boolean }>(http.put('/ai/config', config))
}

export function testAIConfig(config: Partial<AIConfig>) {
  return unwrap<{ ok: boolean; message: string }>(http.post('/ai/test', config))
}

export function diagnose(hostId: string, query: string, context?: string) {
  return unwrap<{ data: DiagnoseResponse }>(
    http.post('/ai/diagnose', { host_id: hostId, query, context }),
  ).then((r) => r.data)
}

export function nl2command(prompt: string, hostId?: string) {
  return unwrap<{ data: Nl2CommandResponse }>(
    http.post('/ai/nl2command', { prompt, host_id: hostId }),
  ).then((r) => r.data)
}

export function analyzeExec(command: string, stdout: string, stderr: string, exitCode: number) {
  return unwrap<{ data: DiagnoseResponse }>(
    http.post('/ai/analyze-exec', { command, stdout, stderr, exit_code: exitCode }),
  ).then((r) => r.data)
}

// ---- Multi-step task planning (Terminal AI Copilot) ----

export interface TaskPlanStep {
  index: number
  title: string
  description: string
  command: string
  risk_level: 'low' | 'medium' | 'high'
  needs_confirm: boolean
  probe?: boolean
  continue_on_error?: boolean
  matched_pattern?: string
}

export interface TaskPlan {
  title: string
  goal: string
  os: string
  distro: string
  arch: string
  shell: string
  summary: string
  risk_level: 'low' | 'medium' | 'high'
  steps: TaskPlanStep[]
  source: 'blueprint' | 'llm'
  model?: string
}

// planTask asks the server to break an operational intent (e.g. "安装 docker")
// into an ordered, host-tailored multi-step plan. The server prefers built-in
// offline blueprints and falls back to the configured LLM.
export function planTask(prompt: string, hostId?: string) {
  return unwrap<{ data: TaskPlan }>(
    http.post('/ai/plan', { prompt, host_id: hostId }, { timeout: 120000 }),
  ).then((r) => r.data)
}

// ---- Ops report & natural language Q&A (P3) ----

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface ChatResponse {
  answer: string
  model: string
  tools_used?: string[]
}

export interface HostReport {
  host_id: string
  hostname: string
  summary: string
  score: number
  alerts: number
}

export interface OpsReport {
  id: string
  generated_at: string
  period: string
  host_ids: string[]
  summary: string
  health_score: number
  host_reports: HostReport[]
  suggestions: string[]
  model: string
}

export function chat(question: string, history?: ChatMessage[]) {
  return unwrap<{ data: ChatResponse }>(
    http.post('/ai/chat', { question, history: history || [] }, { timeout: 120000 }),
  ).then((r) => r.data)
}

export function generateOpsReport(hostIds?: string[], period?: string) {
  return unwrap<{ data: OpsReport }>(
    http.post('/ai/ops-report', { host_ids: hostIds || [], period: period || '24h' }, { timeout: 120000 }),
  ).then((r) => r.data)
}

export function listOpsReports() {
  return unwrap<{ data: OpsReport[] }>(http.get('/ai/ops-reports')).then((r) => r.data)
}

export function getOpsReport(id: string) {
  return unwrap<{ data: OpsReport }>(http.get(`/ai/ops-reports/${id}`)).then((r) => r.data)
}

// ---- Create-app AI assistant (draft fill + form-aware chat) ----

// Snapshot of the create-app form sent as AI context. Secret values are
// redacted server-side before reaching the model.
export interface AppFormSnapshot {
  source_mode: 'template' | 'image' | 'compose' | 'github' | 'custom'
  name: string
  container_name: string
  host_id: string
  template_id: string
  template_params: Record<string, string>
  image: string
  compose_content: string
  repo_url: string
  branch: string
  auth_vault_id: string
  auto_deploy: boolean
  build_type: string
  dockerfile: string
  build_context: string
  build_timeout_sec: number
  ports: { host: number; container: number; bind_scope: string }[]
  env_vars: Record<string, string>
  volumes: string[]
  healthcheck_url: string
  domain: string
  proxy_mode: string
  gateway_host_id: string
}

// AI-proposed form fill; only present fields should be applied.
export interface AppFormDraft {
  source_type?: 'template' | 'image' | 'raw_compose' | 'git'
  name?: string
  container_name?: string
  host_id?: string
  template_id?: string
  template_params?: Record<string, string>
  image?: string
  compose_content?: string
  repo_url?: string
  branch?: string
  auto_deploy?: boolean
  build_type?: 'dockerfile' | 'compose'
  dockerfile?: string
  build_context?: string
  build_timeout_sec?: number
  ports?: { host: number; container: number; bind_scope: string }[]
  env_vars?: Record<string, string>
  volumes?: string[]
  healthcheck_url?: string
  domain?: string
  proxy_mode?: 'local' | 'gateway'
  gateway_host_id?: string
}

export interface AppDraftResponse {
  draft: AppFormDraft
  explanation: string
  missing: string[]
  warnings: string[]
  model: string
}

export interface AppChatResponse {
  answer: string
  form_patch?: AppFormDraft
  patch_warnings?: string[]
  model: string
}

export function generateAppDraft(requirement: string, form: AppFormSnapshot) {
  return unwrap<{ data: AppDraftResponse }>(
    http.post('/ai/app-draft', { requirement, form }, { timeout: 120000 }),
  ).then((r) => r.data)
}

export function chatAppAssistant(question: string, history: ChatMessage[], form: AppFormSnapshot) {
  return unwrap<{ data: AppChatResponse }>(
    http.post('/ai/app-chat', { question, history, form }, { timeout: 120000 }),
  ).then((r) => r.data)
}
