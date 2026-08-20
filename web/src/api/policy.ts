import { http, unwrap } from './http'

export type RiskLevel = 'low' | 'medium' | 'high' | 'blocked'

export interface CommandPolicy {
  enabled: boolean
  blacklist: string[]
  whitelist: string[]
  high_risk_patterns: string[]
}

export interface CommandAuditEntry {
  id: string
  timestamp: string
  username: string
  host_id: string
  host_ids?: string[]
  command: string
  shell: string
  risk_level: RiskLevel
  result: string
  reason: string
}

export function getCommandPolicy() {
  return unwrap<{ data: CommandPolicy }>(http.get('/policy/command')).then((r) => r.data)
}

export function setCommandPolicy(policy: CommandPolicy) {
  return unwrap<{ ok: boolean; data: CommandPolicy }>(http.put('/policy/command', policy)).then((r) => r.data)
}

export function listCommandAudit(params?: { limit?: number; username?: string; host_id?: string }) {
  return unwrap<{ data: CommandAuditEntry[] }>(http.get('/policy/command/audit', { params })).then((r) => r.data)
}