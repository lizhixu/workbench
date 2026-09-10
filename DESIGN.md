# 前端设计文档（Web Console - 牧云主机管理助手 / 云堡垒机）

> **规范基准声明**：本设计文档全量整合并严格对齐后端与需求主文档 `Agent.md`。系统的核心目标为「自部署、自己掌控、永久可用、不依赖第三方 SaaS」。所有功能范围、数据契约、接口命名、权限模型与 AI 增强逻辑均**以 `Agent.md` 为唯一权威标准**。前端同时完整承载截图所识别的双层多页签、双侧边栏、顶部状态横幅与 4-宫格监控等云原生 UI 布局体系。

---

## 1. 产品定位与技术栈规范

### 1.1 产品定位 (与 Agent.md 1 对齐)
自托管的 Web 控制台 + 被管主机 Agent，Agent 主动反向出向回连控制端（gRPC over TLS/443），被管主机零开放端口。控制端构建产物经 `go:embed` 嵌入 Go 单二进制，前端通过同源 `/api/v1` 调 REST、`/api/v1/ws/*` 走 WebSocket。

### 1.2 前端技术栈 (与 Agent.md A.2 对齐)

| 层 | 选型 | 详细说明 |
| --- | --- | --- |
| 框架 | Vue 3 + `<script setup>` + TypeScript | 组合式 API，Strict TypeScript 类型检查 |
| 构建 | Vite 5 | 开发 HMR 代理；生产构建产物打包至 `dist/` 供 Go Embed |
| UI 库 | **Naive UI** (暗色/亮色双原生主题) | 主体 UI 组件库，原生 TypeScript 良好支持 |
| 路由 | Vue Router 4 | 路由守卫 + 顶部 Multi-Tab 工作区状态联动 |
| 状态管理 | Pinia | Workspace 标签 / Auth / Hosts / Messages / Sessions / Settings |
| 请求库 | Axios + 统一拦截器 | baseURL `/api/v1`，JWT Bearer Token 注入与 401 自动刷新 |
| WebSocket | 原生 WebSocket + 自动重连客户端 | 用于在线终端流、文件分片传输、消息日志与实时心跳推送 |
| Web 终端 | `@xterm/xterm` + `fit` + `webgl` + `search` + `web-links` | 堡垒机标准终端，支持 CJK / IME 及自适应尺寸 |
| 监控图表 | ECharts 5 (`echarts/core` 按需加载) | 4-宫格资源监控曲线、资产统计仪表盘 |
| 录像回放 | `asciinema-player` (v2 cast 格式) | 会话审计终端回放、时间轴 Scrubbing 与 Marker 标注 |
| 代码编辑器 | Monaco Editor (`@guolao/vue-monaco-editor`) | 文件在线编辑、文本预览、 Monaco Diff 模式 (按需懒加载) |
| 图标库 | `@vicons/ionicons5` / `@vicons/tabler` | Naive UI 官方 Icon 体系 |
| 辅助工具 | `@vueuse/core`、`dayjs`、`lodash-es` | 响应式 utils、时间格式化与高频操作防抖/节流 |

---

## 2. 目录结构与模块划分 (与 Agent.md B.1/B.4 对齐)

