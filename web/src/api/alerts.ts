import { http, unwrap } from './http'

export type RuleType = 'offline' | 'online' | 'cpu_high' | 'mem_high' | 'disk_high' | 'anomaly'
export type Severity = 'info' | 'warning' | 'critical'

export interface AlertRule {
  id: string
  name: string
  type: RuleType
  severity: Severity
  threshold: number
  duration: number
  metric?: string
  host_filter: string
  group_filter: string
  enabled: boolean
  created_at: string
}

export interface AlertEvent {
  id: string
  rule_id: string
  rule_name: string
  severity: Severity
  host_id: string
  hostname: string
  message: string
  fired_at: string
  resolved: boolean
  resolved_at: string
  ai_interpretation?: string
}

export interface WebhookConfig {
  url: string
  secret: string
  enabled: boolean
}

export function listRules() {
  return unwrap<{ data: AlertRule[] }>(http.get('/alerts/rules')).then((r) => r.data)
}

export function createRule(rule: Omit<AlertRule, 'id' | 'created_at'>) {
  return unwrap<{ data: AlertRule }>(http.post('/alerts/rules', rule)).then((r) => r.data)
}

export function updateRule(id: string, rule: Partial<AlertRule>) {
  return unwrap<{ data: AlertRule }>(http.put(`/alerts/rules/${id}`, rule)).then((r) => r.data)
}

export function deleteRule(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/alerts/rules/${id}`))
}

export function listEvents() {
  return unwrap<{ data: AlertEvent[] }>(http.get('/alerts/events')).then((r) => r.data)
}

export function ackEvent(id: string) {
  return unwrap<{ ok: boolean }>(http.post(`/alerts/events/${id}/ack`))
}

export function ackAllEvents() {
  return unwrap<{ ok: boolean }>(http.post('/alerts/events/ack-all'))
}

export function deleteEvent(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/alerts/events/${id}`))
}

export function clearEvents(resolvedOnly = false) {
  return unwrap<{ ok: boolean }>(http.delete('/alerts/events', { params: { resolved_only: resolvedOnly } }))
}

export function getWebhook() {
  return unwrap<{ data: WebhookConfig }>(http.get('/alerts/webhook')).then((r) => r.data)
}

export function setWebhook(config: WebhookConfig) {
  return unwrap<{ ok: boolean }>(http.put('/alerts/webhook', config))
}

export interface WebhookTestResult {
  ok: boolean
  platform: 'dingtalk' | 'wecom' | 'feishu' | 'generic'
  status_code: number
  duration_ms: number
  message?: string
  error?: string
  response?: string
}

export function testWebhook(config?: Partial<WebhookConfig>) {
  return unwrap<WebhookTestResult>(http.post('/alerts/webhook/test', config || {}))
}