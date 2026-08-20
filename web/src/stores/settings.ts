import { defineStore } from 'pinia'
import { ref } from 'vue'

export type ThemeMode = 'dark' | 'light'

export const useSettingsStore = defineStore('settings', () => {
  const themeMode = ref<ThemeMode>(
    (localStorage.getItem('watchman_theme_mode') as ThemeMode) || 'dark'
  )

  function toggleTheme() {
    themeMode.value = themeMode.value === 'dark' ? 'light' : 'dark'
    localStorage.setItem('watchman_theme_mode', themeMode.value)
  }

  function setThemeMode(mode: ThemeMode) {
    themeMode.value = mode
    localStorage.setItem('watchman_theme_mode', mode)
  }

  return {
    themeMode,
    toggleTheme,
    setThemeMode,
  }
})