```
web/
├── index.html
├── vite.config.ts            # 开发代理 /api → http://localhost:443
├── tsconfig.json
├── package.json
└── src/
    ├── main.ts
    ├── App.vue
    ├── router/
    │   ├── index.ts          # 路由配置、全局守卫、Tab 状态拦截
    │   └── routes.ts         # 路由表定义
    ├── api/                  # REST API 客户端 (与 Agent.md B.4 对齐)
    │   ├── http.ts           # Axios 实例、请求/响应拦截器、JWT 逻辑
    │   ├── auth.ts           # /auth/login, /auth/logout, /auth/me
    │   ├── hosts.ts          # /hosts, /hosts/enroll, /hosts/:id, /accounts, /login-policy
    │   ├── terminal.ts       # /hosts/:id/terminals, /terminals/:sid/share, /sessions
    │   ├── files.ts          # /hosts/:id/files, /files/content, /files/lock, /files/versions
    │   ├── exec.ts           # /exec/tasks, /commands (常用命令库)
    │   ├── monitor.ts        # /hosts/:id/metrics, /alerts, /alert-rules
    │   ├── sysinfo.ts        # /hosts/:id/processes, /ports, /users, /logins
    │   ├── docker.ts         # /hosts/:id/apps, /docker/containers, /docker/images (P2)
    │   ├── security.ts       # /security/scans, /vulnerabilities (P3)
    │   ├── cloud.ts          # /cloud/accounts, /cloud/assets (P2)
    │   ├── network.ts        # /networks/join, /networks/topology (P2)
    │   ├── groups.ts         # /groups, /groups/:id/hosts, /groups/:id/users
    │   ├── users.ts          # /users, /me/term-prefs
    │   ├── audit.ts          # /audit, /sessions/:id/recording
    │   ├── ai.ts             # /ai/config, /ai/chat, /ai/nl2command, /ai/audit, /ai/tools
    │   ├── settings.ts       # /settings, /notify-channels, /notify-rules
    │   ├── system.ts         # /system/health, /system/backup, /system/restore
    │   └── types.ts          # TS DTO 类型契约 (与 Agent.md B.3 数据模型 100% 对齐)
    ├── ws/                   # WebSocket 客户端
    │   ├── client.ts         # 通用 WebSocket：指数退避重连、30s 心跳、帧路由
    │   ├── terminal.ts       # 终端二进制/文本帧处理
    │   └── file.ts           # 文件 Chunk 分片传输 (256KB~1MB) + 校验
    ├── stores/               # Pinia 状态管理
    │   ├── auth.ts           # 用户 token, role (admin/operator/viewer), 组别 Scope
    │   ├── workspace.ts      # 顶部 Multi-Tab 导航工作区状态
    │   ├── hosts.ts          # 主机列表、实时状态 (WS 推送更新)
    │   ├── messages.ts       # 实时消息列表 (消息列表 99+ 未读与日志流)
    │   ├── settings.ts       # 全局主题 (dark/light)、终端偏好、AI 配置缓存
    │   └── sessions.ts       # 活动终端会话、远程协助与终端分享状态
    ├── composables/          # 组合式 Hook (useTerminal, useMetrics, useFileTree, useAI, useAuth)
    ├── components/
    │   ├── layout/           # AppHeader(顶栏+Tab), Sidebar, HostDetailShell
    │   ├── host/             # HostHeaderBanner, HostTable, HostFilterBar, MessageListPanel
    │   ├── terminal/         # TerminalPane, TerminalTabs, RemoteAssistModal, ThemeSelect
    │   ├── file/             # PathBreadcrumb, FileTable, FileToolbar, FileEditor, UploadProgress
    │   ├── monitor/          # MetricGrid (4-宫格), MetricCard, TimeRangePicker
    │   ├── sysinfo/          # ProcessTable, PortList, AccountList, LoginHistoryTable
    │   ├── docker/           # ContainerTable, ImageTable, DockerInstallModal (P2)
    │   ├── security/         # VulnerabilityStatCard, VulnerabilityTable, ScanTrigger (P3)
    │   ├── cloud/            # CloudAccountModal, CloudAssetDashboard (P2)
    │   ├── exec/             # ExecEditor, HostPicker, ResultViewer, CommandLibModal
    │   ├── ai/               # AiChat, Nl2Command, CommandConfirm, AiLogInterpreter
    │   ├── audit/            # AsciinemaPlayer, AuditTable
    │   └── common/           # ConfirmDialog, EmptyState, TagInput, StatusBadge
    ├── views/
    │   ├── auth/Login.vue
    │   ├── hosts/
    │   │   ├── HostList.vue            # 主机列表 (包含左侧分组树 + 实时消息流)
    │   │   └── HostDetail.vue          # 主机详情 (固定顶部状态栏 + 左侧二级 Icon 导航)
    │   ├── hosts/tabs/
    │   │   ├── Overview.vue            # 4-宫格资源监控 (实时/历史)
    │   │   ├── Sysinfo.vue             # 进程清单 / 网络端口 / 系统账号 / 登录历史
    │   │   ├── Terminal.vue            # 多标签终端 + 远程协助 Modal
    │   │   ├── Files.vue               # 文件导航面包屑 + 虚拟树 + 上传/编辑
    │   │   ├── Docker.vue              # (P2) Docker 容器与镜像管理
    │   │   ├── Vulnerabilities.vue     # (P3) 服务器漏洞与安全基线
    │   │   ├── Apps.vue                # (P2) 应用市场一键部署
    │   │   └── CloudAssets.vue         # (P2) 云资产与云账号绑定
    │   ├── exec/ExecPush.vue           # 批量命令推送与常用命令库
    │   ├── sessions/SessionList.vue    # 会话审计列表 + asciinema 回放
    │   ├── alerts/
    │   │   ├── AlertList.vue           # 活跃与历史告警
    │   │   └── AlertRules.vue          # 告警规则与通知渠道 CRUD
    │   ├── groups/GroupManage.vue      # 分组树 + 主机/用户授权 (双视角)
    │   ├── users/UserManage.vue        # 用户管理与 RBAC 角色分配
    │   ├── audit/AuditList.vue         # 系统全量操作审计日志
    │   ├── ai/AiChat.vue               # AI 智能运维助手 (Tool Calling 查平台数据)
    │   ├── settings/Settings.vue       # 通用设置 / 终端偏好 / AI Provider 配置 / 通知渠道
    │   └── system/Health.vue           # 系统健康自检 / 离线数据备份与恢复
    └── styles/               # 主题 CSS 变量 (dark & light)
```

