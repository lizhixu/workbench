# 前端设计文档（Web Console - 牧云主机管理助手 / 云堡垒机）

> **规范基准声明**：本设计文档全量整合并严格对齐后端与需求主文档 `AGENTS.md`。系统的核心目标为「自部署、自己掌控、永久可用、不依赖第三方 SaaS」。所有功能范围、数据契约、接口命名、权限模型与 AI 增强逻辑均**以 `AGENTS.md` 为唯一权威标准**。前端完整承载双层多页签、双侧边栏、顶部状态横幅与 4-宫格监控等云原生 UI 布局体系。

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
    │   ├── docker.ts         # /hosts/:id/docker/*, /docker/install-script
    │   ├── apps.ts           # /apps CRUD/部署/回滚/历史, /apps/catalog, /apps/:id/proxy
    │   ├── security.ts       # /security/scans, /vulnerabilities
    │   ├── cloud.ts          # /cloud/accounts, /cloud/assets
    │   ├── network.ts        # /networks/join, /networks/topology
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
    │   ├── docker/           # ContainerTable, ImageTable, DockerInstallModal
    │   ├── security/         # VulnerabilityStatCard, VulnerabilityTable, ScanTrigger
    │   ├── cloud/            # CloudAccountModal, CloudAssetDashboard
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
    │   │   ├── Docker.vue              # Docker 容器与镜像管理（观测运维；部署走应用中心）
    │   │   ├── Vulnerabilities.vue     # 服务器漏洞与安全基线
    │   │   └── CloudAssets.vue         # 云资产与云账号绑定
    │   ├── apps/
    │   │   ├── AppList.vue            # 应用中心列表
    │   │   ├── AppCreate.vue          # 统一创建入口（模板/单镜像/Compose/Git）
    │   │   └── AppDetail.vue          # 应用详情（部署历史/域名反代/拓扑）
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
  ├ /hosts/:id/docker                   Docker.vue（容器/镜像观测运维）
  ├ /hosts/:id/vulnerabilities          Vulnerabilities.vue
  └ /hosts/:id/cloud                    CloudAssets.vue
/apps                                   AppList.vue（应用中心列表）
  ├ /apps/create                        AppCreate.vue（统一创建：模板/单镜像/Compose/Git）
  └ /apps/:id                           AppDetail.vue（应用详情与域名反代）
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

## 4. 界面布局架构（与 Agent.md 交互对齐）

系统支持**暗色模式 (Dark Theme)** 与 **亮色模式 (Light Theme)** 的双原生主题。

### 4.1 全局界面 Wireframe 架构

