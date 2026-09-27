import type { Component } from 'vue'
import {
  SettingsOutline,
  TerminalOutline,
  CodeSlashOutline,
  PulseOutline,
  SparklesOutline,
  LogoGithub,
  ShieldCheckmarkOutline,
  LockClosedOutline,
  ArchiveOutline,
  ArrowUpCircleOutline,
  DownloadOutline,
  KeyOutline,
} from '@vicons/ionicons5'
import GeneralPrefs from './GeneralPrefs.vue'
import TerminalPrefs from './TerminalPrefs.vue'
import CommandLibrary from './CommandLibrary.vue'
import SystemStatus from './SystemStatus.vue'
import AiConfig from './AiConfig.vue'
import GitProviders from './GitProviders.vue'
import CommandPolicy from './CommandPolicy.vue'
import BackupRestore from './BackupRestore.vue'
import SystemUpgrade from './SystemUpgrade.vue'
import InstallDeploy from './InstallDeploy.vue'
import CertDomain from './CertDomain.vue'
import SecureEntry from './SecureEntry.vue'

export interface SettingsSection {
  key: string
  label: string
  icon: Component
  component: Component
  /** 搜索关键词：空格分隔 */
  keywords: string
  group: string
  /** 仅管理员可见（证书签发/域名绑定等高危入口） */
  adminOnly?: boolean
}

export const SETTINGS_GROUPS = ['个人偏好', '系统管理'] as const

export const settingsSections: SettingsSection[] = [
  { key: 'general', label: '通用设置', icon: SettingsOutline, component: GeneralPrefs, keywords: '通用 首选页面 默认页签 提示语 文件路径', group: '个人偏好' },
  { key: 'terminal', label: '终端偏好', icon: TerminalOutline, component: TerminalPrefs, keywords: '终端 主题 字体 字号 shell 光标 回滚', group: '个人偏好' },
  { key: 'commands', label: '命令库', icon: CodeSlashOutline, component: CommandLibrary, keywords: '命令库 常用命令 推送命令 共享命令', group: '个人偏好' },
  { key: 'status', label: '系统状态', icon: PulseOutline, component: SystemStatus, keywords: '系统状态 健康 版本 在线主机', group: '系统管理' },
  { key: 'ai', label: 'AI 大模型', icon: SparklesOutline, component: AiConfig, keywords: 'AI 大模型 deepseek openai ollama 助手 诊断', group: '系统管理' },
  { key: 'git', label: '代码源集成', icon: LogoGithub, component: GitProviders, keywords: 'git github 代码源 仓库 webhook 自动部署', group: '系统管理' },
  { key: 'policy', label: '高危命令控制', icon: ShieldCheckmarkOutline, component: CommandPolicy, keywords: '高危 命令 拦截 黑名单 白名单 二次确认', group: '系统管理' },
  { key: 'backup', label: '备份恢复', icon: ArchiveOutline, component: BackupRestore, keywords: '备份 恢复 迁移', group: '系统管理' },
  { key: 'upgrade', label: '系统升级', icon: ArrowUpCircleOutline, component: SystemUpgrade, keywords: '升级 版本 agent 更新 重启', group: '系统管理' },
  { key: 'install', label: '安装部署', icon: DownloadOutline, component: InstallDeploy, keywords: '安装 部署 一键安装 二进制 注册令牌', group: '系统管理' },
  { key: 'cert-domain', label: '证书与域名', icon: LockClosedOutline, component: CertDomain, keywords: '证书 域名 SSL ACME 签发 续期 反代 绑定', group: '系统管理', adminOnly: true },
  { key: 'secure-entry', label: '安全入口', icon: KeyOutline, component: SecureEntry, keywords: '安全入口 登录入口 隐藏面板 秘密地址 安全', group: '系统管理', adminOnly: true },
]

export const DEFAULT_SECTION_KEY = 'general'

export function resolveSectionKey(raw: unknown, allowed: SettingsSection[] = settingsSections): string {
  const key = typeof raw === 'string' ? raw : ''
  // 非法 key 或无权限的 adminOnly 分区都回退到默认分区（安全入口）。
  return allowed.some((s) => s.key === key) ? key : DEFAULT_SECTION_KEY
}