---

## 3. 路由设计与导航守卫 (与 Agent.md 3 & B.4 对齐)

### 3.1 路由映射表

```
/login                                  Login.vue（公开）
/                                       → /hosts
/hosts                                  HostList.vue（主机列表与消息流）
/hosts/:id                              HostDetail.vue（主机详情页壳）
  ├ /hosts/:id/overview                 Overview.vue（4-宫格资源监控）
  ├ /hosts/:id/sysinfo                  Sysinfo.vue（系统状态与进程清单）
  ├ /hosts/:id/terminal                 Terminal.vue（在线终端与远程协助）
  ├ /hosts/:id/files                    Files.vue（文件管理）
  ├ /hosts/:id/docker                   (P2) Docker.vue
  ├ /hosts/:id/apps                     (P2) Apps.vue
  ├ /hosts/:id/vulnerabilities          (P3) Vulnerabilities.vue
  └ /hosts/:id/cloud                    (P2) CloudAssets.vue
/exec                                   ExecPush.vue（批量推送命令）
/sessions                               SessionList.vue（会话历史与回放）
/alerts                                 AlertList.vue（告警列表）
/alerts/rules                           AlertRules.vue（告警规则）
/groups                                 GroupManage.vue（分组与权限）
/users                                  UserManage.vue（用户管理）
/audit                                  AuditList.vue（操作审计）
/ai                                     AiChat.vue（AI 智能助手）
/settings                               Settings.vue（全局与 AI 设置）
/system/health                          Health.vue（系统健康与离线备份）
```

### 3.2 导航守卫与权限模型 (与 Agent.md 3.13/3.15 对齐)

- **路由拦截**：`router.beforeEach` 校验 JWT。如 JWT 缺失跳转 `/login`；如无主机访问权限（按 `user_groups` 组别 Scope 过滤）跳转 `403` 页面。
- **角色限制 (`meta.minRole`)**：
  - `admin`：可访问 `/users`、`/system/health`、`/settings` 中的 AI Provider Key 配置与系统备份。
  - `operator`：可执行主机控制、终端操作、文件写操作、批量推送命令。
  - `viewer`：仅读视角，禁用写操作按钮（UI 呈 disabled 或隐藏）。
- **Workspace 多页签联动**：任何路由切换均自动调用 `workspaceStore.openTab()`，在顶部 Header 多页签中维持激活 Tab。

---

## 4. 界面布局架构 (基于截图识别与 Agent.md 交互对齐)

系统支持**暗色模式 (Dark Theme)** 与 **亮色模式 (Light Theme)** 的双原生主题。

### 4.1 全局界面 Wireframe 架构

