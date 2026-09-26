// Saved-command library ("常用命令库") API.
import { http, unwrap } from './http'

export type CommandShell = 'bash' | 'sh' | 'cmd' | 'powershell'

export interface CommandEntry {
  id: string
  name: string
  shell: CommandShell
  content: string
  description: string
  tags?: string[]
  owner?: string
  shared: boolean
  use_count: number
  created_at: string
  updated_at: string
}

export interface CommandEntryInput {
  name: string
  shell: CommandShell
  content: string
  description?: string
  tags?: string[]
  shared?: boolean
}

export function listCommands(params?: { shell?: string; q?: string }) {
  return unwrap<{ data: CommandEntry[] }>(http.get('/commands', { params })).then((r) => r.data ?? [])
}

export function createCommand(input: CommandEntryInput) {
  return unwrap<{ data: CommandEntry }>(http.post('/commands', input)).then((r) => r.data)
}

export function updateCommand(id: string, input: CommandEntryInput) {
  return unwrap<{ data: CommandEntry }>(http.patch(`/commands/${id}`, input)).then((r) => r.data)
}

export function deleteCommand(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/commands/${id}`))
}

// markCommandUsed bumps the usage counter so frequently used entries sort first.
export function markCommandUsed(id: string) {
  return unwrap<{ ok: boolean }>(http.post(`/commands/${id}/use`))
}
