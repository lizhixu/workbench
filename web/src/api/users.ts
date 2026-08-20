import { http, unwrap } from './http'
import type { ListResponse, Role, UserRecord } from './types'

export function listUsers() {
  return unwrap<ListResponse<UserRecord>>(http.get('/users'))
}

export function createUser(username: string, password: string, role: Role) {
  return unwrap<{ data: UserRecord }>(http.post('/users', { username, password, role })).then((r) => r.data)
}

export function updateUserRole(username: string, role: Role) {
  return unwrap<{ ok: boolean }>(http.put(`/users/${username}/role`, { role }))
}

export function updatePassword(oldPassword: string, newPassword: string) {
  const username = JSON.parse(localStorage.getItem('watchman_user') || '{}').username
  return unwrap<{ ok: boolean }>(http.put(`/users/${username}/password`, {
    old_password: oldPassword,
    new_password: newPassword,
  }))
}

export function resetPassword(username: string, newPassword: string) {
  return unwrap<{ ok: boolean }>(http.post(`/users/${username}/reset-password`, { new_password: newPassword }))
}

export function deleteUser(username: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/users/${username}`))
}