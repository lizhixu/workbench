// Host grouping + per-group user authorization ("分组与权限") API.
import { http, unwrap } from './http'

export type GrantRole = 'operate' | 'view'

export interface HostGroup {
  id: string
  name: string
  description: string
  parent_id?: string
  created_at: string
  updated_at: string
  host_count: number
  user_count: number
}

export interface GroupGrant {
  username: string
  group_id: string
  role: GrantRole
  granted_at: string
  granted_by?: string
}

export interface GroupInput {
  name: string
  description?: string
  parent_id?: string
}

export interface MyGroupScope {
  restricted: boolean
  groups: string[]
}

export function listGroups() {
  return unwrap<{ data: HostGroup[]; total: number }>(http.get('/groups')).then((r) => r.data ?? [])
}

export function createGroup(input: GroupInput) {
  return unwrap<{ data: HostGroup }>(http.post('/groups', input)).then((r) => r.data)
}

export function updateGroup(id: string, input: GroupInput) {
  return unwrap<{ data: HostGroup; hosts_migrated: number }>(http.patch(`/groups/${id}`, input))
}

export function deleteGroup(id: string) {
  return unwrap<{ ok: boolean; hosts_cleared: number }>(http.delete(`/groups/${id}`))
}

export function listGroupUsers(id: string) {
  return unwrap<{ data: GroupGrant[] }>(http.get(`/groups/${id}/users`)).then((r) => r.data ?? [])
}

export function setGroupUser(id: string, username: string, role: GrantRole) {
  return unwrap<{ ok: boolean }>(http.post(`/groups/${id}/users`, { username, role }))
}

export function revokeGroupUser(id: string, username: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/groups/${id}/users/${encodeURIComponent(username)}`))
}

// getMyGroupScope reports whether the current user is limited to certain groups.
export function getMyGroupScope() {
  return unwrap<{ data: MyGroupScope }>(http.get('/me/groups')).then((r) => r.data)
}
