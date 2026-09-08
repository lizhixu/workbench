import { http, unwrap } from './http'

export interface NetworkConfig {
  control_plane: 'headscale' | 'tailscale'
  server_url: string
  auth_key: string
  accept_routes: boolean
  advertise_exit_node: boolean
  updated_at?: string
}

export interface NetworkNode {
  host_id: string
  hostname: string
  os: string
  arch: string
  internal_ip: string
  public_ip: string
  agent_online: boolean
  installed: boolean
  online: boolean
  ip: string
  ipv6: string
  node_name: string
  version: string
  direct: boolean
  derp: string
  latency_ms: number
  subnets: string[]
  last_checked?: string
  error_message?: string
}

export interface JoinRequest {
  auth_key?: string
  server_url?: string
  accept_routes?: boolean
  advertise_routes?: string
  advertise_exit_node?: boolean
  hostname?: string
  reset?: boolean
}

export interface PingResult {
  ok: boolean
  output: string
  direct: boolean
  derp: string
  latency_ms: number
}

export function getNetworkConfig() {
  return unwrap<{ data: NetworkConfig }>(http.get('/network/config')).then((r) => r.data)
}

export function updateNetworkConfig(cfg: NetworkConfig) {
  return unwrap<{ data: NetworkConfig }>(http.put('/network/config', cfg)).then((r) => r.data)
}

export function listNetworkNodes() {
  return unwrap<{ data: NetworkNode[] }>(http.get('/network/nodes')).then((r) => r.data)
}

export function checkNetworkNode(hostId: string) {
  return unwrap<{ data: NetworkNode }>(http.post(`/network/nodes/${hostId}/check`)).then((r) => r.data)
}

export function installNetworkNode(hostId: string) {
  return unwrap<{ ok: boolean; message: string; duration_ms?: number; output?: string }>(
    http.post(`/network/nodes/${hostId}/install`),
  )
}

export function joinNetworkNode(hostId: string, req: JoinRequest) {
  return unwrap<{ ok: boolean; message: string; ip?: string; data: NetworkNode }>(
    http.post(`/network/nodes/${hostId}/join`, req),
  )
}

export function leaveNetworkNode(hostId: string, action: 'down' | 'logout' = 'down') {
  return unwrap<{ ok: boolean; message: string; data: NetworkNode }>(
    http.post(`/network/nodes/${hostId}/leave?action=${action}`),
  )
}

export function pingNetworkNode(hostId: string, target: string, count = 3) {
  return unwrap<PingResult>(http.post(`/network/nodes/${hostId}/ping`, { target, count }))
}