```
┌────────────────────────────────────────────────────────────────────────────────────────┐
│ 顶部导航栏 (Top Header Bar)                                                              │
│ [Logo] 牧云主机管理助手 │ [Tab 1: 牧云主机助手] [Tab 2: 系统设置] [Tab 3: game-pc ✖] ...   │ (版本进阶 · 讨论区 · 帮助 · ⚙ 设置 · ☀/🌙 主题 · 用户)  │
├────────────────────────────────────────────────────────────────────────────────────────┤
│ 页面主体 (Main Viewport)                                                                │
│                                                                                        │
│ 模式 A: 【主机列表页布局】                                                               │
│ ┌──────────────────────┬─────────────────────────────────────────────────────────────┐ │
│ │ 左侧双栏 Panel        │ 右侧主工作区 (Host Table Area)                              │ │
│ │ ├─ 分组树 Workspace  │ ┌─────────────────────────────────────────────────────────┐ │ │
│ │ │  Monster的个人空间 │ │ 筛选栏: 快速搜索🔍 | 共 15 台主机 | [绑定主机] [推送命令] [↻] │ │ │
│ │ ├─ 实时消息列表 99+  │ ├─────────────────────────────────────────────────────────┤ │ │
│ │ │  • 主机上线 game-pc│ │ 主机表格 Host Table                                       │ │ │
│ │ │  • 主机离线 nas    │ │ [主机/OS]  [状态/开机时长]  [CPU/内存配置]  [公网/内网 IP]  │ │ │
│ │ └────────────────────┴─────────────────────────────────────────────────────────┘ │ │
│                                                                                        │
│ 模式 B: 【主机详情页布局】                                                              │
│ ┌────────────────────────────────────────────────────────────────────────────────────┐ │
│ │ 顶部固定主机状态横幅 (Host Header Banner)                                             │ │
│ │ [🖥️ game-pc | Ubuntu 20.04]  CPU: 16核 | 内存: 15.6GB  内网: 172.20.69.191 | 外网...  │ │
│ │ 状态: [已开机 10天22小时]                                                  电源控制 [⏻]│ │
│ ├───────────┬────────────────────────────────────────────────────────────────────────┤ │
│ │ 左侧二级  │ 右侧内容工作区 (Sub-Module Content Pane)                                │ │
│ │ 图标导航  │                                                                        │ │
│ │ 📊 监控   │ 1. Overview: 4-宫格实时/历史图表 (CPU/内存/网络/磁盘)                   │ │
│ │ 📋 进程   │ 2. Sysinfo: 进程清单/网络端口/系统账号/登录历史                        │ │
│ │ 🚢 容器   │ 3. Docker: 容器列表/镜像管理                                           │ │
│ │ 📁 文件   │ 4. Files: 面包屑路径 / 导航工具栏 / 虚拟文件表                          │ │
│ │ 🛡️ 漏洞   │ 5. Terminal: 多 Shell Tab + [远程协助] 授权弹窗                        │ │
│ │ 🔲 云资产 │ 6. Vulnerabilities / Cloud: 风险指标卡 / [添加云账号] 模态框           │ │
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

### 5.4 `messagesStore`
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
- **双侧边栏布局**：
  - **左侧组别树**：显示 Workspace 分组结构及主机计数 (`Monster的个人空间 15`)。
  - **左侧消息列表 (`消息列表 99+`)**：实况推送日志（如 `• 主机上线 - game-pc (115.205...) 2024-10-21 19:53`）。
- **右侧主机表格**：
  - 展示 `主机` (OS 图标 + 主机名 + 系统发行版如 `Ubuntu 20.04.4 LTS` / `CentOS 7` / `Windows 11`)。
  - `状态` (在线绿 Pill `已开机 10天22小时` / 离线红 Pill `当前已离线`)。
  - `配置` (硬件规格 `CPU 16核(x64)`, `内存 15.6 GB`)。
  - `IP` (双 IP 独立展示：`内 172.20.69.191`，`外 183.159.65.120 (浙江省-杭州市)`).
  - `操作`：快捷启动终端、管理文件、解绑/卸载 Agent。

---

### 6.2 4-宫格资源监控 `/hosts/:id/overview`（与 Agent.md 3.2 对齐）

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

### 6.3 系统状态 `/hosts/:id/sysinfo`（与 Agent.md 3.3 对齐）

#### 功能点
- **4 大子 Tab**：`进程清单` | `网络端口` | `系统账号` | `登录历史`。
- **进程清单**：展示 `进程名 / PID`、`用户 / UID`、`启动时间`、`CPU` (按点击降序排序 `CPU ↓`)、`内存`、`命令参数` (带展开/收起按钮)。点击操作列可弹出二次确认框「结束进程」。
- **网络端口**：展示 Listening 端口、Proto (TCP/UDP)、监听地址、关联进程 PID 与名称。
- **登录历史**：显示登录用户名、终端类型 (pts/tty)、源 IP 地址、登录时间与状态 (成功/失败)。

---

### 6.4 Web 终端与远程协助 `/hosts/:id/terminal`（与 Agent.md 3.5 & B.5 对齐）

#### 功能点
- **多标签与登录配置**：同一主机支持打开多个终端 Tab (`root@nas:~`, `htop`)；根据登录策略 (全局/单机: 禁止/口令/缺省账号) 自动完成免密/鉴权连接。支持 Windows (PowerShell/CMD)。
- **终端主题与控件**：内置 `Miku`、`Dracula`、`Solarized`、`Monokai` 等终端配色下拉选框。
- **远程协助（Terminal Share）模态框**：
  - 生成连接口令 (如 `2gw937`) 及随机重置按钮。
  - 链接有效期选择 (`5分钟`, `15分钟`, `1小时`).
  - 权限模式单选：
    - `只允许对方观看你的操作` (ReadOnly viewer)
    - `允许对方完全控制你的终端` (FullControl controller)
  - 拥有者可实时查看在线订阅者，并可随时一键切断授权。
- **AI 智能终端辅助条 (Nl2Command)**：在终端底部集成自然语言输入框，输入意图后调用 `/api/v1/ai/nl2command` 生成 Shell 代码块、中文解释与风险等级。高危命令强制要求确认后才注入终端执行。

---

### 6.5 文件管理 `/hosts/:id/files`（与 Agent.md 3.6 & A.5/B.5 对齐）

#### 功能点
- **面包屑路径导航**：顶栏包含 `返回` 按钮、`上级目录` 按钮及可编辑的路径面包屑 (`/ > root >` 或 `/ > tmp >`)。
- **操作工具栏**：`刷新`、`新建目录`、`上传文件`。
- **文件表单**：列出名称 (文件夹/文件图标)、大小 (`34.0 MB`)、用户 (`root`)、属性 (`drwxr-xr-x`, `-rw-r--r--`)、修改时间、操作 (`...` 下拉菜单: 预览/编辑/下载/复制/移动/删除)。
- **Monaco 在线编辑器**：支持文本/代码文件弹窗在线编辑，配合 ETag 乐观锁校验防并发覆盖。
- **分片传输与断点续传**：大文件上传下载走 WebSocket 分片传输 (`256KB~1MB` Chunk)，展示实时进度条，自动基于 SHA256 校验与 `.part` 记录续传。

---

### 6.6 Docker 管理 `/hosts/:id/docker`（与 Agent.md 3.4 对齐）

> Docker 面板定位为运行时观测与运维，不再承担应用部署入口——所有新建服务统一走 6.7 应用中心（`/apps/create`）。

#### 功能点
- **容器管理**：子 Tab `容器`。展示 Checkbox、`状态` (Green Pill `运行中 4月9天` / Red Pill `已停止`)、`名称` (容器名 + ID 如 `plex 2a0a02d23c1`)、`镜像` (`linuxserver/plex:...`)、`资源使用` (`CPU 0.2%, 内存 165.7 MB`)、`暴露端口`、`创建时间`。支持启动、停止、重启、查看日志、一键进入容器终端及批量操作。
- **纳管状态列**：识别容器 `watchman.app=<appID>` 标签——应用中心部署的容器显示「应用中心纳管」并可跳转应用详情；标签对应应用已删除显示警告；外部/临时容器如实标注。
- **部署新应用**：容器页签工具栏的「部署新应用」按钮跳转 `/apps/create?host_id=<当前主机>`（预选目标主机），由应用中心完成创建部署。
- **镜像管理**：子 Tab `镜像`。列表展示镜像 ID、Tag、大小、创建时间；支持单个删除、批量清理无用镜像 (Prune)、拉取镜像（输入 `repo:tag`）；工具栏提供「镜像加速」入口，支持查看与修改 `daemon.json` 中的 `registry-mirrors`，内置常用国内加速源快捷添加，保存后平滑重载 Docker 守护进程。
- **一键安装 Docker**：如主机未装 Docker，控制端检测后提供「一键安装 Docker」按钮。

---

### 6.7 应用中心 `/apps` (与 Agent.md 3.7 对齐)

> 原主机级「应用市场」Tab（`/hosts/:id/apps`）已废弃删除，全部部署能力收敛到全局应用中心。

#### 功能点
- **统一创建入口** (`/apps/create`)：四种部署来源卡片——应用模板（Nginx/Redis/MySQL/雷池 WAF 等，`GET /apps/catalog` 声明式目录，动态渲染参数表单）、单镜像（输入 `repo:tag`）、Compose 编排（在线 YAML）、Git 仓库（GitHub 授权选择或手动 URL，支持 Webhook 自动部署）。
- **端口绑定范围**：每条端口映射可选「公网开放」（绑定 0.0.0.0）或「仅异地组网」（只绑定主机 Tailscale IP，公网不可达），联动展示所选主机的组网 IP。
- **创建时绑定域名**：填写域名与反代模式（节点本地 / 统一网关），提交依次执行 创建应用 → 自动签发证书（默认 ACME 账户）→ 下发 Nginx 反代并热加载；绑定失败不回滚应用。
- **应用管理**：应用列表/详情支持部署、回滚、启停、查看构建日志、部署历史、AI 排障诊断；健康检查探活失败自动保留旧版本（`-next`/`-prev` 滚动替换）。
- **流量拓扑展示**：详情页「域名与反代」页签可视化 `公网用户 → 网关节点 (Nginx/SSL) → Tailscale 内网 → 应用容器`，网关模式下应用主机无需公网 IP。

---

### 6.8 批量推送命令 `/exec` (与 Agent.md 3.8 对齐)

#### 功能点
- **主机多选**：左侧/弹出框组件 `HostPicker` 支持按分组树勾选多台目标主机。
- **编辑器与常用命令库**：支持 Bash/sh/CMD/PowerShell 类型切换；支持从「常用命令库」下拉加载已保存的脚本；支持上传/输入多行脚本。
- **结果聚合展示**：点击开始推送，右侧按主机卡片实时渲染 stdout、stderr、exit_code 及耗时 (ms)。
- **AI 辅助生成**：集成自然语言转命令，自动填充至推送命令编辑器草稿。

---

### 6.9 动态组网 / 内网穿透 `/networks` (与 Agent.md 3.9 & A.6 对齐)

#### 功能点
- **虚拟内网管理**：无需暴露端口，复用 Agent 反向隧道通道，在 Web 端勾选主机一键「加入组网」，自动分配虚拟 IP (`100.64.x.x`)。
- **拓扑可视化**：展示组网节点连通状态与端到端延迟。

---

### 6.10 安全扫描与漏洞管理 `/vulnerabilities`（与 Agent.md 3.10 对齐）

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
- **活跃告警列表**：展示主机上线/离线及自定义指标告警，按严重程度 (紧急 Critical / 高危 High / 提示 Info) 色标高亮，提供 Acknowledgement 操作。
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

### 6.14 云资产与云账号绑定 `/cloud`（与 Agent.md 3.14 对齐）

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

### 9.2 页面留白与布局间距体系
- **全局外层内边距**：路由页面的外层内边距由 `AppShell` 的 `.app-body` 统一施加，统一绑定 CSS 变量 `--page-padding`（桌面端 `16px` / 窄屏移动端 `10px`）。
- **严禁页面私加外层 padding**：各路由页面根容器组件**自身不得再设置外层 `padding`**，否则各页面留白会出现不一致与双重缩进问题。
- **主卡片内边距**：页面内主卡片/主面板的内边距统一取 `--card-padding`（`16px`），与页面留白保持同一基准档位。

### 9.3 页面标题规范：外层单文字标题体系
- **标准语法**：凡具备独立页面标题的视图，统一在主视口外层顶栏左侧使用单文字 `<h2 class="page-title">标题名</h2>`，右侧配合 `NSpace` 承载页面级全局操作按钮组（如刷新、新增等）。
- **四项杜绝规范**：
  1. **杜绝中英文混排**：禁止出现 `(Tailscale / Headscale)` 等冗长括号英文，统一使用简炼中文（如 `异地组网`）。
  2. **杜绝图标混排**：标题文字前禁止附加各类修饰性 `NIcon`，保持极简统一的纯文本层级。
  3. **杜绝标题下方副标题说明**：取消标题下方的 `page-desc` 说明段落，界面交互以直观功能为主，必要说明使用 `NAlert` 或字段 Tooltip 呈现。
  4. **杜绝文字塞在卡片内部**：严禁将整页主标题写在 `<NCard title="推送命令">` 或内层卡片中；主标题必须外提至视口顶部，卡片内仅保留表单/表格内容。

### 9.4 大数据表格布局规范：整页自适应 + 表头分页固定
凡是承载**不定长数据列表**的表格（审计日志、进程清单、端口清单、系统账号、登录历史、会话记录、文件列表、容器/镜像列表、扫描结果、告警消息、用户、凭据、分组、命令拦截审计、数据卷归档等），一律采用「整页填满视口 + 表头与分页固定 + 仅数据区滚动 + **必须分页**」的布局。严禁让整页跟着表格一起长高把分页条推到首屏之外，严禁不分页一次性渲染全量数据。

#### 1. Flex 三件套布局模型
1. **页面根容器**撑满可用高度并作为纵向 flex 容器（使用全局类 `.page-flex-column`）：
   ```scss
   .page-flex-column {
     display: flex;
     flex-direction: column;
     height: 100%;
     min-height: 0; // 必须，否则子项无法收缩
   }
   ```
2. **表格容器**吃掉剩余高度，必须同时声明 `min-height: 0`（使用全局类 `.table-flex-fill`）。flex 子项默认 `min-height: auto`，不写则永远不会收缩，滚动条会外溢到页面顶层：
   ```scss
   .table-flex-fill {
     flex: 1;
     min-height: 0;
     display: flex;
     flex-direction: column;
     overflow: hidden;

     // 包裹 NCard 时，卡片内容区必须具备同样的 flex 能力
     .n-card-content {
       flex: 1;
       min-height: 0;
       display: flex;
       flex-direction: column;
       overflow: hidden;
       padding: 0 !important;

       &:first-child {
         padding-top: 0 !important;
       }
     }

     .n-data-table {
       flex: 1;
       min-height: 0;
     }
   }
   ```
3. **`NDataTable` 一律加 `flex-height`**：让表格按父容器高度自适应计算滚动区，表头随之固定：
   ```vue
   <NDataTable flex-height :columns="columns" :data="rows" :pagination="pagination" />
   ```
   - `flex-height` 与写死的 `:max-height` 是互斥语义，用了前者严禁再写后者。
   - 横向可滚动的宽表继续保留 `:scroll-x`（如 `:scroll-x="1240"`），两者完全兼容。

4. **分页条固定在表格下方**：使用 `flex-shrink: 0` 钉住，不参与滚动。分页条需固定条高与上下对称内边距（`.table-pagination-bar`），让分页组件在条内垂直居中，避免紧贴卡片底边或导致下拉菜单被边缘裁切。

#### 2. 分页选型与页码收敛机制
凡不定长列表**必须分页**，按数据来源与交互场景二选一：
- **服务端分页**（数据持续累积且后端支持分页参数）：
  请求携带 `offset` / `page_size`，响应的 `total` 必须为**未分页的总条数**。前端使用独立的 `NPagination` 嵌入底部分离固定栏 `.table-pagination-bar`。换页、改变每页条数显式重新触发请求。
- **前端分页**（全量数组已在内存，或后端具备固定上限条数）：
  将全量数组交由 `NDataTable` 的 `:pagination`，统一接入 `web/src/composables/useTablePagination.ts`：
  ```ts
  const rowCount = computed(() => rows.value.length)
  const { pagination, resetPage } = useTablePagination({ pageSize: 20, rowCount })
  ```
  `rowCount` 用于**页码收敛**：删除某页末行或筛选条件收紧导致总行数变少时，页码自动被夹回最大有效页，杜绝「有数据却渲染成空白页」的缺陷；刷新、切页签或筛选条件变更时调用 `resetPage()` 回归第一页。

#### 3. 验收口径与表格防错
- **杜绝原生 `<NTable>` 承载不定长数据**：`NTable` 仅为静态 HTML 表格包装，无 `flex-height`、无固定表头与分页。数据表格必须使用 `NDataTable` + `columns` render 函数；单元格样式在 render 中生成时，scoped 样式必须使用 `:deep()` 穿透。
- **`flex-height` 高度生效验收**：`.table-flex-fill` 已配置 `flex: 1; min-height: 0`。若另行手写 flex 容器，必须确保 `NDataTable` 自身获得 `flex: 1`，验收口径为：**表格数据区 `clientHeight` 必须明显大于表头高度**（避免数据区 clientHeight 为 0 导致表格空白）。
- **固定列（fixed: left/right）纯色防透**：横向滚动表格中，固定列（如首列、操作列）背景色必须硬绑定不透明的 `var(--bg-card)`，行悬停时同步切换为不透明的 `tdColorHover`（`#18223a` / `#f8fafc`），杜绝半透明导致底层单元格文字穿透重叠。
- **例外规范**：卡片内部的辅助性小表格（常用命令库、备份列表、执行主机勾选框、告警规则清单等），因数据量天然有限且非页面主体，允许使用固定 `:max-height` 限高且不分页，但**必须在代码注释中显式声明本例外**。