```
┌────────────────────────────────────────────────────────────────────────────────────────┐
│ 顶部导航栏 (Top Header Bar)                                                              │
│ [Logo] 牧云主机管理助手 │ [Tab 1: 牧云主机助手] [Tab 2: 系统设置] [Tab 3: game-pc ✖] ...   │ (版本进阶 · 讨论区 · 帮助 · ⚙ 设置 · ☀/🌙 主题 · 用户)  │
├────────────────────────────────────────────────────────────────────────────────────────┤
│ 页面主体 (Main Viewport)                                                                │
│                                                                                        │
│ 模式 A: 【主机列表页布局 - 参截图 deb6161f36388d3826c0127670de472a.png】                 │
│ ┌──────────────────────┬─────────────────────────────────────────────────────────────┐ │
│ │ 左侧双栏 Panel        │ 右侧主工作区 (Host Table Area)                              │ │
│ │ ├─ 分组树 Workspace  │ ┌─────────────────────────────────────────────────────────┐ │ │
│ │ │  Monster的个人空间 │ │ 筛选栏: 快速搜索🔍 | 共 15 台主机 | [绑定主机] [推送命令] [↻] │ │ │
│ │ ├─ 实时消息列表 99+  │ ├─────────────────────────────────────────────────────────┤ │ │
│ │ │  • 主机上线 game-pc│ │ 主机表格 Host Table                                       │ │ │
│ │ │  • 主机离线 nas    │ │ [主机/OS]  [状态/开机时长]  [CPU/内存配置]  [公网/内网 IP]  │ │ │
│ │ └────────────────────┴─────────────────────────────────────────────────────────┘ │ │
│                                                                                        │
│ 模式 B: 【主机详情页布局 - 参截图 07e3f66e/530dffdb/7eedd061 等】                       │
│ ┌────────────────────────────────────────────────────────────────────────────────────┐ │
│ │ 顶部固定主机状态横幅 (Host Header Banner)                                             │ │
│ │ [🖥️ game-pc | Ubuntu 20.04]  CPU: 16核 | 内存: 15.6GB  内网: 172.20.69.191 | 外网...  │ │
│ │ 状态: [已开机 10天22小时]                                                  电源控制 [⏻]│ │
│ ├───────────┬────────────────────────────────────────────────────────────────────────┤ │
│ │ 左侧二级  │ 右侧内容工作区 (Sub-Module Content Pane)                                │ │
│ │ 图标导航  │                                                                        │ │
│ │ 📊 监控   │ 1. Overview: 4-宫格实时/历史图表 (CPU/内存/网络/磁盘)                   │ │
│ │ 📋 进程   │ 2. Sysinfo: 进程清单/网络端口/系统账号/登录历史                        │ │
│ │ 🚢 容器   │ 3. Docker (P2): 容器列表/镜像管理                                      │ │
│ │ 📁 文件   │ 4. Files: 面包屑路径 / 导航工具栏 / 虚拟文件表                          │ │
│ │ 🛡️ 漏洞   │ 5. Terminal: 多 Shell Tab + [远程协助] 授权弹窗                        │ │
│ │ 🔲 云资产 │ 6. Vulnerabilities (P3) / Cloud (P2): 风险指标卡 / [添加云账号] 模态框 │ │
│ └───────────┴────────────────────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## 5. 前端状态管理设计 (Pinia Stores)

### 5.1 `authStore`
- `token` / `refreshToken`：存储 JWT。
- `user`：`{ id, username, role, email }`。
- `groupScopes`：当前用户有权访问的 `group_id` 集合。
- actions: `login()`, `logout()`, `can(action, scope)`。

### 5.2 `workspaceStore`
- `activeTabKey`：当前激活页签的 Key (如 `/hosts/game-pc`)。
- `tabs`：`Array<{ key: string, title: string, path: string, icon?: string, closable: boolean }>`。
- actions: `openTab()`, `closeTab()`, `switchTab()`。

### 5.3 `hostsStore` (与 Agent.md 3.1 & B.2 实时心跳对齐)
- `list`：主机对象列表（包含 `agent_id`, `hostname`, `os`, `arch`, `distro`, `status`, `last_seen_at`, `uptime_str`, `internal_ip`, `external_ip`, `location`, `tags`）。
- **实时同步**：订阅 WebSocket `host_status` 事件，收到 Agent 上/下线或心跳变更时增量更新指定行，无须重刷列表。

### 5.4 `messagesStore` (参截图 `deb6161f36388d3826c0127670de472a.png`)
- `unreadCount`：未读消息计数 (如 `99+`)。
- `feed`：实时接收的主机上线、离线、监控告警日志列表。

### 5.5 `sessionsStore` (与 Agent.md 3.5 & B.5 终端与分享对齐)
- `tabs`：`Array<{ sid, hostId, title, shell, status, termRef, wsRef }>`。
- `remoteAssist`：`{ active: boolean, code: string, expireMinutes: number, mode: 'view' | 'control', subscribersCount: number }`。

### 5.6 `settingsStore`
- `theme`：`'dark'` | `'light'` | `'auto'`。
- `termPrefs`：`{ theme, defaultShell, fontFamily, fontSize }` (拉取并同步 `/me/term-prefs`)。
- `aiConfig`：`{ enabled, defaultProvider, fallbacks, maskOnExternal }`。

---

## 6. 功能模块与页面详细设计 (完整涵盖 Agent.md 需求)

### 6.1 主机绑定与列表管理 `/hosts` (与 Agent.md 3.1 & B.8.2 对齐)

#### 功能点
- **一键绑定**：点击顶栏「绑定主机」按钮，弹出 Modal，显示控制端生成的安装命令（内含 `enroll_token`）：
  - Linux: `curl -fsSL https://console.example.com/install?token=xxx | bash`
  - Windows: 提供 `.exe` 安装包下载链接与 Token 填入说明。
- **双侧边栏布局 (参截图 `deb6161f36388d3826c0127670de472a.png`)**：
  - **左侧组别树**：显示 Workspace 分组结构及主机计数 (`Monster的个人空间 15`)。
  - **左侧消息列表 (`消息列表 99+`)**：实况推送日志（如 `• 主机上线 - game-pc (115.205...) 2024-10-21 19:53`）。
- **右侧主机表格**：
  - 展示 `主机` (OS 图标 + 主机名 + 系统发行版如 `Ubuntu 20.04.4 LTS` / `CentOS 7` / `Windows 11`)。
  - `状态` (在线绿 Pill `已开机 10天22小时` / 离线红 Pill `当前已离线`)。
  - `配置` (硬件规格 `CPU 16核(x64)`, `内存 15.6 GB`)。
  - `IP` (双 IP 独立展示：`内 172.20.69.191`，`外 183.159.65.120 (浙江省-杭州市)`).
  - `操作`：快捷启动终端、管理文件、解绑/卸载 Agent。

---

