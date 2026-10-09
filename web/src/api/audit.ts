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

// Human-readable labels for audit actions. Keys are the action codes stored
// in the audit trail; unknown codes fall back to the raw value, so every new
// action code must get an entry here (AGENTS.md §7 操作审计全覆盖).
export const actionLabels: Record<string, string> = {
  login: '登录控制台',
  logout: '退出登录',
  // 终端 / 命令 / 隧道
  terminal_open: '打开终端',
  terminal_share: '分享终端',
  exec: '执行命令',
  batch_exec: '批量推送命令',
  tunnel_open: '打开内网隧道',
  tunnel_close: '关闭内网隧道',
  // 文件
  file_mkdir: '新建目录',
  file_move: '移动/重命名文件',
  file_copy: '复制文件',
  file_remove: '删除文件',
  file_download: '下载文件',
  file_upload: '上传文件',
  // 主机
  host_unbind: '解绑主机',
  host_group: '调整分组',
  host_tags: '更新标签',
  host_billing: '更新财务与流量配置',
  process_kill: '结束进程',
  agent_upgrade: '升级 Agent',
  scan_trigger: '触发安全扫描',
  docker_op: 'Docker 操作',
  docker_mirrors: '配置镜像加速',
  docker_install: '一键安装 Docker',
  app_install: '安装应用',
  app_uninstall: '卸载应用',
  // 会话
  session_delete: '删除会话/录像',
  // 用户与设置
  user_create: '创建用户',
  user_delete: '删除用户',
  user_password: '修改用户密码',
  user_role: '调整用户角色',
  user_reset_password: '重置用户密码',
  user_settings_update: '更新个人设置',
  system_settings_update: '更新系统设置',
  policy_update: '更新高危命令策略',
  ai_config_update: '更新 AI 模型配置',
  vault_op: '凭证库操作',
  // 应用中心
  app_create: '创建应用',
  app_update: '更新应用配置',
  app_delete: '删除应用',
  app_deploy: '部署应用',
  app_rollback: '回滚应用到上一版本',
  app_stop: '停止应用',
  app_start: '启动应用',
  app_restart: '重启应用',
  app_webhook_sync: '同步 Webhook 自动部署',
  app_proxy_bind: '绑定反代域名',
  app_proxy_unbind: '解绑反代域名',
  // 证书中心
  cert_issue: '签发证书',
  cert_import: '导入已有证书',
  cert_renew: '续期证书',
  cert_delete: '删除证书',
  cert_config_update: '更新证书 DNS 验证配置',
  cert_account_create: '添加 ACME 账户',
  cert_account_update: '更新 ACME 账户',
  cert_account_delete: '删除 ACME 账户',
  cert_manual_dns_confirm: '确认手动 DNS-01 解析',
  cert_manual_dns_cancel: '取消手动 DNS-01 订单',
  panel_cert_issue: '签发面板证书',
  panel_cert_issue_confirm: '确认面板证书 DNS 解析',
  panel_cert_issue_cancel: '取消面板证书签发',
  // 备份
  system_backup: '创建控制端备份',
  system_backup_delete: '删除控制端备份',
  system_restore: '从备份恢复控制端',
  backup_job_create: '创建备份任务',
  backup_job_update: '更新备份任务',
  backup_job_delete: '删除备份任务',
  backup_job_run: '立即执行备份任务',
  backup_archive_restore: '从归档恢复数据',
  backup_archive_delete: '删除备份归档',
  backup_s3_create: '添加 S3 备份目标',
  backup_s3_update: '更新 S3 备份目标',
  backup_s3_delete: '删除 S3 备份目标',
  backup_s3_test: '测试 S3 备份目标',
  // Git
  git_token_set: '设置 Git 访问令牌',
  git_account_delete: '解绑 Git 账户',
  // 分组与授权
  group_create: '创建分组',
  group_update: '更新分组',
  group_delete: '删除分组',
  group_grant: '授权用户访问分组',
  group_revoke: '移除用户分组授权',
  // 常用命令
  command_create: '新建常用命令',
  command_update: '更新常用命令',
  command_delete: '删除常用命令',
  // 告警
  alert_rule_create: '创建告警规则',
  alert_rule_update: '更新告警规则',
  alert_rule_delete: '删除告警规则',
  alert_events_ack_all: '全部确认告警',
  alert_event_ack: '确认告警',
  alert_event_delete: '删除告警',
  alert_events_clear: '清空告警记录',
  alert_webhook_update: '更新告警 Webhook',
  alert_webhook_test: '测试告警 Webhook',
  // 控制端自身
  restart: '重启控制端',
  upgrade: '在线升级控制端',
  batch_upgrade: '批量升级 Agent',
}

// A few short action codes are reused across target types and only mean
// something together with the target: resolve "action:target_type" first.
export const actionTypeLabels: Record<string, string> = {
  'update:system': '上传控制端二进制',
  'update:network_config': '更新组网配置',
  'install:network_node': '安装组网客户端',
  'join:network_node': '加入异地组网',
  'leave:network_node': '退出异地组网',
}

export function actionLabel(action: string, targetType?: string): string {
  if (targetType) {
    const typed = actionTypeLabels[`${action}:${targetType}`]
    if (typed) return typed
  }
  return actionLabels[action] || action
}

// Target types shown in the audit detail modal.
export const targetTypeLabels: Record<string, string> = {
  host: '主机',
  user: '用户',
  session: '终端会话',
  system: '系统',
  app: '应用',
  cert: '证书',
  group: '分组',
  command: '常用命令',
  alert: '告警',
  backup: '备份',
  network_config: '组网配置',
  network_node: '组网节点',
}

export function targetTypeLabel(targetType?: string): string {
  if (!targetType) return '-'
  return targetTypeLabels[targetType] || targetType
}

export const riskLabels: Record<string, string> = {
  low: '低危',
  medium: '中危',
  high: '高危',
}

export const resultLabels: Record<string, string> = {
  success: '成功',
  failed: '失败',
  blocked: '已拦截',
}