### 9.5 多页签与表格布局规范：Tab 与表格卡片分层架构
- **标准参考**：以「消息与告警中心（`AlertList.vue`）」为标准布局规范模板（快照备份 `SnapshotList.vue`、证书中心 `CertList.vue` 等均按此对齐）。
- **分层拓扑架构**：
  1. **顶层容器**：`.page-flex-column` 纵向 flex 撑满视口，`overflow: hidden`。
  2. **页面顶栏**：`.page-header`（左侧单文字标题，右侧操作按钮组）。
  3. **全局 Tab 栏**：使用独立的 `.tabs-container` 包裹 `<NTabs type="line">`，**严禁将 Tab 塞进表格内部或放在卡片容器内**。
  4. **页签内容区**：各个 `NTabPane` 内部由 `.tab-pane-content` 承载，若有指引则上方放 `NAlert`，下方紧跟 `<NCard :bordered="false" class="table-flex-fill">`。
  5. **表格自适应撑满**：表格统一设置 `flex-height` 和 `:bordered="false"`，结合 `.table-flex-fill` 与全局 flex 穿透样式，让表头与分页条牢牢钉在视口两端，仅数据区纵向滚动。

### 9.6 表格容器视觉规范：统一背景色与消除多余外边框
- **背景色绝对统一**：表格与主容器卡片一律统一使用系统标准卡片背景变量 `var(--bg-card)`（暗色 `#131b2e`，亮色 `#ffffff`），严禁使用任意无主题感知的 `rgba(128, 128, 128, 0.06)` 等伪背景。
- **消除多余外边框**：表格卡片容器严禁添加外层实线边框（如禁止出现 `border: 1px solid var(--border-color);` 与外层生硬的 `padding`），消除突兀内缩矩形框，保持平整、沉浸的云控制台视觉风格。

