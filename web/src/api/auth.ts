import { http, unwrap } from './http'
import type { LoginResponse } from './types'

export function login(username: string, password: string) {
  return unwrap<LoginResponse>(
    http.post('/auth/login', { username, password }),
  )
}

export function logout() {
  localStorage.removeItem('watchman_token')
  localStorage.removeItem('watchman_user')
}