// Per-user terminal preferences ("终端偏好"), persisted on the server so the
// terminal looks the same from any machine the user logs in from.
import { http, unwrap } from './http'

export type TermShell = '' | 'bash' | 'sh' | 'cmd' | 'powershell'

export interface TermPrefs {
  username?: string
  theme: string
  /** Empty means "let the agent pick the platform default". */
  default_shell: TermShell
  font_family: string
  font_size: number
  cursor_blink: boolean
  scrollback: number
  updated_at?: string
}

export interface TermPrefsResponse {
  data: TermPrefs
  /** Theme names the server accepts; used to build the picker. */
  themes: string[]
}

// Mirrors prefs.Defaults on the server. Used before the first load resolves
// and when the server has no preferences store configured.
export const DEFAULT_TERM_PREFS: TermPrefs = {
  theme: 'GitHub Dark',
  default_shell: '',
  font_family: 'Consolas, "Cascadia Code", "Courier New", monospace',
  font_size: 14,
  cursor_blink: true,
  scrollback: 2000,
}

export function getTermPrefs() {
  return unwrap<TermPrefsResponse>(http.get('/me/term-prefs'))
}

export function saveTermPrefs(prefs: TermPrefs) {
  return unwrap<{ data: TermPrefs }>(http.put('/me/term-prefs', prefs)).then((r) => r.data)
}
