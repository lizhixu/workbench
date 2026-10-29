import { http, unwrap } from './http'

export interface GitHubAccount {
  login: string
  name: string
  avatar_url: string
  html_url: string
  auth_type: string
  updated_at: string
}

export interface GitProvidersStatus {
  github: {
    connected: boolean
    account?: GitHubAccount
  }
}

export interface GitHubRepo {
  id: number
  name: string
  full_name: string
  private: boolean
  html_url: string
  clone_url: string
  description: string
  default_branch: string
  pushed_at: string
  language: string
  stargazers_count: number
}

export interface GitHubBranch {
  name: string
  protected: boolean
  sha: string
}

export function getGitProviders() {
  return unwrap<{ data: GitProvidersStatus }>(http.get('/git/providers')).then((r) => r.data)
}

export function connectGitHubToken(token: string) {
  return unwrap<{ ok: boolean; data: GitHubAccount }>(
    http.post('/git/github/token', { token }),
  ).then((r) => r.data)
}

export function disconnectGitHub() {
  return unwrap<{ ok: boolean }>(http.delete('/git/github'))
}

export function listGitHubRepos(query = '') {
  return unwrap<{ data: GitHubRepo[] }>(
    http.get('/git/github/repos', { params: { q: query } }),
  ).then((r) => r.data)
}

export function listGitHubBranches(owner: string, repo: string) {
  return unwrap<{ data: GitHubBranch[] }>(
    http.get(`/git/github/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches`),
  ).then((r) => r.data)
}

// ---- GitHub App（App 级推送事件，替代逐仓 Webhook）----

export interface GitHubAppInstallation {
  id: number
  account_login: string
  account_type: string
  target_type: string
  repos?: string[]
  suspended?: boolean
  updated_at: string
}

export interface GitHubAppStatus {
  configured: boolean
  app_id?: number
  slug?: string
  webhook_url: string
  installations: GitHubAppInstallation[]
}

export interface GitHubAppInstalledRepo {
  account: string
  full_name: string
}

export interface GitHubAppManifest {
  manifest: Record<string, unknown>
  /** 实际用于 manifest 的名称：请求未填时由服务端生成唯一值。 */
  name: string
  state: string
  submit_url: string
  panel_url: string
  public_url_configured: boolean
}

export function getGitHubAppStatus() {
  return unwrap<GitHubAppStatus>(http.get('/github-app/status'))
}

export function createGitHubAppManifest(name: string, target: string, org?: string) {
  return unwrap<GitHubAppManifest>(
    http.post('/github-app/manifest', { name, target, org: org || undefined }),
  )
}

export function exchangeGitHubAppManifest(code: string, state: string) {
  return unwrap<{ ok: boolean; app_id: number; slug: string }>(
    http.post('/github-app/manifest/exchange', { code, state }),
  )
}

export function saveGitHubAppConfig(appId: number, privateKey: string, webhookSecret: string) {
  return unwrap<{ ok: boolean; app_id: number }>(
    http.post('/github-app/config', { app_id: appId, private_key: privateKey, webhook_secret: webhookSecret }),
  )
}

export function disconnectGitHubApp() {
  return unwrap<{ ok: boolean }>(http.delete('/github-app'))
}

export function syncGitHubAppInstallations() {
  // 同步可能拉多个安装的仓库清单，给足 60s（默认 axios 超时仅 15s）。
  return unwrap<{ installations: GitHubAppInstallation[] }>(
    http.post('/github-app/installations/sync', undefined, { timeout: 60000 }),
  ).then((r) => r.installations)
}

export function listGitHubAppRepos() {
  return unwrap<{ repos: GitHubAppInstalledRepo[] }>(
    http.get('/github-app/installations/repos'),
  ).then((r) => r.repos)
}

export function listGitHubAppBranches(owner: string, repo: string) {
  return unwrap<{ branches: string[] }>(
    http.get(`/github-app/repos/${encodeURIComponent(owner)}/${encodeURIComponent(repo)}/branches`),
  ).then((r) => r.branches)
}

export function switchAppToGitHubApp(appId: string) {
  return unwrap<{ ok: boolean; message: string; data: unknown }>(
    http.post(`/apps/${appId}/github-app/switch`),
  )
}