### 6.2 4-宫格资源监控 `/hosts/:id/overview` (与 Agent.md 3.2 对齐，参截图 `7eedd061075032f9d591c86ecb49b5b5.png`)

#### 功能点
- **模式切换与时间选择**：
  - 模式 Segmented Radio: `实时` | `历史`。
  - 历史模式 DatePicker (`2024/10/31`) + TimePicker (`07:00`) + 快捷跨度 (`+1小时`, `+1天`)。
- **4-宫格监控网格 (2x2 Grid Layout)**：
  1. **CPU 监控**：显示 CPU 核心数 `16 Core`，绘制 CPU 使用率趋势曲线，叠加告警阈值虚线。
  2. **内存监控**：显示内存总量 `15.6 GB`，绘制柱状/面积图，悬浮 Tooltip 显示精准时间与容量 (`2024-10-31 07:10:00 内存 1.4 GB`)。
  3. **网络吞吐**：提供子切换 `双向` | `上传` | `下载`；双向吞吐柱状/折线图。
  4. **磁盘吞吐**：提供子切换 `双向` | `读取` | `写入`；Tooltip 交互显示读写速率 (`读取 0.0 bps / 写入 629.3 Kbps`)。
- **挂载点容量**：展示各磁盘挂载点 (`/`, `/data`) 的已用与总量进度条。

---

### 6.3 系统状态 `/hosts/:id/sysinfo` (与 Agent.md 3.3 对齐，参截图 `d93bdd9b43a0052f3ea7051f28f2890b.png`)

#### 功能点
- **4 大子 Tab**：`进程清单` | `网络端口` | `系统账号` | `登录历史`。
- **进程清单**：展示 `进程名 / PID`、`用户 / UID`、`启动时间`、`CPU` (按点击降序排序 `CPU ↓`)、`内存`、`命令参数` (带展开/收起按钮)。点击操作列可弹出二次确认框「结束进程」。
- **网络端口**：展示 Listening 端口、Proto (TCP/UDP)、监听地址、关联进程 PID 与名称。
- **登录历史**：显示登录用户名、终端类型 (pts/tty)、源 IP 地址、登录时间与状态 (成功/失败)。

---

### 6.4 Web 终端与远程协助 `/hosts/:id/terminal` (与 Agent.md 3.5 & B.5 对齐，参截图 `530dffdb50aeb688018a507e1ee8ba70.png` & `8cea2aba1db6b31ddbdd36938eb58ec4.png`)

#### 功能点
- **多标签与登录配置**：同一主机支持打开多个终端 Tab (`root@nas:~`, `htop`)；根据登录策略 (全局/单机: 禁止/口令/缺省账号) 自动完成免密/鉴权连接。支持 Windows (PowerShell/CMD)。
- **终端主题与控件**：内置 `Miku`、`Dracula`、`Solarized`、`Monokai` 等终端配色下拉选框。
- **远程协助 (Terminal Share) 模态框 (参截图 `8cea2aba1db6b31ddbdd36938eb58ec4.png`)**：
  - 生成连接口令 (如 `2gw937`) 及随机重置按钮。
  - 链接有效期选择 (`5分钟`, `15分钟`, `1小时`).
  - 权限模式单选：
    - `只允许对方观看你的操作` (ReadOnly viewer)
    - `允许对方完全控制你的终端` (FullControl controller)
  - 拥有者可实时查看在线订阅者，并可随时一键切断授权。
- **AI 智能终端辅助条 (Nl2Command)**：在终端底部集成自然语言输入框，输入意图后调用 `/api/v1/ai/nl2command` 生成 Shell 代码块、中文解释与风险等级。高危命令强制要求确认后才注入终端执行。

---

### 6.5 文件管理 `/hosts/:id/files` (与 Agent.md 3.6 & A.5/B.5 对齐，参截图 `07e3f66e8164cb0cc7a29179417d53cc.png` & `615955927e9516528aff6b4f8429f762.png`)

#### 功能点
- **面包屑路径导航**：顶栏包含 `返回` 按钮、`上级目录` 按钮及可编辑的路径面包屑 (`/ > root >` 或 `/ > tmp >`)。
- **操作工具栏**：`刷新`、`新建目录`、`上传文件`。
- **文件表单**：列出名称 (文件夹/文件图标)、大小 (`34.0 MB`)、用户 (`root`)、属性 (`drwxr-xr-x`, `-rw-r--r--`)、修改时间、操作 (`...` 下拉菜单: 预览/编辑/下载/复制/移动/删除)。
- **Monaco 在线编辑器**：支持文本/代码文件弹窗在线编辑，配合 ETag 乐观锁校验防并发覆盖。
- **分片传输与断点续传**：大文件上传下载走 WebSocket 分片传输 (`256KB~1MB` Chunk)，展示实时进度条，自动基于 SHA256 校验与 `.part` 记录续传。

