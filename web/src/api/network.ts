import { http, unwrap } from './http'

export interface NetworkConfig {
  control_plane: 'headscale' | 'tailscale'
  server_url: string
  auth_key: string // 只写：GET 永远返回 ""，不回显密钥
  auth_key_set?: boolean // GET 返回是否已配置密钥
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
  error?: string
  hint?: string
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
  // 服务端探测最长 20 秒（tailscale status --json）+ 15 秒 Agent 回传余量，
  // 默认 15 秒会在慢主机上误杀请求，按惯例放宽到 25 秒。
  return unwrap<{ data: NetworkNode }>(
    http.post(`/network/nodes/${hostId}/check`, undefined, { timeout: 25000 }),
  ).then((r) => r.data)
}

// 只读实时探测主机的组网 IP（live 优先、缓存兜底）。创建应用页选中主机时
// 调用，避免展示因 --reset 换 IP 而过期的缓存值；只读端点，不记审计。
export function probeMeshIP(hostId: string) {
  return unwrap<{ data: { host_id: string; ip: string; source: string } }>(
    http.get(`/network/nodes/${hostId}/mesh-ip`),
  ).then((r) => r.data)
}

// 异步组网任务：install/join 不再同步等远端命令跑完（安装最长 300 秒，
// 同步请求会被 Cloudflare 代理 100 秒切断、造成假超时）。POST 只做校验
// 并立即返回 202 + task_id，任务在服务端后台跑，前端轮询 getNetworkTask
// 直至 success/failed。
export interface NetworkTask {
  task_id: string
  host_id: string
  kind: 'install' | 'join'
  status: 'running' | 'success' | 'failed'
  message: string
  detail?: string
  ip?: string
  previous_ip?: string
  ip_changed?: boolean
  duration_ms?: number
  created_at?: string
  finished_at?: string
}

export interface NetworkTaskAccepted {
  ok: boolean
  task_id: string
  status: 'running'
  message: string
}

export function startInstallNetworkNode(hostId: string) {
  return unwrap<NetworkTaskAccepted>(http.post(`/network/nodes/${hostId}/install`))
}

export function startJoinNetworkNode(hostId: string, req: JoinRequest) {
  return unwrap<NetworkTaskAccepted>(http.post(`/network/nodes/${hostId}/join`, req))
}

export function getNetworkTask(hostId: string, taskId: string) {
  return unwrap<{ data: NetworkTask }>(http.get(`/network/nodes/${hostId}/tasks/${taskId}`)).then(
    (r) => r.data,
  )
}

// 发起一个异步组网任务并等到终态（单机与批量共用）：POST 拿 task_id 后
// 每 2 秒轮询一次，上限 6 分钟；失败抛错（message + detail）。轮询超时
// 不报失败，只提示点探测确认——任务仍可能在服务端继续跑。
export async function runNetworkTask(
  hostId: string,
  start: () => Promise<NetworkTaskAccepted>,
): Promise<NetworkTask> {
  const { task_id } = await start()
  const deadline = Date.now() + 360000
  for (;;) {
    const task = await getNetworkTask(hostId, task_id)
    if (task.status === 'success' || task.status === 'failed') {
      if (task.status === 'failed') {
        throw new Error(task.detail ? `${task.message}：${task.detail}` : task.message)
      }
      return task
    }
    if (Date.now() > deadline) {
      throw new Error('任务轮询超时，服务端可能仍在执行，请稍后点探测确认状态')
    }
    await new Promise((r) => setTimeout(r, 2000))
  }
}

export function leaveNetworkNode(hostId: string, action: 'down' | 'logout' = 'down') {
  return unwrap<{ ok: boolean; message: string; data: NetworkNode }>(
    http.post(`/network/nodes/${hostId}/leave?action=${action}`),
  )
}

export function pingNetworkNode(hostId: string, target: string, count = 3) {
  // 服务端 ping 最长 40 秒，旧版 flag 失败还会再花 40 秒重试一次，
  // 默认 15 秒会在慢主机上误杀请求，放宽到 90 秒。
  return unwrap<PingResult>(
    http.post(`/network/nodes/${hostId}/ping`, { target, count }, { timeout: 90000 }),
  )
}
