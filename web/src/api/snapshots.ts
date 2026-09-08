import { http, unwrap } from './http'

export type JobKind = 'dir' | 'volume' | 'database'

export interface BackupJob {
  id: string
  name: string
  host_id: string
  kind: JobKind
  target: string
  db_type?: string
  db_name?: string
  retention: number
  schedule: {
    cron?: string
    manual?: boolean
  }
  enabled: boolean
  last_run?: string
  last_error?: string
  created_at: string
}

export interface Archive {
  id: string
  job_id: string
  host_id: string
  kind: JobKind
  target: string
  size: number
  sha256?: string
  created_at: string
}

export interface CreateJobReq {
  name: string
  host_id: string
  kind: JobKind
  target: string
  db_type?: string
  db_name?: string
  retention?: number
  cron?: string
  enabled?: boolean
}

export function listBackupJobs() {
  return unwrap<{ data: BackupJob[] }>(http.get('/backups/jobs')).then((r) => r.data)
}

export function createBackupJob(req: CreateJobReq) {
  return unwrap<{ data: BackupJob }>(http.post('/backups/jobs', req)).then((r) => r.data)
}

export function updateBackupJob(id: string, req: Partial<CreateJobReq>) {
  return unwrap<{ data: BackupJob }>(http.put(`/backups/jobs/${id}`, req)).then((r) => r.data)
}

export function deleteBackupJob(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/backups/jobs/${id}`))
}

export function runBackupJob(id: string) {
  return unwrap<{ ok: boolean; message: string }>(http.post(`/backups/jobs/${id}/run`))
}

export function listArchives(jobId: string, offset = 0, pageSize = 20) {
  return unwrap<{ data: Archive[]; total: number; offset: number; page_size: number }>(
    http.get(`/backups/jobs/${jobId}/archives`, { params: { offset, page_size: pageSize } }),
  )
}

export function restoreArchive(jobId: string, archiveId: string) {
  return unwrap<{ ok: boolean; message: string }>(
    http.post(`/backups/jobs/${jobId}/archives/${archiveId}/restore`),
  )
}

export function deleteArchive(jobId: string, archiveId: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/backups/jobs/${jobId}/archives/${archiveId}`))
}