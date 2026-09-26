// Unified operational audit trail API.
import { http, unwrap } from './http'

export interface AuditEntry {
  id: string
  timestamp: string
  username: string
  action: string
  target_type: string
  target_id?: string
  detail?: string
  ip?: string
  user_agent?: string
  risk_level: 'low' | 'medium' | 'high'
  result: 'success' | 'failed' | 'blocked'
}

export interface AuditStats {
  total: number
  today: number
  high_risk: number
  failed: number
}

export interface AuditQuery {
  username?: string
  action?: string
  target?: string
  result?: string
  risk?: string
  from?: string
  to?: string
  offset?: number
  page_size?: number
}

export async function listAudit(q: AuditQuery = {}) {
  return unwrap<{ data: AuditEntry[]; stats: AuditStats; page: { offset: number; limit: number } }>(
    http.get('/audit', { params: q }),
  )
}

// Build a download URL for CSV export (browser navigates with token query).
export function auditExportUrl(q: AuditQuery = {}) {
  const base = import.meta.env.VITE_API_BASE || '/api/v1'
  const params = new URLSearchParams()
  if (q.username) params.set('username', q.username)
  if (q.action) params.set('action', q.action)
  if (q.target) params.set('target', q.target)
  if (q.result) params.set('result', q.result)
  if (q.risk) params.set('risk', q.risk)
  if (q.from) params.set('from', q.from)
  if (q.to) params.set('to', q.to)
  const token = localStorage.getItem('watchman_token')
  if (token) params.set('token', token)
  return `${base}/audit/export?${params.toString()}`
}

// Human-readable labels for audit actions.
export const actionLabels: Record<string, string> = {
  login: '登录控制台',
  terminal_open: '打开终端',
  terminal_share: '分享终端',
  exec: '执行命令',
  batch_exec: '批量推送命令',
  file_mkdir: '新建目录',
  file_move: '移动文件',
  file_copy: '复制文件',
  file_remove: '删除文件',
  file_download: '下载文件',
  file_upload: '上传文件',
  host_unbind: '解绑主机',
  host_group: '调整分组',
  host_tags: '更新标签',
  docker_op: 'Docker 操作',
  agent_upgrade: '升级 Agent',
  user_create: '创建用户',
  user_delete: '删除用户',
  user_password: '修改密码',
  user_role: '调整角色',
  user_reset_password: '重置密码',
  user_mgmt: '用户管理',
  policy_update: '更新高危策略',
  vault_op: '凭证库操作',
}

export function actionLabel(action: string): string {
  return actionLabels[action] || action
}
