import { http, unwrap } from './http'

// ---- GitHub App（App 级仓库授权与自动部署）----

export interface GitHubAppRepoMeta {
  full_name: string
  private?: boolean
  description?: string
  default_branch?: string
  language?: string
  pushed_at?: string
}

export interface GitHubAppInstallation {
  id: number
  account_login: string
  account_type: string
  target_type: string
  repos?: GitHubAppRepoMeta[]
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
  installation_id: number
  account: string
  account_type: string
  full_name: string
  private: boolean
  description?: string
  default_branch: string
  language?: string
  pushed_at?: string
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
