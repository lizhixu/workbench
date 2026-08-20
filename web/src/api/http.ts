// axios instance with JWT injection and 401/403 handling.
import axios, { AxiosError, type AxiosInstance } from 'axios'

const baseURL = import.meta.env.VITE_API_BASE || '/api/v1'

export const http: AxiosInstance = axios.create({
  baseURL,
  timeout: 15000,
})

// Request interceptor: attach bearer token.
http.interceptors.request.use((config) => {
  const token = localStorage.getItem('watchman_token')
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// Response interceptor: unwrap errors, handle 401.
http.interceptors.response.use(
  (resp) => resp,
  (error: AxiosError<{ error?: string; needs_confirm?: boolean; risk_level?: string; matched_pattern?: string }>) => {
    const status = error.response?.status
    const data = error.response?.data
    const msg = data?.error || error.message
    if (status === 401) {
      // Token invalid/expired: clear and redirect to login.
      localStorage.removeItem('watchman_token')
      if (!window.location.pathname.startsWith('/login')) {
        window.location.href = '/login'
      }
    }
    const err = new Error(msg) as Error & { status?: number; needsConfirm?: boolean; riskLevel?: string; matchedPattern?: string }
    err.status = status
    err.needsConfirm = data?.needs_confirm
    err.riskLevel = data?.risk_level
    err.matchedPattern = data?.matched_pattern
    return Promise.reject(err)
  },
)

// unwrap helper: return resp.data directly.
export async function unwrap<T>(p: Promise<{ data: T }>): Promise<T> {
  const r = await p
  return r.data
}