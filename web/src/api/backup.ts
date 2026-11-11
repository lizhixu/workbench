import { http, unwrap } from './http'

export interface BackupMeta {
  name: string
  size: number
  files: number
  sha256: string
  note: string
  created_by: string
  created_at: string
  auto: boolean
  kind?: 'manual' | 'scheduled' | 'safety'
  remote_target?: string
  remote_name?: string
  remote_key?: string
  remote_error?: string
}
export interface RestoreResult {
  files: number
  bytes: number
  safety_copy: string
  restart_required: boolean
  pending: boolean
  legacy: boolean
}
export interface BackupTask {
  id: string
  kind: 'backup' | 'restore'
  status: '' | 'running' | 'ready' | 'success' | 'failed' | 'cancelled'
  phase: string
  files: number
  bytes: number
  error?: string
  archive?: BackupMeta
  result?: RestoreResult
  started_at: string
  finished_at?: string
}
export interface BackupPolicy {
  enabled: boolean
  time: string
  retention: number
  target_id: string
  last_run?: string
  last_error?: string
}
export interface BackupStatus {
  data: BackupTask
  pending: boolean
  max_upload_bytes: number
  max_data_bytes: number
}
export function listBackups() {
  return unwrap<{ data: BackupMeta[] }>(http.get('/system/backups')).then(r => r.data || [])
}
export function backupStatus() {
  return unwrap<BackupStatus>(http.get('/system/backups/status'))
}
export function getBackupPolicy() {
  return unwrap<{ data: BackupPolicy }>(http.get('/system/backups/policy')).then(r => r.data)
}
export function saveBackupPolicy(policy: BackupPolicy) {
  return unwrap<{ data: BackupPolicy }>(http.put('/system/backups/policy', policy)).then(r => r.data)
}
export function createBackup(note = '', targetID = 'local') {
  return unwrap<{ data: BackupTask }>(http.post('/system/backup', { note, target_id: targetID, async: true })).then(r => r.data)
}
export function deleteBackup(name: string) {
  return unwrap<{ ok: boolean; warning?: string }>(http.delete(`/system/backups/${encodeURIComponent(name)}`))
}
export function restoreBackup(name: string, vaultPass = '') {
  return unwrap<{ data: BackupTask }>(http.post(`/system/backups/${encodeURIComponent(name)}/restore`, { vault_pass: vaultPass })).then(r => r.data)
}
export function uploadRestore(file: File, vaultPass: string, onProgress: (percent: number) => void) {
  const form = new FormData()
  // The streaming server reads bounded metadata before the file part.
  form.append('vault_pass', vaultPass)
  form.append('file', file)
  return unwrap<{ data: BackupTask }>(http.post('/system/restore', form, {
    timeout: 0,
    onUploadProgress: e => { if (e.total) onProgress(Math.round(e.loaded / e.total * 100)) },
  })).then(r => r.data)
}
export function cancelPendingRestore() {
  return unwrap<{ ok: boolean }>(http.delete('/system/restore/pending'))
}
export function backupDownloadUrl(name: string): string {
  const token = localStorage.getItem('watchman_token') || ''
  return `/api/v1/system/backups/${encodeURIComponent(name)}/download?token=${encodeURIComponent(token)}`
}