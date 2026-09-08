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