---

### 6.6 Docker 管理 `(P2)` `/hosts/:id/docker` (与 Agent.md 3.4 对齐，参截图 `7efe160649be740f6818f79b0511c9ca.png`)

#### 功能点
- **容器管理**：子 Tab `容器`。展示 Checkbox、`状态` (Green Pill `运行中 4月9天` / Red Pill `已停止`)、`名称` (容器名 + ID 如 `plex 2a0a02d23c1`)、`镜像` (`linuxserver/plex:...`)、`资源使用` (`CPU 0.2%, 内存 165.7 MB`)、`暴露端口`、`创建时间`。支持启动、停止、重启、查看日志、一键进入容器终端及批量操作。
- **镜像管理**：子 Tab `镜像`。列表展示镜像 ID、Tag、大小、创建时间；支持单个删除、批量清理无用镜像 (Prune)。
- **一键安装 Docker**：如主机未装 Docker，控制端检测后提供「一键安装 Docker」按钮。

---

### 6.7 应用市场 `(P2)` `/hosts/:id/apps` (与 Agent.md 3.7 对齐)

#### 功能点
- **一键部署**：提供 Nginx、Redis、MySQL、雷池 WAF 社区版等应用卡片。点击安装后基于 `params_schema` 动态渲染参数表单，提交后在目标主机以 Docker 容器化拉起。
- **应用管理**：展示已安装应用列表，提供启停、重启、日志查看、卸载及修改配置。

---

### 6.8 批量推送命令 `/exec` (与 Agent.md 3.8 对齐)

#### 功能点
- **主机多选**：左侧/弹出框组件 `HostPicker` 支持按分组树勾选多台目标主机。
- **编辑器与常用命令库**：支持 Bash/sh/CMD/PowerShell 类型切换；支持从「常用命令库」下拉加载已保存的脚本；支持上传/输入多行脚本。
- **结果聚合展示**：点击开始推送，右侧按主机卡片实时渲染 stdout、stderr、exit_code 及耗时 (ms)。
- **AI 辅助生成**：集成自然语言转命令，自动填充至推送命令编辑器草稿。

---

### 6.9 动态组网 / 内网穿透 `(P2)` `/networks` (与 Agent.md 3.9 & A.6 对齐)

#### 功能点
- **虚拟内网管理**：无需暴露端口，复用 Agent 反向隧道通道，在 Web 端勾选主机一键「加入组网」，自动分配虚拟 IP (`100.64.x.x`)。
- **拓扑可视化**：展示组网节点连通状态与端到端延迟。

---

### 6.10 安全扫描与漏洞管理 `(P3)` `/vulnerabilities` (与 Agent.md 3.10 对齐，参截图 `9d3541098660b255a12d2b6af332ef18.png`)

#### 功能点
- **顶栏控制**：标题 `牧云·服务器漏洞管理`、`自动扫描周期: 无` 下拉框、`((·)) 一键扫描` 按钮。
- **4 大统计指标卡**：
  1. `需紧急修复的漏洞` (紧急红)
  2. `今日已处理漏洞风险事件` (安全绿)
  3. `累计已处理漏洞风险事件` (安全绿)
  4. `已管理服务器` (品牌蓝)
- **分类过滤 Tab**：`104 所有漏洞` | `0 有风险` | `无风险` | `POC验证` | `版本匹配` | `其它检测方式`。
- **漏洞列表**：展示 CVE/CT 编号、检测方式、披露时间、扫描时间、风险数及「立即检测」按钮。支持 AI 漏洞报告解读与一键生成修复命令。

---

### 6.11 监控告警与通知 `/alerts` (与 Agent.md 3.11 对齐)

#### 功能点
- **活跃告警列表**：展示主机上线/离线及自定义指标告警，按严重程度 (P0 Critical / P1 High / P2 Info) 色标高亮，提供 Acknowledgement 操作。
- **告警规则 CRUD**：设置告警指标 (CPU/内存/网络/磁盘/主机离线)、操作符 (`>`)、阈值、检测窗口时间及生效主机范围。
- **通知渠道管理**：配置邮件 (SMTP) 与 Webhook (钉钉/飞书/企微/自定义) 通知通道。

---

### 6.12 常用命令库与系统设置 `/settings` (与 Agent.md 3.12 对齐)

#### 功能点
- **通用设置**：进入主机管理时的首选落地页面配置、功能提示语 Toggle、全局暗色/亮色主题。
- **常用命令库**：增删改查常用运维 Shell 脚本。
- **在线终端与文件配置**：默认登录账号、默认文件打开路径。
- **AI 适配层设置 (Admin 专享)**：配置 AI Provider (Ollama/vLLM/DeepSeek/GLM/OpenAI)、BaseURL、API Key、降级链 (Fallbacks)、频率限制与数据脱敏开关 (`mask_on_external`)。

