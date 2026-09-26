// Unified settings API client (settings-center Phase 2).
//
// Replaces api/prefs.ts: every setting is addressed by a dotted key in one of
// two scopes (system / user). The server validates values against its key
// registry and returns the schema so pickers are rendered from the server
// instead of hardcoding options twice.
import { http, unwrap } from './http'

export type TermShell = '' | 'bash' | 'sh' | 'cmd' | 'powershell'

export type ThemeMode = 'dark' | 'light'

export interface TermPrefs {
  theme: string
  /** Empty means "let the agent pick the platform default". */
  default_shell: TermShell
  font_family: string
  font_size: number
  cursor_blink: boolean
  scrollback: number
}

// Setting key names, mirroring server/internal/settings Definitions.
export const SETTING_KEYS = {
  themeMode: 'appearance.theme_mode',
  termTheme: 'terminal.theme',
  termShell: 'terminal.default_shell',
  termFontFamily: 'terminal.font_family',
  termFontSize: 'terminal.font_size',
  termCursorBlink: 'terminal.cursor_blink',
  termScrollback: 'terminal.scrollback',
} as const

export interface SettingSchemaEntry {
  key: string
  scope: 'system' | 'user'
  kind: 'string' | 'int' | 'bool' | 'enum'
  title: string
  default: unknown
  enum?: string[]
}

export type SettingsData = Record<string, unknown>

export interface SettingsResponse {
  data: SettingsData
  schema: SettingSchemaEntry[]
}

// Defaults used before the first load resolves and when the server has no
// settings store configured. They match the server-side registry defaults.
export const DEFAULT_TERM_PREFS: TermPrefs = {
  theme: 'GitHub Dark',
  default_shell: '',
  font_family: 'Consolas, "Cascadia Code", "Courier New", monospace',
  font_size: 14,
  cursor_blink: true,
  scrollback: 2000,
}

/** Build a TermPrefs view from raw user-scope settings data. */
export function termPrefsFromData(data: SettingsData): TermPrefs {
  const get = <T>(key: string, fallback: T): T => {
    const v = data[key]
    return v === undefined || v === null ? fallback : (v as T)
  }
  return {
    theme: get(SETTING_KEYS.termTheme, DEFAULT_TERM_PREFS.theme),
    default_shell: get<TermShell>(SETTING_KEYS.termShell, DEFAULT_TERM_PREFS.default_shell),
    font_family: get(SETTING_KEYS.termFontFamily, DEFAULT_TERM_PREFS.font_family),
    font_size: get(SETTING_KEYS.termFontSize, DEFAULT_TERM_PREFS.font_size),
    cursor_blink: get(SETTING_KEYS.termCursorBlink, DEFAULT_TERM_PREFS.cursor_blink),
    scrollback: get(SETTING_KEYS.termScrollback, DEFAULT_TERM_PREFS.scrollback),
  }
}

/** Convert a TermPrefs form back to settings keys for a partial update. */
export function termPrefsToData(p: TermPrefs): SettingsData {
  return {
    [SETTING_KEYS.termTheme]: p.theme,
    [SETTING_KEYS.termShell]: p.default_shell,
    [SETTING_KEYS.termFontFamily]: p.font_family,
    [SETTING_KEYS.termFontSize]: p.font_size,
    [SETTING_KEYS.termCursorBlink]: p.cursor_blink,
    [SETTING_KEYS.termScrollback]: p.scrollback,
  }
}

export function getMySettings() {
  return unwrap<SettingsResponse>(http.get('/me/settings'))
}

export function saveMySettings(data: SettingsData) {
  return unwrap<{ data: SettingsData }>(http.put('/me/settings', { data })).then((r) => r.data)
}

export function getSystemSettings() {
  return unwrap<SettingsResponse>(http.get('/settings'))
}

export function saveSystemSettings(data: SettingsData) {
  return unwrap<{ data: SettingsData }>(http.put('/settings', { data })).then((r) => r.data)
}
