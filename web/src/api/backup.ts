// Control-plane backup / restore (AGENTS.md 5.1 数据自主可控).
import { http, unwrap } from './http'

export interface BackupMeta {
  name: string
  size: number
  files: number
  sha256: string
  note: string
  created_by: string
  created_at: string
  /** Archives the server took on its own, currently the pre-restore copy. */
  auto: boolean
}

export interface RestoreResult {
  files: number
  bytes: number
  safety_copy: string
  restart_required: boolean
}

export interface RestoreResponse {
  data: RestoreResult
  message: string
}

export function listBackups() {
  return unwrap<{ data: BackupMeta[] }>(http.get('/system/backups')).then((r) => r.data || [])
}

// Archiving the whole data directory can take a while on a host with a lot of
// recordings, so this call opts out of the short global timeout.
export function createBackup(note = '') {
  return unwrap<{ data: BackupMeta }>(
    http.post('/system/backup', { note }, { timeout: 300000 }),
  ).then((r) => r.data)
}

export function deleteBackup(name: string) {
  return unwrap<{ ok: boolean }>(
    http.delete(`/system/backups/${encodeURIComponent(name)}`),
  )
}

export function restoreBackup(name: string) {
  return unwrap<RestoreResponse>(
    http.post(`/system/backups/${encodeURIComponent(name)}/restore`, {}, { timeout: 300000 }),
  )
}

export function uploadRestore(file: File, onProgress?: (percent: number) => void) {
  const form = new FormData()
  form.append('file', file)
  return unwrap<RestoreResponse>(
    http.post('/system/restore', form, {
      timeout: 600000,
      onUploadProgress: (e) => {
        if (!onProgress || !e.total) return
        onProgress(Math.round((e.loaded / e.total) * 100))
      },
    }),
  )
}

// The download endpoint streams a file, so it is reached by navigation rather
// than XHR; the JWT rides along as a query parameter the same way the file
// download and WebSocket endpoints take it.
export function backupDownloadUrl(name: string): string {
  const token = localStorage.getItem('watchman_token') || ''
  const tokParam = token ? `?token=${encodeURIComponent(token)}` : ''
  return `/api/v1/system/backups/${encodeURIComponent(name)}/download${tokParam}`
}