### 9.7 滚动容器与页签内高度约定
- **单一滚动源原则**：一个可滚动区域只允许存在一个激活的滚动条。页面外层把滚动职责交付给表格时，外层容器必须显式配置 `overflow: hidden`，杜绝内外双层滚动条嵌套出现。
- **长列表滚动优化**：所有滚动容器统一带有 `-webkit-overflow-scrolling: touch` 与 `overscroll-behavior: contain`（`global.scss` 中针对 `.scroll-container`, `.sub-view-pane`, `.host-list-container` 等集中声明），防止滚动链路冒泡到视口外部。
- **页签内滚动容器高度约束**：
  页签内的自定义滚动容器**必须显式声明 `height: 100%` 并搭配 `min-height: 0`**；单独写 `overflow-y: auto` 不成立。`NTabPane` 虽然内部撑满，但子容器若默认为 `height: auto` 会跟随内容无界长高并溢出页签底边，导致外层 `overflow: hidden` 将超出内容静默截断（用户看不见也无法滚动）。验收口径：容器在 `scrollHeight > clientHeight` 时必须能够产生大于 0 的有效 `scrollTop` 滚动位移。

### 9.8 表单控件设计规范：NSelect 空值约定与暗色可见边框
#### 1. NSelect 空值约定
- **绑定 NSelect `v-model:value` 的字段一律用 `null` 作为空值**（声明 `null as string | null`），**严禁使用 `''`（空字符串）**。Naive UI 在遇到未命中选项列表的值时会触发默认 `fallbackOption`，把空字符串合成一个 `label: ''` 的真实选项，使选择器误认为「已选中空值」，导致 Placeholder 占位文本永不渲染（幽灵选中态）。只有 `null` 才会被识别为空，占位符方可正常显示。
- **唯一例外**：选项列表中**真实存在 `value: ''` 条目**时允许使用 `''`（如应用中心「高级：手动指定证书」中的「自动匹配（推荐）」选项）。
- **联动态**：重置表单与清空字段统一赋 `null`；提交接口载荷时通过 `value || undefined` 过滤；`Map.get(id)` 等强类型取值前先校验 truthy。

