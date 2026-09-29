import { http, unwrap } from './http'

export interface ServerUpgradeResult {
  ok: boolean
  message: string
  path: string
  size: number
  bak_path?: string
}

export interface BatchUpgradeAgentsResult {
  ok: boolean
  message: string
  count: number
  hosts: string[]
}

export interface UpdateInfo {
  current_version: string
  latest_version: string
  has_update: boolean
  is_beta: boolean
  release_notes: string
  published_at: string
  asset_url: string
  asset_name: string
  asset_size: number
  checksums_url: string
  checked_at: string
  channel: string
}

export interface OnlineUpgradeResult {
  ok: boolean
  message: string
  target_version: string
}

/**
 * 检查控制端最新版本发布信息（对比当前版本与官方 GitHub Release）。
 * force=true 强制绕过 5 分钟服务端缓存。
 */
export function checkSystemUpdate(force = false, beta?: boolean) {
  const params: Record<string, any> = {}
  if (force) params.force = 'true'
  if (beta !== undefined) params.beta = beta ? 'true' : 'false'
  return unwrap<UpdateInfo>(http.get('/system/check-update', { params, timeout: 20000 }))
}

/**
 * 触发控制端一键在线自升级（从官方 Release 下载目标安装包并原子替换，随后平滑重启）。
 */
export function onlineUpgradeServer(targetVersion?: string) {
  return unwrap<OnlineUpgradeResult>(
    http.post('/system/online-upgrade', { target_version: targetVersion }, { timeout: 600000 }),
  )
}

/**
 * 上传控制端可执行文件二进制，原子替换运行程序并备份旧版。
 */
export function uploadServerBinary(file: File, onProgress?: (percent: number) => void) {
  const form = new FormData()
  form.append('file', file)
  return unwrap<ServerUpgradeResult>(
    http.post('/system/upgrade', form, {
      timeout: 300000,
      headers: { 'Content-Type': 'multipart/form-data' },
      onUploadProgress: (e) => {
        if (e.total && onProgress) {
          onProgress(Math.round((e.loaded * 100) / e.total))
        }
      },
    }),
  )
}

/**
 * 触发控制端受控平滑重启流程（向全网 Agent 下发维护信令预告）。
 */
export function restartServer() {
  return unwrap<{ ok: boolean; message: string }>(
    http.post('/system/restart'),
  )
}

/**
 * 批量升级全网或指定在线 Agent 主机（自动打上维护标签并下发升级）。
 * force=true 时即使版本一致也重新推送，便于开发版/同版本号场景重刷。
 */
export function batchUpgradeAgents(hostIds?: string[], force = false) {
  return unwrap<BatchUpgradeAgentsResult>(
    http.post('/system/upgrade-agents', { host_ids: hostIds || [], force }, { timeout: 30000 }),
  )
}
