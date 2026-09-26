import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import type { User } from '../api/types'
import { login as apiLogin, logout as apiLogout } from '../api/auth'
import { useSettingsStore } from './settings'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<User | null>(loadUser())
  const token = ref<string>(localStorage.getItem('watchman_token') || '')

  const isLoggedIn = computed(() => !!token.value)
  const role = computed(() => user.value?.role ?? null)

  async function login(username: string, password: string) {
    const res = await apiLogin(username, password)
    token.value = res.token
    user.value = res.user
    localStorage.setItem('watchman_token', res.token)
    localStorage.setItem('watchman_user', JSON.stringify(res.user))
    // Fresh login without a page reload: pull server-side settings now so the
    // UI theme follows the account immediately.
    useSettingsStore().load()
  }

  function logout() {
    apiLogout()
    token.value = ''
    user.value = null
  }

  function can(action: string): boolean {
    // MVP: admin can everything; operator can operate; viewer is read-only.
    const r = user.value?.role
    if (r === 'admin') return true
    if (r === 'operator') return action !== 'manage_users'
    return false
  }

  return { user, token, isLoggedIn, role, login, logout, can }
})

function loadUser(): User | null {
  const raw = localStorage.getItem('watchman_user')
  if (!raw) return null
  try {
    return JSON.parse(raw) as User
  } catch {
    return null
  }
}