// Unified settings store (settings-center Phase 2).
//
// Owns the user-scope settings document plus the UI theme mode. The theme used
// to live only in browser localStorage; it is now a server-side setting
// (appearance.theme_mode) so it follows the user across machines, with
// localStorage kept as a read cache for first paint.
//
// Migration runs once inside load(): if the server has no theme yet but
// localStorage has one, the local choice is pushed up so nothing is lost.
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  getMySettings,
  saveMySettings,
  SETTING_KEYS,
  type SettingsData,
  type SettingSchemaEntry,
  type ThemeMode,
} from '../api/settings'

export type { ThemeMode }

const LS_THEME_KEY = 'watchman_theme_mode'

function readLocalTheme(): ThemeMode | null {
  const v = localStorage.getItem(LS_THEME_KEY)
  return v === 'dark' || v === 'light' ? v : null
}

export const useSettingsStore = defineStore('settings', () => {
  // Synchronous localStorage seed: first paint must not wait for the network.
  const themeMode = ref<ThemeMode>(readLocalTheme() || 'dark')
  const userData = ref<SettingsData>({})
  const schema = ref<SettingSchemaEntry[]>([])
  const loaded = ref(false)
  let inflight: Promise<void> | null = null

  function load(): Promise<void> {
    if (loaded.value) return Promise.resolve()
    if (inflight) return inflight
    inflight = getMySettings()
      .then(async (res) => {
        userData.value = res.data || {}
        schema.value = res.schema || []
        const serverTheme = res.data?.[SETTING_KEYS.themeMode]
        if (serverTheme === 'dark' || serverTheme === 'light') {
          themeMode.value = serverTheme
        } else {
          // Server has no theme yet: push the local choice up once so the
          // user's existing preference is preserved server-side.
          const local = readLocalTheme()
          if (local) {
            try {
              const data = await saveMySettings({ [SETTING_KEYS.themeMode]: local })
              userData.value = data
            } catch {
              // Cosmetic only: keep the local value if the push fails.
            }
          }
        }
        localStorage.setItem(LS_THEME_KEY, themeMode.value)
        loaded.value = true
      })
      .catch(() => {
        // Offline or logged out: keep the localStorage value; theme and
        // terminal still render with defaults.
        loaded.value = true
      })
      .finally(() => {
        inflight = null
      })
    return inflight
  }

  function reload(): Promise<void> {
    loaded.value = false
    return load()
  }

  /** Read one user-scope key with a fallback (for facades like termPrefs). */
  function getUserKey<T>(key: string, fallback: T): T {
    const v = userData.value[key]
    return v === undefined || v === null ? fallback : (v as T)
  }

  /** Partial update of user-scope keys; returns the full effective data. */
  async function saveUserKeys(data: SettingsData): Promise<SettingsData> {
    const saved = await saveMySettings(data)
    userData.value = saved
    const t = saved[SETTING_KEYS.themeMode]
    if (t === 'dark' || t === 'light') {
      themeMode.value = t
      localStorage.setItem(LS_THEME_KEY, t)
    }
    return saved
  }

  function setThemeMode(mode: ThemeMode) {
    const prev = themeMode.value
    themeMode.value = mode
    localStorage.setItem(LS_THEME_KEY, mode)
    // Fire-and-forget: the UI updates instantly, the server sync follows.
    // On failure roll back so UI and server never disagree silently.
    saveUserKeys({ [SETTING_KEYS.themeMode]: mode }).catch(() => {
      themeMode.value = prev
      localStorage.setItem(LS_THEME_KEY, prev)
    })
  }

  function toggleTheme() {
    setThemeMode(themeMode.value === 'dark' ? 'light' : 'dark')
  }

  // Whether feature tip paragraphs are shown; drives the hide-tips class on
  // documentElement (see App.vue). Defaults to true before load.
  const showTips = computed(() => getUserKey<boolean>(SETTING_KEYS.showTips, true))

  return {
    themeMode,
    showTips,
    userData,
    schema,
    loaded,
    load,
    reload,
    getUserKey,
    saveUserKeys,
    setThemeMode,
    toggleTheme,
  }
})