---

### 6.13 分组与权限管理 `/groups` & `/users` (与 Agent.md 3.13 对齐)

#### 功能点
- **分组管理**：树状分组新增、重命名、删除；关联主机集合。
- **双视角授权**：
  - **分组视角**：选择指定分组，勾选关联的主机与授权的用户。
  - **用户视角**：选择指定用户，分配其可访问的组别 Scope 与 RBAC 角色 (`admin`, `operator`, `viewer`)。

---

### 6.14 云资产与云账号绑定 `(P2)` `/cloud` (与 Agent.md 3.14 对齐，参截图 `c00d3ff3239994086878287e86ac900a.png`)

#### 功能点
- **添加云账号模态框 (Add Cloud Account Modal)**：
  - 厂商 Tabs：`阿里云` | `腾讯云` | `AWS` | `Microsoft Azure`。
  - 字段：云账号备注、AccessKey ID、AccessKey Secret (带帮助链接 `如何获取AccessKey?`)。
  - 开关：`自动绑定云账号下的所有主机 (安装牧云主机助手)`。
- **云资产仪表盘**：自动发现云服务器实例并对比 Agent 绑定状态。

---

### 6.15 用户认证与操作审计回放 `/audit` & `/sessions` (与 Agent.md 3.15 对齐)

#### 功能点
- **全量操作审计**：记录控制端登录、终端打开、文件读写、批量命令推送、高危操作及 AI 调用的审计日志，支持按时间、用户、动作筛选与导出。
- **终端会话回放 (Asciinema Playback)**：在 `/sessions` 列表中点击「回放」，弹窗唤起 `asciinema-player`，实时解析后端流式返回的 `.cast` 录像文件，支持倍速播放与控制权 Marker 标注。

---

### 6.16 AI 智能助手体系 (增强层) `/ai` & 全局浮窗 (与 Agent.md 3.16 & A.7 对齐)

#### 功能点
- **后端模型适配与安全**：前端调用 `/api/v1/ai/*`。所有 AI 功能均设可关/可降级逻辑；第三方模型上送前由后端自动脱敏。
- **自然语言转命令 (Nl2Command)**：在终端与推送命令中支持自然语言描述生成命令，返回 JSON `{command, explanation, risk_level, requires_confirm}`。高危命令呈现红色警告框，强制二次确认。
- **智能诊断与日志解读**：在监控告警与系统报错处提供「AI 根因诊断」按钮；支持粘贴/上传日志进行总结分析与排查向导。
- **平台数据自然语言问答 (Tool Calling)**：`/ai/chat` 对话框支持自然语言查询（如“哪台机器内存快满了？”），后端通过 Tool Calling 调取 `list_hosts`, `query_metrics`, `list_alerts` 等 API 返回结构化回答。

---

## 7. 关键组件接口设计 (Vue Components Specs)

### 7.1 `HostHeaderBanner.vue` (主机详情固定顶栏)
- **Props**: `host: HostDTO`
- **Emits**: `'power-action': [action: 'reboot' | 'shutdown' | 'disconnect']`
- **渲染**: 展示主机 Icon、Name、OS Badge、CPU 核心数、内存 GB、内网 IP、外网 IP + 地理归属地、运行天数及电源控制图标按钮。

### 7.2 `MetricGrid.vue` (4-宫格资源监控网格)
- **Props**: `hostId: string`, `mode: 'live' | 'history'`, `timeRange: [Date, Date]`
- **内部封装**: 包含 4 个 ECharts 实例 (CPU, 内存, 网络吞吐, 磁盘吞吐)。支持网络与磁盘的 `双向/上传/下载/读取/写入` 按钮切换及悬浮 Tooltip 渲染。

### 7.3 `TerminalPane.vue` (xterm.js 终端面板)
- **Props**: `sid: string`, `hostId: string`, `theme: string`
- **Expose**: `focus()`, `sendCommand(cmd: string)`
- **内部机制**: 绑定 `@xterm/xterm` 与 WebGL/Fit/Search Addons；WebSocket 链接 `/api/v1/ws/terminal/:sid`；集成底部 AI 命令行 (`Nl2Command.vue`)。

### 7.4 `RemoteAssistModal.vue` (远程协助授权弹窗)
- **Props**: `visible: boolean`, `sid: string`
- **Form**: 随机连接口令 (如 `2gw937`)、有效期下拉 (`5m`, `15m`, `1h`)、权限模式单选 (`view` | `control`)。点击生成链接并自动复制。

### 7.5 `FileBreadcrumb.vue` (文件路径导航栏)
- **Props**: `currentPath: string`
- **Emits**: `'navigate': [path: string]`, `'back'`, `'up'`, `'refresh'`, `'mkdir'`, `'upload'`
- **渲染**: 返回按钮、上级目录按钮、可点击路径节点及右侧工具按钮。