#### 2. 暗色模式表单控件可见边框
- Naive UI 暗色主题默认将 `Input` 与 `InternalSelection` 的常态边框设为全透明（`1px solid #0000`），仅在 hover/focus 时才呈现边框，导致暗色背景下输入框与下拉列表缺乏视界边界。
- 统一在 `web/src/App.vue` 的 `themeOverrides` 中全局补回可见边框：
  ```ts
  const darkControlBorder = '1px solid rgba(255, 255, 255, 0.16)'
  // Input: { border: darkControlBorder, borderDisabled: '1px solid rgba(255, 255, 255, 0.08)' }
  // InternalSelection: { border: darkControlBorder }
  ```
  该配置经 `NConfigProvider` 全局生效（含 InputNumber、DatePicker 以及 Modal 内控件），禁止在各个页面私自手写零散的输入框边框样式。

### 9.9 紧凑表单与间距补偿规范
当 `NForm` 声明 `:show-feedback="false"` 启用紧凑模式时，Naive UI 会彻底移除 `.n-form-item` 底部的 `margin-bottom`（实测为 `0px`），导致多个相邻表单字段直接粘连。
- **显式间距补偿**：在紧凑表单外层定义专有类名补齐标准间距：
  ```scss
  .compact-form {
    :deep(.n-form-item) { margin-bottom: 14px; }
    :deep(.n-form-item:last-child) { margin-top: 2px; margin-bottom: 0; } // 末尾提交按钮
    :deep(.n-form-item-label) { padding-bottom: 8px; }                    // label 与控件间距
  }
  ```
