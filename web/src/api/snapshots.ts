import { http, unwrap } from './http'

export type JobKind = 'dir' | 'volume' | 'database'

export interface S3Target {
  id: string
  name: string
  provider: 'r2' | 'minio' | 'aws' | 'aliyun' | 'tencent' | 'custom'
  endpoint: string
  region: string
  bucket: string
  prefix?: string
  access_key: string
  secret_key?: string
  force_path_style: boolean
  is_default: boolean
  created_at: string
}

export interface BackupJob {
  id: string
  name: string
  host_id: string
  kind: JobKind
  target: string
  db_type?: string
  db_name?: string
  storage_target?: string // "default" | "local" | "s3_xxxx"
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
  storage_target?: string
  storage_path?: string
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
  storage_target?: string
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

// S3 Storage Targets API
export function listS3Targets() {
  return unwrap<{ data: S3Target[] }>(http.get('/backups/s3-targets')).then((r) => r.data)
}

export function createS3Target(req: Partial<S3Target>) {
  return unwrap<{ data: S3Target }>(http.post('/backups/s3-targets', req)).then((r) => r.data)
}

export function updateS3Target(id: string, req: Partial<S3Target>) {
  return unwrap<{ data: S3Target }>(http.put(`/backups/s3-targets/${id}`, req)).then((r) => r.data)
}

export function deleteS3Target(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/backups/s3-targets/${id}`))
}

export function testS3Target(id: string) {
  return unwrap<{ ok: boolean; message: string }>(http.post(`/backups/s3-targets/${id}/test`))
}
