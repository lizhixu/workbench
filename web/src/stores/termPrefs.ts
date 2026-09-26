// Terminal preference facade over the unified settings store.
//
// Preferences live on the server (GET/PUT /me/settings, terminal.* keys) so
// they follow the user across machines. This store keeps the historical public
// API (prefs / themes / load / save / reset) so terminal components and the
// settings page keep working unchanged; persistence is delegated to
// useSettingsStore.
//
// prefs stays a mutable ref: TerminalPane does optimistic theme switching
// (assign, save, roll back on failure).
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { useSettingsStore } from './settings'
import {
  DEFAULT_TERM_PREFS,
  SETTING_KEYS,
  termPrefsFromData,
  termPrefsToData,
  type TermPrefs,
} from '../api/settings'
import { DEFAULT_TERM_THEME, TERM_THEMES } from '../utils/termThemes'

export const useTermPrefsStore = defineStore('termPrefs', () => {
  const settings = useSettingsStore()
  const prefs = ref<TermPrefs>({ ...DEFAULT_TERM_PREFS })
  const loading = ref(false)
  let inflight: Promise<TermPrefs> | null = null

  // Theme list prefers the server schema enum; falls back to the bundled list
  // when the settings document has not loaded (or the endpoint is down).
  const themes = computed<string[]>(() => {
    const fromSchema = settings.schema.find((s) => s.key === SETTING_KEYS.termTheme)?.enum
    return fromSchema?.length ? fromSchema : Object.keys(TERM_THEMES)
  })
  const loaded = computed(() => settings.loaded)

  // load ensures the settings document is fetched (deduplicated by the settings
  // store) and then syncs this facade's prefs ref from it. Concurrent callers
  // (several terminal tabs opening at the same time) share one request.
  function load(force = false): Promise<TermPrefs> {
    if (inflight) return inflight

    loading.value = true
    const fetcher = force ? settings.reload() : settings.load()
    inflight = fetcher
      .then(() => {
        prefs.value = termPrefsFromData(settings.userData)
        return prefs.value
      })
      .catch(() => {
        // Preferences are cosmetic: if the endpoint is unavailable the
        // terminal must still open with defaults.
        return prefs.value
      })
      .finally(() => {
        loading.value = false
        inflight = null
      })
    return inflight
  }

  async function save(next: TermPrefs): Promise<TermPrefs> {
    const data = await settings.saveUserKeys(termPrefsToData(next))
    prefs.value = termPrefsFromData(data)
    return prefs.value
  }

  function reset() {
    prefs.value = { ...DEFAULT_TERM_PREFS, theme: DEFAULT_TERM_THEME }
  }

  return { prefs, themes, loaded, loading, load, save, reset }
})