- **折叠面板内容补偿**：折叠面板内部必须显式给 `.n-collapse-item__content-inner` 补偿 `padding: 16px 0 8px`，防止表单顶到面板标题或贴近底边。
- **度量基准**：Label 到控件间距 `8px`、控件之间间距 `14px`、提交按钮上方间距 `14px`。严禁在模板中写大量内联 `style="margin-top: ..."`。

### 9.10 顶部 Workspace 多页签拖拽排序交互规范
顶部多工作区页签（`AppShell.vue` 中的 `workspace-tab-item`）承载多任务多视图上下文，支持鼠标拖拽水平调整页签顺序：
1. **原生零依赖**：利用 HTML5 Drag and Drop 原生机制（`draggable="true"`, `@dragstart`, `@dragover`, `@drop`, `@dragend`），无需额外三方库。
2. **动态位移感知**：计算鼠标距离当前目标 item 左边缘的距离，按 `< width/2` 动态判断放置到当前 item 的左侧 (`left`) 或右侧 (`right`)。
3. **清晰指示线**：通过 `.drop-left::before` 与 `.drop-right::after` 伪元素在间隙处绘制品牌色垂直指示线，源节点半透明展示；关闭按钮加 `@mousedown.stop` 防止误触拖拽。
4. **Store 顺序持久响应**：在 Pinia `workspace` store 中执行 `moveTab`，通过 `splice` 自动修正索引偏移，确保顶部工作区与组件激活状态平滑响应。

