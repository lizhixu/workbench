import { http, unwrap } from './http'

export interface AIConfig {
  base_url: string
  model: string
  api_key: string
  enabled: boolean
  provider?: string
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
