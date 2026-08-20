// Central TypeScript types mirroring the control server REST DTOs.

export type Role = 'admin' | 'operator' | 'viewer'

export interface User {
  username: string
  role: Role
}

export interface UserRecord {
  id: string
  username: string
  role: Role
  created_at: string
  updated_at: string
}

export interface LoginResponse {
  token: string
  user: User
  expires_in: number
}

export interface EnrollResponse {
  enroll_token: string
  expires_in: number
  install: string
}

export interface Host {
  id: string
  hostname: string
  os: string
  arch: string
  distro: string
  agent_version: string
  status: 'online' | 'offline' | 'maintenance'
  last_seen: string
  registered: string
  group: string
  tags: string[]
  uptime: number
  cpu_cores?: number
  mem_total?: number
  internal_ip?: string
  public_ip?: string
  location?: string
}

export interface ListResponse<T> {
  data: T[]
  total: number
}

export interface HealthResponse {
  ok: boolean
  agents: number
  version: string
}

// ---- Terminal ----
export interface TerminalSession {
  session_id: string
  ws_url: string
}

// ---- Files ----
export interface FileInfo {
  name: string
  path: string
  size: number
  is_dir: boolean
  mode: string
  mod_time: string
}

// ---- Exec ----
export interface ExecResult {
  exec_id: string
  exit_code: number
  stdout: string
  stderr: string
  duration_ms: number
  error: string
}

export interface BatchExecResult {
  host_id: string
  exit_code: number
  stdout: string
  stderr: string
  error: string
}

// ---- Metrics ----
export interface Mount {
  path: string
  total: number
  used: number
}

export interface Metrics {
  ts: number
  cpu_usage: number
  mem_usage: number
  mem_total: number
  mem_used: number
  net_rx: number
  net_tx: number
  disk_read: number
  disk_write: number
  mounts: Mount[]
}

// ---- Docker ----
export interface DockerContainer {
  ID: string
  Image: string
  Names: string
  Status: string
  Ports: string
}

export interface DockerImage {
  Repository: string
  Tag: string
  ID: string
  Size: string
}

// ---- Session / Audit ----
export interface SessionRecord {
  id: string
  agent_id: string
  hostname: string
  operator: string
  started_at: string
  ended_at: string
  duration_sec: number
  live: boolean
  cast_path: string
}