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
  showTips: 'appearance.show_tips',
  termTheme: 'terminal.theme',
  termShell: 'terminal.default_shell',
  termFontFamily: 'terminal.font_family',
  termFontSize: 'terminal.font_size',
  termCursorBlink: 'terminal.cursor_blink',
  termScrollback: 'terminal.scrollback',
  defaultHostTab: 'navigation.default_host_tab',
  filesDefaultPath: 'files.default_path',
  secureEntryEnabled: 'security.secure_entry_enabled',
  secureEntryPath: 'security.secure_entry_path',
  joinBetaProgram: 'system.join_beta_program',
  // Certificate center timing (system scope, admin only).
  certAutoRenewDays: 'certs.auto_renew_days',
  certExpiryReminderDays: 'certs.expiry_reminder_days',
  // Panel domain and public access (system scope).
  // 绑定域名即自动启用严格域名限制（只能通过该域名访问面板），无独立开关。
  publicURL: 'server.public_url',
  // Panel timezone (system scope): IANA name; '' = server OS timezone.
  timezone: 'server.timezone',
  panelDomain: 'security.panel_domain',
  // Panel SSL / HTTPS (system scope)
  panelSSLEnabled: 'security.panel_ssl_enabled',
  panelSSLMode: 'security.panel_ssl_mode',
  panelSSLCertID: 'security.panel_ssl_cert_id',
  panelSSLCertPEM: 'security.panel_ssl_cert_pem',
  panelSSLKeyPEM: 'security.panel_ssl_key_pem',
  panelForceHTTPS: 'security.panel_force_https',
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
  /** Keys the user explicitly saved; everything else in data is a server default. */
  stored_keys: string[]
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
  return unwrap<SettingsResponse>(http.put('/me/settings', { data }))
}

export function getSystemSettings() {
  return unwrap<SettingsResponse>(http.get('/settings'))
}

export function saveSystemSettings(data: SettingsData) {
  return unwrap<SettingsResponse>(http.put('/settings', { data }))
}

export interface PanelCertStatus {
  ssl_enabled: boolean
  ssl_mode: string
  ssl_cert_id: string
  panel_domain: string
  strict_domain: boolean
  force_https: boolean
  public_url: string
  active: boolean
  source: string
  subject: string
  issuer: string
  dns_names: string[]
  not_after?: string
  days_left: number
  error?: string
}

export function getPanelCertStatus() {
  return unwrap<PanelCertStatus>(http.get('/system/panel-cert'))
}

// Panel timezone for client-side time rendering. GET /version is readable by
// every signed-in role (unlike the admin-only system settings), so this is
// how non-admin clients learn the zone. '' = follow the browser timezone.
export function getPanelTimezone() {
  return unwrap<{ timezone?: string }>(http.get('/version')).then((r) => r.timezone || '')
}

