import { http, unwrap } from './http'

export interface Credential {
  id: string
  name: string
  type: string
  username: string
  secret: string
  host: string
  description: string
  created_at: string
  updated_at: string
}

export function listCredentials() {
  return unwrap<{ data: Credential[] }>(http.get('/vault/credentials')).then((r) => r.data)
}

export function getCredential(id: string) {
  return unwrap<{ data: Credential }>(http.get(`/vault/credentials/${id}`)).then((r) => r.data)
}

export function createCredential(cred: Omit<Credential, 'id' | 'created_at' | 'updated_at'>) {
  return unwrap<{ data: Credential }>(http.post('/vault/credentials', cred)).then((r) => r.data)
}

export function updateCredential(id: string, cred: Partial<Credential>) {
  return unwrap<{ ok: boolean }>(http.put(`/vault/credentials/${id}`, cred))
}

export function deleteCredential(id: string) {
  return unwrap<{ ok: boolean }>(http.delete(`/vault/credentials/${id}`))
}

export function upgradeAgent(hostId: string, version: string, sha256?: string) {
  return unwrap<{ ok: boolean; version: string; message: string }>(
    http.post(`/hosts/${hostId}/upgrade`, { version, sha256 }),
  )
}