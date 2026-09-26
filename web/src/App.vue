<script setup lang="ts">
import { computed, watchEffect } from 'vue'
import { RouterView } from 'vue-router'
import {
  NMessageProvider,
  NDialogProvider,
  NConfigProvider,
  darkTheme,
  type GlobalThemeOverrides,
  zhCN,
  dateZhCN,
} from 'naive-ui'
import { useSettingsStore } from './stores/settings'

const settingsStore = useSettingsStore()

// Pull server-side settings (incl. the UI theme migrated from localStorage).
// Deduplicated inside the store, so calling it here and after login is safe.
settingsStore.load()

watchEffect(() => {
  const mode = settingsStore.themeMode
  document.documentElement.setAttribute('data-theme', mode)
  if (mode === 'dark') {
    document.documentElement.classList.add('dark')
    document.documentElement.classList.remove('light')
  } else {
    document.documentElement.classList.add('light')
    document.documentElement.classList.remove('dark')
  }
  // Feature-tip paragraphs marked with .tip-hint are hidden when the user
  // disables appearance.show_tips in General settings.
  document.documentElement.classList.toggle('hide-tips', !settingsStore.showTips)
})

const currentTheme = computed(() => {
  return settingsStore.themeMode === 'dark' ? darkTheme : null
})

const themeOverrides = computed<GlobalThemeOverrides>(() => {
  const isDark = settingsStore.themeMode === 'dark'
  return {
    common: {
      primaryColor: '#6366f1',
      primaryColorHover: '#818cf8',
      primaryColorPressed: '#4f46e5',
      primaryColorSuppl: '#818cf8',
      successColor: '#10b981',
      warningColor: '#f59e0b',
      errorColor: '#ef4444',
      infoColor: '#3b82f6',
      bodyColor: isDark ? '#0b0f19' : '#f1f5f9',
      cardColor: isDark ? '#131b2e' : '#ffffff',
      tableColor: isDark ? '#131b2e' : '#ffffff',
      modalColor: isDark ? '#1a233a' : '#ffffff',
      popoverColor: isDark ? '#1a233a' : '#ffffff',
      inputColor: isDark ? '#1a233a' : '#ffffff',
      borderColor: isDark ? 'rgba(255, 255, 255, 0.08)' : '#e2e8f0',
      dividerColor: isDark ? 'rgba(255, 255, 255, 0.08)' : '#e2e8f0',
      textColorBase: isDark ? '#f3f4f6' : '#1e293b',
      textColor1: isDark ? '#f3f4f6' : '#0f172a',
      textColor2: isDark ? '#d1d5db' : '#334155',
      textColor3: isDark ? '#9ca3af' : '#64748b',
      borderRadius: '6px',
      fontFamily:
        "-apple-system, BlinkMacSystemFont, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', 'Segoe UI', Roboto, sans-serif",
      fontFamilyMono:
        "'JetBrains Mono', Consolas, 'Liberation Mono', Menlo, monospace",
    },
    Card: {
      borderRadius: '8px',
      borderColor: isDark ? 'rgba(255, 255, 255, 0.08)' : '#e2e8f0',
      color: isDark ? '#131b2e' : '#ffffff',
    },
    DataTable: {
      borderColor: isDark ? 'rgba(255, 255, 255, 0.08)' : '#e2e8f0',
      tdColorHover: isDark ? 'rgba(255, 255, 255, 0.04)' : '#f8fafc',
      thColor: isDark ? '#18223a' : '#f8fafc',
      thTextColor: isDark ? '#9ca3af' : '#475569',
      thFontWeight: '600',
    },
    Button: {
      borderRadiusMedium: '6px',
      borderRadiusSmall: '5px',
      borderRadiusTiny: '4px',
    },
    Input: {
      borderRadius: '6px',
      borderColor: isDark ? 'rgba(255, 255, 255, 0.12)' : '#cbd5e1',
    },
    Tag: {
      borderRadius: '4px',
    },
  }
})
</script>

<template>
  <NConfigProvider
    :theme="currentTheme"
    :theme-overrides="themeOverrides"
    :locale="zhCN"
    :date-locale="dateZhCN"
  >
    <NMessageProvider>
      <NDialogProvider>
        <RouterView />
      </NDialogProvider>
    </NMessageProvider>
  </NConfigProvider>
</template>