### 9.11 功能提示语的可开关约定
- **样式标记**：各页面中用于功能指引、机制说明的说明性段落（如 `<p class="tip-hint">`）统一附加 `.tip-hint` 类名。
- **全局用户开关控制**：受用户设置 `appearance.show_tips` 控制。`App.vue` 监听设置并将 `.hide-tips` 挂在 `documentElement` 上；全局样式由以下规则自动隐藏：
  ```scss
  .hide-tips .tip-hint {
    display: none !important;
  }
  ```
- **业务安全红线**：操作警告、系统报错类 `NAlert` 以及核心业务数据展示文字**严禁添加 `.tip-hint`**；开关仅控制辅助性指引说明，不得隐藏任何业务关键信息。

### 9.12 外部 Markdown 内容渲染与排版规范
凡是展示外部输入的 Markdown（GitHub Release 更新日志、AI 部署助手分析、对话助手排版等），一律走 `web/src/utils/markdown.ts` 的 `renderMarkdown()`，**严禁直接使用 `<pre>{{ text }}</pre>` 裸文本输出**。
1. **零第三方依赖**：纯自研轻量解析，杜绝打包体积膨胀。
2. **语法覆盖**：支持 `#`~`######` 标题（映射为 h4~h6 保证容器内和谐）、有序/无序列表与缩进排版、`**加粗**`、`*斜体*`、`~~删除线~~`、行内 `code`、带语言标注的 ``` 代码块（提取语言至 `data-lang`）、安全链接、引用块 `>`、水平分割线 `---`、以及带有横向滚动外壳 `<div class="md-table-wrap">` 的 GFM 响应式表格。
3. **安全注入防御**：全局强制先转义 HTML 实体后再做 Markdown 标记解析；链接仅放行 `http://`, `https://`, `mailto:`, 锚点与站内绝对路径，其余降级为 `#`，确保直接配合 `v-html` 渲染时的绝对安全。
4. **样式穿透**：容器必须配置专有类（如 `.markdown-body` 或 `.changelog-body`），通过 scoped CSS `:deep()` 下发段落、标题、表格、代码块以及 `white-space: normal` 排版规则。
5. **AI 助手与部署助手集成**：
   - AI 对话助手与部署草稿卡片（`AppAiAssistant.vue`）统一接入 `renderMarkdown()`；
   - 后端 `extractJSONPayload` 自动清洗 `<think>` 与 `<thought>` 思考标签，并经 `repairJSON` 修复未转义换行与尾随逗号，防止原始数据泄露或破坏气泡流式布局。

### 9.13 AI 运维助手输入卡规范（CherryStudio 风格一体化 Composer）
AI 助手页（`web/src/views/ai/AiChat.vue`）的输入区域采用现代化的一体化圆角输入卡（`.chat-composer`）：
1. **一体化外观**：容器 `14px` 圆角、`var(--bg-card)` 纯净底色、`var(--border-color)` 细微描边；`:focus-within` 聚焦时容器整体点亮品牌色边框并叠合 3px 柔和光晕；内部嵌入无边框透明 `NInput`（`:bordered="false"`）。
2. **底栏操作区**：`.composer-footer` 左侧提供等宽圆角键帽提示（`.hint-key`：`Enter 发送 · Shift+Enter 换行`，窄屏自动隐藏）；右侧为 `36px` 圆形强调色图标发送按钮（禁用时优雅降级为弱化背景），严禁回退为外挂长方形传统按钮。
3. **顶部高度拖拽把手**：输入卡顶部集成 `.chat-resize-handle`，鼠标向上拖拽增加高度、向下回落；双击把手立即复位至默认基准高度 `64px`。
4. **弹性上下限与防溢出保护**：下限 `MIN_INPUT_H = 64px`；上限按视口高度动态扣减，确保上层对话消息区保留至少 `180px`（`INPUT_MIN_LIST_H`）；集成 `ResizeObserver` 随窗口缩放自适应收敛高度。
5. **Pointer Capture 完整指针捕获**：使用 `setPointerCapture` 将指针事件锁定在把手元素，即便光标移出浏览器外松开也能安全触发 `pointerup` 并释放事件，避免拖拽粘滞。
6. **窄屏移动端自适应**：屏幕宽度 `≤768px` 时自动隐藏拖拽把手，输入框回落至固定 `64px`，发送按钮自适应增大至 `40px` 确保触控体验。

