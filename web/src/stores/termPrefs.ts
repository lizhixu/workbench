// Terminal preference store. Preferences live on the server (GET/PUT
// /me/term-prefs) so they follow the user across machines; this store caches
// them for the session and is the single source both the terminal component
// and the settings page read from.
import { defineStore } from 'pinia'
import { ref } from 'vue'
import {
  DEFAULT_TERM_PREFS,
  getTermPrefs,
  saveTermPrefs,
  type TermPrefs,
} from '../api/prefs'
import { DEFAULT_TERM_THEME, TERM_THEMES } from '../utils/termThemes'

export const useTermPrefsStore = defineStore('termPrefs', () => {
  const prefs = ref<TermPrefs>({ ...DEFAULT_TERM_PREFS })
  const themes = ref<string[]>(Object.keys(TERM_THEMES))
  const loaded = ref(false)
  const loading = ref(false)
  let inflight: Promise<TermPrefs> | null = null

  // load fetches once per session. Concurrent callers (several terminal tabs
  // opening at the same time) share one request.
  function load(force = false): Promise<TermPrefs> {
    if (loaded.value && !force) return Promise.resolve(prefs.value)
    if (inflight) return inflight

    loading.value = true
    inflight = getTermPrefs()
      .then((res) => {
        if (res.data) prefs.value = { ...DEFAULT_TERM_PREFS, ...res.data }
        if (res.themes?.length) themes.value = res.themes
        loaded.value = true
        return prefs.value
      })
      .catch(() => {
        // Preferences are cosmetic: if the endpoint is unavailable the
        // terminal must still open with defaults.
        loaded.value = true
        return prefs.value
      })
      .finally(() => {
        loading.value = false
        inflight = null
      })
    return inflight
  }

  async function save(next: TermPrefs): Promise<TermPrefs> {
    const saved = await saveTermPrefs(next)
    prefs.value = { ...DEFAULT_TERM_PREFS, ...saved }
    loaded.value = true
    return prefs.value
  }

  function reset() {
    prefs.value = { ...DEFAULT_TERM_PREFS, theme: DEFAULT_TERM_THEME }
  }

  return { prefs, themes, loaded, loading, load, save, reset }
})