---

## 8. 构建、部署与代码质量规范 (与 Agent.md B.6 & 7 对齐)

### 8.1 静态构建与 Embed 打包
1. 前端在 `web/` 执行 `npm run build` 生成生产产物于 `web/dist/`。
2. 控制端 Go 源码在 `server/web/embed.go` 中通过 `//go:embed web/dist/*` 嵌入二进制。
3. 路由使用 History 模式，控制端兜底匹配未命中静态资源的请求返回 `index.html`。

### 8.2 代码质量与编译零错误防线 (与 Agent.md 第 7 节对齐)
- **严格类型校验**：Vue Template 与 TS 脚本严格无类型错误，避免使用废弃 Emoji 表情或断句字符。
- **构建验证**：任何前端改动须通过 `vue-tsc --noEmit` 与 `vite build` 验证，确保编译零 Error / 零 Warning。

---

## 9. 前端页面布局与组件开发规范

### 9.1 组件选用准则：优先使用 Naive UI 原生组件，非必要不封装
- **核心原则**：所有视图与交互设计中，**一律优先使用 Naive UI 原生组件库**（`NCard`, `NDataTable`, `NTabs`, `NTabPane`, `NButton`, `NSpace`, `NTag`, `NModal`, `NAlert`, `NInput`, `NSelect`, `NRadioGroup` 等），**非必要绝不二次封装**或手写自定义 `div` 模拟组件结构。
- **杜绝低效套壳**：严禁无意义地对 Naive UI 原生控件包一层壳作为私有组件；业务组件仅在跨多页面复用高度特化业务逻辑时方可封装（如终端 `TerminalPane`、资源曲线 `MetricsPane` 等）。
- **统一主题感知**：原生组件深度绑定 Naive UI 的运行时暗色/亮色主题与 CSS 变量（如 `--bg-card`, `--border-color`, `--text-primary` 等），杜绝因私自定义类脱离主题变量而导致的色彩断层或样式漂移。

### 9.2 页面标题规范：外层单文字标题体系
- **标准语法**：凡具备独立页面标题的视图，统一在主视口外层顶栏左侧使用单文字 `<h2 class="page-title">标题名</h2>`，右侧配合 `NSpace` 承载页面级全局操作按钮组（如刷新、新增等）。
- **四项杜绝规范**：
  1. **杜绝中英文混排**：禁止出现 `(Tailscale / Headscale)` 等冗长括号英文，统一使用简炼中文（如 `异地组网`）。
  2. **杜绝图标混排**：标题文字前禁止附加各类修饰性 `NIcon`，保持极简统一的纯文本层级。
  3. **杜绝标题下方副标题说明**：取消标题下方的 `page-desc` 说明段落，界面交互以直观功能为主，必要说明使用 `NAlert` 或字段 Tooltip 呈现。
  4. **杜绝文字塞在卡片内部**：严禁将整页主标题写在 `<NCard title="推送命令">` 或内层卡片中；主标题必须外提至视口顶部，卡片内仅保留表单/表格内容。

### 9.3 多页签与表格布局规范：Tab 与表格卡片分层架构
- **标准参考**：以「消息与告警中心（`AlertList.vue`）」为标准布局规范模板（快照备份 `SnapshotList.vue`、证书中心 `CertList.vue` 均已完成对齐）。
- **分层拓扑架构**：
  1. **顶层容器**：`.page-flex-column` 纵向 flex 撑满视口，`overflow: hidden`。
  2. **页面顶栏**：`.page-header`（左侧单文字标题，右侧操作按钮组）。
  3. **全局 Tab 栏**：使用独立的 `.tabs-container` 包裹 `<NTabs type="line">`，**严禁将 Tab 塞进表格内部或放在卡片容器内**。
  4. **页签内容区**：各个 `NTabPane` 内部由 `.tab-pane-content` 承载，若有指引则上方放 `NAlert`，下方紧跟 `<NCard :bordered="false" class="table-flex-fill">`。
  5. **表格自适应撑满**：表格统一设置 `flex-height` 和 `:bordered="false"`，结合 `.table-flex-fill` 与全局 flex 穿透样式，让表头与分页条牢牢钉在视口两端，仅数据区纵向滚动。

### 9.4 表格容器视觉规范：统一背景色与消除多余外边框
- **背景色绝对统一**：表格与主容器卡片一律统一使用系统标准卡片背景变量 `var(--bg-card)`（暗色 `#131b2e`，亮色 `#ffffff`），严禁使用任意无主题感知的 `rgba(128, 128, 128, 0.06)` 等伪背景。
- **消除多余外边框**：表格卡片容器严禁添加外层实线边框（如禁止出现 `border: 1px solid var(--border-color);` 与外层生硬的 `padding`），消除突兀内缩矩形框，保持平整、沉浸的云控制台视觉风格。