### 9.14 登录门户工程美学与交互规范（/login）
登录页（`web/src/views/auth/Login.vue`）作为整个 Web 控制台的入口门户，定位于工业级、专业的云原生基础设施与堡垒机控制台，**坚决摒弃廉价泛滥的「AI 味」浮夸元素**（如大面积高饱和粉紫霓虹流体弥散光球、旋转光斑、科幻粒子飞舞或塑料毛玻璃眩光）：
- **纯净冷调深空底色**：采用纯净暗色深底（`#090d16`），卡片背后不设任何发散光圈、色晕或弥散光球（拒绝模糊朦胧的 AI 渲染套路），保持底板深邃干净。
- **动态探照灯与精密工程网格（鼠标跟随交互）**：
  - **纯粹双层网格结构**：底层由常驻的 36px 间距超细静态网格（`.bg-grid-base`，微弱暗色，带全局椭圆遮罩）铺底；上层叠加高亮网格（`.bg-grid-spotlight`），利用 `mask-image: radial-gradient(circle 340px at var(--mouse-x) var(--mouse-y), ...)` 实现光标扫过区域时网格线条细腻浮现的精密工业质感（仅照亮网格线条，不叠加大面积模糊色斑光圈）；
  - **RAF 物理阻尼插值**：通过 `pointermove` 捕获光标坐标，并利用 `requestAnimationFrame` 配合阻尼系数（`ease = 0.1`）平滑过渡 CSS 变量 `--mouse-x` 与 `--mouse-y`，光标静止或移出窗口时平滑回落至视口中心，避免机械生硬突变；
  - **设备感知与优雅降级**：通过 `window.matchMedia('(pointer: fine)')` 侦测指针精度，仅在 PC 鼠标等高精度设备上激活实时光标跟随循环，移动端与触控设备保持静态居中展示，节约算力并避免触控跳跃。
- **工程对齐准星点缀**：
  - 核心卡片四围对应位置点缀极微弱的十字定位准星（`.ch-mark`，`+` 符号，`rgba(255,255,255,0.16)`），赋予系统仪器级的精密对齐感；四角不堆砌冗余伪状态、虚假端口或非官方版本号话术，保持整体视野纯粹干净；
  - 准星装饰元素设为 `pointer-events: none; user-select: none;`，绝不干扰交互；移动端（`max-width: 768px`）自动隐藏，保证小屏操作干净紧凑。
- **登录卡片细节与设计令牌契合**：
  - 卡片本体（`.login-card`）采用微弱高质感磨砂（`rgba(19, 27, 46, 0.82)` 对应后台暗色卡片变量 `#131b2e` + `backdrop-filter: blur(16px)`）配合层次阴影与 `border: 1px solid rgba(255, 255, 255, 0.08)`，顶缘增加 1px 细微品牌高光流线（`.card-glow-beam`）；
  - 顶部 Logo 采用纯净无边框贴合渲染（`.login-logo-img` 42px 直接呈现，与后台 AppShell 顶栏 Logo 形式统一），搭配纯净副标题文字与表单前缀图标（`PersonOutline`、`LockClosedOutline`），通透轻盈。

### 9.15 前端时间展示与时区规范
- **统一格式化工具**：全站所有表格、卡片、时间轴的时间展示一律调用 `web/src/utils/time.ts` 集中函数（`fmtDateTime` 含秒、`fmtDateTimeMinute` 不含秒、`fmtDate` 仅日期、`fmtTime` 仅时刻）。**严禁在前端组件中手写 `slice(0, 19).replace('T', ' ')` 等易错字符串切割**。
- **时区权威感知**：时间格式化工具严格消费后端下发的 `server.timezone` 系统时区配置；当服务端未显式配置时平滑降级为浏览器本地时区。
- **时间范围筛选控件规范**：凡具备时间范围筛选的列表（如操作审计、会话记录、监控历史等），统一采用 `NDatePicker type="datetimerange"`（输出标准时间戳区间，清空为 `null`），提交接口前通过 `new Date(ms).toISOString()` 序列化为标准 RFC3339 字符串，且在触发筛选时自动重置页码为第一页。


