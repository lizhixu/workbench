// Operator tunnel control-plane API (watchmanctl helper endpoints).
// The data plane itself lives on the dedicated -opgrpc TLS port and never
// goes through this REST endpoint.
import { http, unwrap } from './http'

export interface TunnelClientInfo {
  enabled: boolean
  endpoint?: string
  cli: Record<string, string> // platform key -> download URL
}

export interface MyTunnel {
  conn_id: string
  agent_id: string
  target: string
  created_at: string
  idle_seconds: number
}

export function getTunnelClientInfo() {
  return unwrap<TunnelClientInfo>(http.get('/tunnels/client-info'))
}

export function listMyTunnels() {
  return unwrap<{ tunnels: MyTunnel[] }>(http.get('/tunnels/mine')).then((r) => r.tunnels ?? [])
}

export function closeMyTunnel(connID: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/tunnels/mine/${connID}`))
}
