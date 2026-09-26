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
