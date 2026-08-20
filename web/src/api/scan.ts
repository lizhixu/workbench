import { http, unwrap } from './http'

export type ScanType = 'baseline' | 'intrusion' | 'vuln'
export type ScanStatus = 'running' | 'completed' | 'failed'

export interface ScanFinding {
  category: string
  severity: 'critical' | 'high' | 'medium' | 'low' | 'info'
  title: string
  detail: string
  suggestion: string
}

export interface ScanJob {
  id: string
  host_id: string
  hostname: string
  type: ScanType
  status: ScanStatus
  started_at: string
  finished_at?: string
  findings_count: number
  findings_json?: ScanFinding[]
  progress: number
  error?: string
}

export interface ScanPriorityItem {
  level: 'critical' | 'high' | 'medium' | 'low'
  title: string
  reason: string
}

export interface ScanRecommendationItem {
  title: string
  severity: 'critical' | 'high' | 'medium' | 'low'
  steps: string
  command?: string
}

export interface ScanReportResponse {
  summary: string
  priorities: ScanPriorityItem[]
  recommendations: ScanRecommendationItem[]
  model: string
}

export interface DiagnoseResponse {
  answer: string
  model: string
  tokens_used?: number
}

export function triggerScan(hostId: string, type: ScanType) {
  return unwrap<{ data: ScanJob }>(http.post(`/hosts/${hostId}/scans`, { type }, { timeout: 300000 }))
    .then((r) => r.data)
}

export function listScans(params?: { host_id?: string; type?: ScanType; status?: ScanStatus }) {
  return unwrap<{ data: ScanJob[] }>(http.get('/scans', { params })).then((r) => r.data)
}

export function getScan(id: string) {
  return unwrap<{ data: ScanJob }>(http.get(`/scans/${id}`)).then((r) => r.data)
}

export function getLatestScan(hostId: string) {
  return unwrap<{ data: ScanJob | null }>(http.get(`/hosts/${hostId}/scans/latest`)).then((r) => r.data)
}

export function analyzeScanReport(scanType: ScanType, findingsJSON: string, hostId?: string) {
  return unwrap<{ data: ScanReportResponse }>(
    http.post('/ai/scan-report', { scan_type: scanType, findings_json: findingsJSON, host_id: hostId }, { timeout: 120000 }),
  ).then((r) => r.data)
}

export function scanReportFollowup(reportContext: string, question: string) {
  return unwrap<{ data: DiagnoseResponse }>(
    http.post('/ai/scan-report/followup', { report_context: reportContext, question }, { timeout: 120000 }),
  ).then((r) => r.data)
}