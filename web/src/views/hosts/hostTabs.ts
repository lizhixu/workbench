// 主机详情页签的单一来源：HostDetail 的二级导航与通用设置「首选页面」
// 下拉框共用这一份，避免两处各自维护导致选项漂移。
// 注意：不能把这份定义放在 HostDetail.vue 的 <script setup> 里 export——
// @vue/compiler-sfc 明确禁止 <script setup> 出现 ES module exports，会导致 vite build 失败。
import type { Component } from 'vue'
import {
  StatsChartOutline,
  ListOutline,
  CubeOutline,
  FolderOpenOutline,
  ShieldCheckmarkOutline,
  TerminalOutline,
  GitNetworkOutline,
} from '@vicons/ionicons5'

export interface HostTabItem {
  key: string
  label: string
  icon: Component
}

export const subNavItems: HostTabItem[] = [
  { key: 'files', label: '文件管理', icon: FolderOpenOutline },
  { key: 'metrics', label: '资源监控', icon: StatsChartOutline },
  { key: 'sysinfo', label: '系统状态', icon: ListOutline },
  { key: 'terminal', label: '在线终端', icon: TerminalOutline },
  { key: 'docker', label: 'Docker', icon: CubeOutline },
  { key: 'vulnerabilities', label: '漏洞管理', icon: ShieldCheckmarkOutline },
  { key: 'network', label: '异地组网', icon: GitNetworkOutline },
]

export const validHostTabs: string[] = subNavItems.map((i) => i.key)
