# 自研云堡垒机 / 主机管理助手 需求文档

> 参考对象：长亭百川云「牧云·主机管理助手 / 云堡垒机」
> 参考来源：<https://rivers.chaitin.cn/product/co1som8hp38s73e07jog> 及其官方文档 `/docs/zh/cloudwalker/{intro,function,start}`
> 背景：原 SaaS 服务说停就停、不提供服务，数据与可用性不可控，故自研一套可自部署的等价系统，核心目标是「自己掌控、永久可用、不依赖第三方 SaaS」。
>
> **必读 + 维护约定**：
> 1. 本文档**每次会话/每次任务开工前必须完整阅读**，并且**必须严格遵循其中的全部约束**（§7 代码质量校验、§8 前端布局规范、§8.5 终端断线重连、§8.6 维护握手与告警防抖等）。开工前先读、改代码时对照，不得凭印象或惯例行事；与文档冲突时以文档为准，若文档确实过时则先更新文档再改代码。
> 2. 本文档是需求与设计的唯一事实来源。**所有需求的改动和新增，实现代码的同时必须同步维护本文档**——涉及协议字段、数据模型、REST/WS 接口、配置项、前端布局与交互规范、构建与部署方式的，都要在对应章节补齐或更正，不允许只改代码不更新文档。

---

## 1. 产品定位

简单、易用的主机一体化运维托管平台，用浏览器像「放羊」一样集中管理分散在各处（本地、内网、多云、家用）的服务器/PC，覆盖运维、监控、安全三类能力，面向个人开发者与小型团队自部署。

一句话：**一个自托管的 Web 控制台 + 装在被管主机上的 Agent，Agent 主动回连控制端，打通内网，实现远程运维、监控、安全审计。**

---

## 2. 总体架构

### 2.1 组件

| 组件 | 职责 | 说明 |
| --- | --- | --- |
| 控制端（Server / Console） | Web 控制台 + API 网关 + 接入服务 + 数据存储 + 告警引擎 | 自部署的中心节点，对外提供 Web UI 与 API，接收 Agent 回连 |
| Agent（被管端） | 采集主机信息、执行下发指令、提供终端/文件/Docker 操作通道 | 轻量常驻进程，兼容 Linux/Windows、arm64/amd64 |
| 组网模块 | 内网穿透 / 虚拟组网 | 把处于不同内网的主机打通成一个大内网 |
| AI 引擎 | 对接大模型，提供智能诊断、命令生成、报告解读、异常检测等能力 | 可接入第三方模型（OpenAI / 兼容 OpenAI 协议的网关）或本地模型；通过统一适配层调用，模型可热切换 |

### 2.2 关键设计原则（对标原产品痛点）

- **无需开放端口**：Agent 主动出向回连控制端（WebSocket / 长连接），被管主机不需要公网 IP、不对外暴露 SSH/RDP 端口。
- **一键绑定**：控制端生成一条安装命令（含 token），在被管主机执行后即完成注册；Windows 提供安装包。
- **跨架构/跨系统**：Agent 兼容 Linux 与 Windows，兼容 arm64 与 amd64。
- **永久托管**：自部署，数据落本地，不受第三方停服影响。
- **自包含部署**：控制端可一键部署（建议 Docker / 单二进制 + 内置 DB），不依赖外部云服务。
- **AI 可选可插**：AI 能力为增强项，模型走统一适配层（兼容 OpenAI 协议），可接第三方或本地模型；未配置模型时系统其余功能照常可用，不硬依赖 AI。

---

## 3. 功能需求

### 3.1 主机绑定与管理（核心）

- **一键绑定**：控制台生成安装命令，复制到被管主机执行即完成绑定；Windows 可下载安装包点击安装。
- **主机列表**：展示所有已绑定主机，支持分组、搜索、筛选、排序。
- **主机详情**：进入单台主机的管理视图，按 tab 切换各功能模块。
- **主机状态**：在线/离线状态实时显示。
- **解绑/卸载**：支持从控制端解绑主机，并支持卸载 Agent。

### 3.2 资源监控

- **实时监控**：CPU、内存、网络吞吐、磁盘读写吞吐的实时占用情况。
- **存储监控**：展示各挂载点的容量与占用情况。
- **历史监控**：默认保存 7 天历史数据，支持按起止日期、时长查看历史占用曲线。
- **自定义阈值告警**：可按参数设置告警条件（见 3.11 监控告警）。

### 3.3 系统状态

- **进程清单**：进程名、PID、UID、CPU/内存使用率、启动时间，支持排序，支持结束进程。
- **网络端口**：协议、监听地址、进程名等端口信息。
- **系统账号**：系统用户名、账号状态、UID 等。
- **登录历史**：登录用户名、登录状态、登录时间等记录。

### 3.4 Docker 管理

> Docker 面板定位为**运行时观测与运维**（容器/镜像的查看、启停、日志、终端、镜像加速），**不再承担应用部署入口**。所有新建服务一律走 §3.7 应用中心统一部署。

- **容器管理**：容器运行状态、名称、镜像、资源使用、暴露端口、创建时间；支持启停、重启、删除、查看日志；支持批量操作。
- **纳管状态列**：容器列表识别 `watchman.app=<appID>` 标签：由应用中心部署的容器显示「应用中心纳管」并可跳转对应应用详情；标签指向的应用已删除时显示警告；无标签的外部/临时容器如实标注。
- **部署新应用**：容器页签工具栏提供「部署新应用」入口，跳转 `/apps/create?host_id=<当前主机>`，预选目标主机后由应用中心完成创建。
- **镜像加速**：主机详情 Docker「镜像」页签提供「镜像加速」入口，支持查看当前主机的 `registry-mirrors`、快捷选取常用国内加速源（DaoCloud、1ms 等）、手动编辑加速镜像列表，保存后自动平滑更新 `daemon.json` 并重载 Docker 守护进程，无需停机或重启现有容器。
- **容器终端**：一键进入容器 shell。
- **镜像管理**：镜像名称、ID、大小、创建时间；支持删除单个、批量删除、清除未使用镜像。
- **一键安装 Docker**：若主机未安装 Docker，控制端可一键为其安装。

### 3.5 在线终端（Web Terminal）

- **浏览器内 SSH/Shell**：无需本地安装客户端，浏览器随时连接主机终端。
- **免密登录**：通过配置登录账号实现一键免密登录。
- **多标签**：同一主机可开多个终端标签。
- **主题切换**：提供多套终端主题。
- **终端分享**：生成临时口令 + 分享链接，支持「观看」「控制」两种模式，多人可同时使用同一个终端，拥有者可实时查看/打断/终止分享。
- **登录行为配置**：全局登录行为可选「禁止登录 / 口令登录 / 缺省账号登录」；也可为单台主机单独配置登录账号，单机配置优先于全局。
- **Windows 支持**：支持 PowerShell / CMD。

### 3.6 文件管理

- **目录操作**：新建目录、移动目录（可同时改名）。
- **文件操作**：上传、下载、复制、移动、删除、预览、编辑。
- **初始化路径**：可配置进入文件管理时的默认路径。
- **大文件/进度**：上传下载需展示进度。

### 3.7 应用中心（统一部署入口）

> 原主机级「应用市场」Tab 与 Docker「创建容器」表单已废弃删除：所有应用部署（模板/单镜像/Compose/Git）统一收敛到全局应用中心，主机详情不再有应用市场页签。模板目录 API `GET /apps/catalog` 保留，作为应用中心「应用模板」来源的数据。

- **四种部署来源**（`Application.source_type`）：
  - `template`：内置模板一键部署（Nginx、Redis、MySQL、PostgreSQL、雷池 WAF 等），只填模板参数即可拉起；模板经 `ResolveTemplate` 解析为镜像 + 环境变量 + 默认端口/卷，走与应用中心一致的引擎部署流（持久化、健康检查、滚动替换）。
  - `image`：直接输入镜像引用（`nginx:alpine`、`registry.example.com/app:1.0`）部署单容器。
  - `raw_compose`：在线编写 Docker Compose YAML 编排微服务栈。
  - `git`：链接 Git 仓库（GitHub 授权选择或手动 URL），Dockerfile / Compose 构建，支持推送自动部署（Webhook）。
- **端口绑定范围**（`PortMapping.bind_scope`）：每条端口映射可选「公网开放」（默认，`-p host:container` 绑定 0.0.0.0）或「仅异地组网」（`-p <tailscaleIP>:host:container` 只绑定主机 Tailscale IP，公网不可达）。选择「仅异地组网」时目标主机必须已加入组网，引擎找不到 Mesh IP 时降级为公网绑定并在部署日志中告警。
- **域名与反代集成**（`PUT /apps/:id/proxy`）：
  - `local` 模式：应用所在主机自建 Nginx（`watchman-app-nginx` 容器）反代，上游 `127.0.0.1:<port>`。
  - `gateway` 模式：另选一台有公网 IP 的网关主机运行 Nginx，上游指向**应用主机的 Tailscale Mesh IP**（经 `network.Store.GetNodeStatus` 解析），公网流量在网关卸载 TLS 后经组网内网穿透到无公网的应用主机。
  - 证书自动化：绑定域名时若证书中心无覆盖证书，自动用默认 ACME 账户签发后写入 Nginx 并热加载。
  - 网关主机 ID 持久化在 `Application.proxy_gateway_host_id`，解绑时同时清理应用主机与网关主机两侧配置。
- **创建时即可绑定域名**：创建页填写域名与网关节点后，提交依次执行 创建应用 → 自动签发证书 → 下发反代；绑定失败不回滚应用创建，可稍后在详情页重试。
- **全生命周期管理**：部署历史、构建日志、健康检查探活失败自动保留旧版本（`-next`/`-prev` 滚动替换）、回滚、启停、AI 排障诊断，对所有来源的应用一致生效。
- **已安装管理**：应用列表/详情支持重启、停止、启动、查看日志、删除（`watchman.app` 标签标记容器归属）。
- **可扩展**：模板定义为服务端声明式配置（`server/internal/apps/templates.go` 的 `Catalog`），支持自定义接入新模板。

### 3.8 推送命令（批量执行）

- **批量选择主机**：在主机列表勾选目标主机。
- **多 Shell 类型**：Linux Bash、Linux sh、Windows CMD、Windows PowerShell。
- **命令编辑 / 常用命令**：支持编辑命令，或从「自定义常用命令」库中选取。
- **执行与结果**：一键开始推送，按主机展示执行结果输出。
- **脚本支持**：支持下发脚本（不仅是单行命令）。

### 3.9 自动组网 / 内网穿透

- **动态组网**：将不同内网的主机组到一个虚拟内网，使家中的电脑与办公室的服务器等能互联互通。
- **操作简单**：选择主机加入组网即可，尽量零配置。
- **跨区域组网**：打通处于不同内网的主机之间连通关系。

### 3.10 安全扫描

- **安全基线扫描**：对服务器进行安全基线检查。
- **入侵痕迹排查**：扫描主机上的入侵痕迹。
- **漏洞扫描**：对服务器进行漏洞扫描（对标原产品「百川漏扫」联动能力）。
- **扫描报告**：展示扫描结果，可查看与导出。

### 3.11 监控告警

- **内置监控项**：默认提供「主机上线」「主机离线」告警。
- **月流量超额预警（内置，非规则）**：基于每台主机在「财务与规格」中配置的「流量限制 (GB)」与「月流量计算类型」（双向/仅上行/仅下行）自动判定：达到 80% 触发警告、达到 100% 触发严重告警（触发 ID 为 `builtin-traffic-80` / `builtin-traffic-100`），回落后自动标记解决。**不存在也不允许再创建 `traffic_high` 类型的告警规则**——流量配额是主机属性而非全局规则，告警中心的规则类型选项中已移除该项。未配置流量限制的主机不产生流量告警。
- **到期续费提醒（内置，非规则）**：主机在「财务与规格」中配置了「到期时间」后，距到期不足 30 天时自动发起一次续费提醒（触发 ID `builtin-expiry-remind`，警告级别，走告警中心与 webhook 通知）；到期时间在 30 天以外的主机不提醒，续费（到期时间推后）后提醒自动解除，逾期未续保持提醒且不重复触发。
- **自定义监控项**：可添加监控参数并设置告警条件。
- **消息通知**：支持配置告警通知方式（邮件/Webhook 等，可扩展）。
- **秒级感知**：主机状态变化及时告警。

### 3.12 系统设置

> 实现备注（2026-09-26，设置中心 Phase 2）：所有设置项走统一存储 `server/internal/settings`
>（`settings.json`，system/user 双作用域、点分键、注册表校验），前端经 `stores/settings`
> 统一读写；`appearance.theme_mode` 已从浏览器 localStorage 迁到服务端（localStorage 仅作首屏缓存）。
> 秘密类配置（AI Key / Git Token）不进统一设置，保留专用存储。

- **通用设置**：
  - 首选页面（进入主机管理时默认展示的模块：资源负载/系统状态/Docker/在线终端/文件管理/应用市场）。
  - 开关加载页面时的功能提示语。
- **自定义常用命令**：增删改查，用于推送命令快速选取。
- **监控告警**：见 3.11。
- **在线终端配置**：登录行为、各主机登录账号、默认账号。
- **文件管理配置**：默认管理路径。
- **动态组网**：组网管理入口。
- **分组权限**：见 3.13。

### 3.13 分组与权限

- **分组管理**：分组的新增、修改、删除。
- **分组-主机关联**：给分组关联主机。
- **分组-用户授权**：给分组授权用户，使用户获得该分组下主机的访问权限。
- **分组视角**：从分组出发管理主机与用户关系。
- **用户视角**：从用户出发进行分组授权。

### 3.14 多云联动（可选 / 二期）

- **联动云计算 API**：录入阿里云/腾讯云/微软云/亚马逊云等的 token，对云上服务器进行自动发现与联动管理。

### 3.15 用户与认证

- **登录认证**：控制端登录（账号密码，可扩展 OAuth/SSO）。
- **多用户**：支持多用户，配合分组权限实现隔离。
- **操作审计（自研增量）**：记录谁在何时对哪台主机做了什么操作（原产品为 SaaS，自研需自保审计日志，满足「停服自担」背景下的可追溯要求）。

### 3.16 AI 能力（增强）

> 设计原则：AI 是「增强」而非「必需」。通过统一模型适配层（兼容 OpenAI 协议）接入，可接第三方（OpenAI / 兼容网关 / 国产大模型）或本地模型，模型可热切换；未配置模型时其余功能不受影响。所有 AI 调用需可审计、可关闭、可限速。

#### 3.16.1 模型接入与配置
- **统一适配层**：兼容 OpenAI Chat Completions 协议；支持配置 BaseURL / API Key / 模型名 / 超时 / 重试。
- **多 provider**：可配置多个模型来源（OpenAI、兼容网关、本地 Ollama/vLLM 等），按场景或按用户选择/路由。
- **可观测**：记录 token 用量、耗时、调用来源，便于成本控制。
- **开关与降级**：全局开关、按功能开关；模型不可用时降级为「不使用 AI」的常规流程，不阻断主流程。

#### 3.16.2 智能终端助手
- **自然语言转命令**：在在线终端/推送命令中用自然语言描述意图，AI 生成对应 Shell（Bash/PowerShell/CMD）命令。
- **命令解释**：对任意命令给出中文解释与风险评估（高危命令提示）。
- **一键执行 / 编辑**：生成的命令默认进入可编辑草稿，确认后再执行，不自动盲跑。
- **上下文感知**：可携带当前主机信息（系统、发行版、已装软件等）提升生成准确度；敏感信息脱敏后再送模型。

#### 3.16.3 智能诊断
- **异常根因分析**：当主机离线、资源告警、进程异常、端口异常时，AI 结合监控数据与系统状态给出可能原因与处置建议。
- **日志解读**：上传/选择一段日志，AI 总结要点、定位报错、给出修复建议（对标原产品「百川漏扫」大模型报告解读思路）。
- **故障排查向导**：多轮对话式排查，AI 根据反馈建议下一步要执行的诊断命令（用户确认后下发）。

#### 3.16.4 监控与告警智能分析
- **智能阈值/异常检测**：在固定阈值之外，提供基于 AI 的异常检测（趋势突变、毛刺），降低手动配阈值成本。
- **告警降噪与合并**：对告警风暴做合并、去重、分级，AI 生成一句话摘要。
- **告警解读**：每条告警附带 AI 生成的可能原因与处置建议。

#### 3.16.5 安全扫描报告解读
- **基线 / 漏洞 / 入侵痕迹报告解读**：扫描完成后 AI 生成中文解读：风险概述、影响范围、修复优先级、修复命令/步骤。
- **可对话追问**：对报告内容多轮追问，逐步细化到「这台主机这个漏洞怎么修」。
- **修复建议可执行化**：建议中的命令可一键带入「推送命令」草稿，确认后下发。

#### 3.16.6 主机概览与运维报告
- **单机/多机巡检报告**：按需或定时生成主机健康度报告（资源、进程、登录、安全扫描），AI 撰写摘要与改进建议。
- **自然语言问答**：在控制台用自然语言问「哪台机器磁盘快满了」「最近有哪些异常登录」，AI 查询后回答（Function/Tool calling 调用平台 API，而非自由编造）。

#### 3.16.7 权限与安全约束
- **数据脱敏**：送往外部模型前对密钥、token、密码、敏感路径等做脱敏；本地模型可选不脱敏。
- **不自动高危操作**：AI 仅给建议/草稿，高危命令、删除、重启等一律需人工二次确认。
- **审计留痕**：记录每次 AI 调用的输入摘要、输出、操作者、是否被采纳/执行。
- **按用户/分组限速**：可对 AI 调用做配额与频率限制。

---

## 4. 非功能需求

| 维度 | 要求 |
| --- | --- |
| 可用性 | 自部署、不依赖任何第三方 SaaS；单机即可跑，支持数据备份/迁移。 |
| 部署 | 控制端支持 Docker 一键部署或单二进制部署；内置可选数据库（SQLite/Postgres 等）。 |
| 兼容性 | Agent 兼容 Linux（常见发行版）/ Windows，arm64/amd64。 |
| 性能 | 资源监控采集频率可调（默认秒级~分钟级）；历史数据至少保留 7 天（可配置保留时长）。 |
| 安全 | Agent ↔ 控制端全链路加密（TLS/WSS）；token 鉴权；终端/文件等高危操作需鉴权与审计。 |
| 可扩展 | 应用市场、告警通知方式、扫描规则均可扩展。 |
| 数据自主 | 所有主机信息、监控数据、审计日志均落本地存储，可导出/备份。 |
| AI 接入 | 通过统一适配层接入，兼容 OpenAI 协议；支持第三方与本地模型；AI 调用可关、可降级、可审计、可限速；敏感数据上送前脱敏。 |

---

## 5. 对比原产品的自研增补点

原 SaaS 停服即全失，自研需额外补齐以下能力，避免重蹈覆辙：

1. **数据自主可控**：监控/审计/配置全部本地存储，支持一键备份与跨机迁移。
2. **离线可用**：控制端不依赖任何云端鉴权/计费，断网仍可用。
3. **操作审计**：完整记录登录、终端会话、文件操作、命令下发、高危动作，可导出。
4. **会话回放（建议）**：终端会话可录制并回放，满足事后追溯。
5. **高危命令管控（建议）**：可配置命令黑/白名单、高危命令二次确认或拦截，避免误操作。
6. **部署可移植**：不绑定特定云厂商，任意一台 Linux 机器即可托管控制端。

---

## 6. 交付范围（建议分期）

### Phase 1（MVP，可替代日常使用）
- 控制端 + Agent 基础架构（回连、鉴权、心跳）
- 一键绑定（Linux/Windows）
- 主机列表 / 状态 / 分组
- 资源监控 + 7 天历史
- 系统状态（进程/端口/账号/登录历史）
- 在线终端（含登录行为配置、多标签、主题）
- 文件管理
- 推送命令（含常用命令库）
- 基础用户与分组权限
- 操作审计
- AI 适配层 + 智能终端助手（自然语言转命令 / 命令解释 / 高危提示）

### Phase 2
- 终端分享（观看/控制）
- Docker 管理（容器/镜像/终端/批量/一键装 Docker）
- 应用市场（一键安装常用应用）
- 监控告警（自定义监控项 + 通知方式）
- 动态组网 / 内网穿透
- 会话回放
- AI 智能诊断（异常根因 / 日志解读 / 故障排查向导）
- AI 主机概览与自然语言问答（Tool calling 查平台数据）

### Phase 3
- 安全扫描（基线 / 入侵痕迹 / 漏洞）
- AI 安全扫描报告解读 + 修复建议可执行化
- AI 监控异常检测 / 告警降噪与解读
- 多云联动
- 高危命令管控
- 多用户 SSO / 更细粒度权限
- AI 运维报告（定时巡检 / 健康度报告）

---

# 附录 A：技术选型方案

> 调研日期：2026-08-13。基于四路并行调研（开源项目、Agent 通信+Web 终端、组网/内网穿透、AI 适配层）汇总，务实面向个人开发者 / 小团队自部署。

## A.1 推荐架构总览

```
[被管机 Agent] ──(出站长连接 / TLS / 443)──▶ [控制端 Server] ◀── [Web 浏览器]
   Go 单二进制            Go 单二进制              Vue3 + xterm.js
   反向回连                内含网关/RPC/隧道        WebSocket 终端
   主动出向，不开入站端口   公网入口，会话锚点/审计
```

核心原则：
- **Agent 主动出站回连控制端，被管机零开放端口**；控制端是唯一公网入口。
- 所有会话流量经控制端中继，便于审计/录像。
- 控制端是「会话锚点」：Agent 短暂断连时，控制端保留 PTY 进程与状态，浏览器侧会话不中断、可续接。
- AI 为增强层，可关可降级，不影响主流程。

## A.2 技术栈推荐

| 层 | 选型 | 理由 |
| --- | --- | --- |
| 后端语言（控制端 + Agent） | **Go** | 单二进制、跨平台交叉编译、goroutine 适合长连接/多 Agent 并发；1Panel/Tactical RMM/Teleport/frp/Headscale 全是 Go，生态成熟，维护成本最低 |
| 前端 | **Vue 3 + TS + Element Plus / Naive UI** | 运维类组件库丰富，社区中文资料多，1Panel/JumpServer Luna 均用 Vue |
| 数据库 | **SQLite（默认）+ 可选 PostgreSQL** | 个人/小规模零运维单文件；规模上来再迁 PG。避免一上来 MySQL+Redis+Celery 三件套 |
| 缓存/队列（可选） | Redis（小规模可不用） / NATS（中等规模解耦 Agent 控制与批量任务，参考 Tactical RMM） | 按需引入 |
| Web 终端前端 | **xterm.js + addon-fit + addon-webgl + addon-search** | 事实标准（VS Code/JumpServer/1Panel 均用），生态最全，支持 CJK/IME |
| Agent PTY | Linux/mac：**creack/pty**；Windows：**UserExistsError/conpty**（ConPTY） | 统一 `PTY` 接口 + build tag 分平台 |
| 控制端 ↔ 浏览器 | **WebSocket**（gorilla/websocket 或 nhooyr.io/websocket） | 浏览器唯一现实选择，xterm.js 生态绑定 |
| Agent ↔ 控制端 | **gRPC over HTTP/2 + TLS（443）**，或轻量替代 **yamux over TCP+TLS** | gRPC 多 stream 天然多路复用、强类型 `.proto`；443 穿透性最好；yamux 适合砍依赖极致精简 |
| 会话录像 | **asciinema cast v2** 格式，控制端实时增量落盘 | 开放格式、现成播放器（asciinema-player）、增量写入断电不丢 |
| AI 适配层 | **LiteLLM SDK**（应用层统一接口 + fallback）+ 可选 **New-API** 网关 | OpenAI 协议为统一接口；热切换/降级/成本追踪内置 |
| 本地模型引擎 | 开发用 **Ollama**，生产用 **vLLM** | OpenAI 兼容 endpoint，零成本起步 |
| 组网底座 | **自研 Agent 回连反向隧道（frp 同构）** 为主，按需叠加 **Headscale + 自建 DERP** | 复用 Agent 通道、一键加入组网、稳定穿 NAT；mesh 直连需求出现再叠加 overlay |

一句话总结：**Go（控制端 + Agent 同语言）+ Vue3/xterm.js + SQLite/PostgreSQL + gRPC over TLS（Agent 反向回连）+ asciinema cast（录像）+ LiteLLM/Ollama（AI）+ 自研反向隧道组网**。

## A.3 Agent ↔ 控制端通信

- **传输**：gRPC bidi-stream over HTTP/2 + TLS（443）；多路复用 = 一个连接上每个业务（终端会话 / 文件传输 / 监控上报 / 命令执行 / Docker 操作）各开一条 stream，互不阻塞。轻量替代：hashicorp/yamux over TCP+TLS，自定二进制帧。
- **鉴权**：起步「enroll token + TLS + 长连接 Bearer Token」（一机一 token、短 TTL、吊销列表）；后续升级 **mTLS + 证书热轮换**（参考 Tailscale/Teleport）。
- **心跳/重连**：30s 心跳，3 次失联判定断连；指数退避（1s→2s→4s→…→60s 上限）+ 抖动。
- **会话恢复**：控制端保留 PTY 进程 ~60s，Agent/浏览器重连按 session_id 续接同一 stream；文件传输按 offset 续传。

## A.4 Web 终端

- 数据通路：浏览器 xterm.js ↔ WebSocket ↔ 控制端（路由/录制/广播）↔ Agent 长连接 stream ↔ PTY(bash/powershell)。
- **Windows**：ConPTY（`CreatePseudoConsole`），注意官方坑——通信通道分线程服务、关闭前先排空 output，否则死锁。Go 用 `UserExistsError/conpty`。
- **跨平台抽象**：Agent 定义统一 `PTY` 接口（Start/Read/Write/Resize/Close），build tag 分平台实现。
- **终端分享**：控制端做 pub/sub 广播；订阅者带 `controller`/`viewer` 角色，viewer 输入被丢弃，控制权切换记审计（asciicast `"m"` marker）。
- **终端内文件传输**：参考 ttyd 的 ZMODEM / trzsz，浏览器侧 rz/sz 体验，与独立文件管理面板互补。
- **参考实现**：直接抄 gotty 的 `pty.Start → io.Copy ↔ websocket` 骨架；精读 1Panel 的 agent 目录（产品形态最接近）与 ttyd 的 WS 帧协议。

## A.5 文件管理

- **走长连接多路 stream 分片传输**，绝不在 Agent 开 HTTP 端口（守住「不开放被管机端口」约束）。
- 消息定义 `read/write/stat/list/reume + offset/chunk_seq/sha256`，类 SFTP 语义自实现（不跑 SSH 服务）。
- **断点续传**：Agent 维护 `.part` + offset，重连 resume；分块 SHA256 + 整体校验。
- **预览/编辑**：流式给浏览器渲染（文本/图片/PDF）；编辑用 Monaco/CodeMirror，保存经 stream 写回，带版本/etag 校验防覆盖；大文件支持范围读（head/tail/offset）。

## A.6 组网 / 内网穿透

- **主路线：自研 Agent 回连反向隧道（frp 同构）**
  - 控制端内置类 frps 的隧道协调；Agent 复用已有回连通道注册虚拟 IP/端口映射，**不装第二个 VPN 进程**。
  - 一键加入组网：Web 选中主机 → 下发「加入网络」→ Agent 在回连通道上注册映射规则，零额外配置。
  - 访问：控制端为每台主机分配虚拟 IP（如 100.64.x.x），用户从控制端发起 SSH 时流量送入对应 Agent 反向隧道，Agent 本地转发到 `127.0.0.1:22` 或目标内网地址。
  - 加密：Agent↔控制端 TLS/mTLS；隧道内流量端到端加密。
- **可选增强：主机两两 P2P 直连**时叠加 **Headscale + 自建 DERP**（Tailscale 客户端 + 自托管控制面），控制端调用 Headscale API 编排「下发 auth key → tailscale up」；未出现大流量直连需求前不引入。
- **不采用**：nps（2021 停更，有安全风险）、WireGuard 原生（无控制面=自造 Tailscale，工程量不划算）、ZeroTier 自建 planet（改造深）。

## A.7 AI 适配层与模型接入

- **统一适配层**：LiteLLM SDK（`litellm.completion` 统一接口 + 内置重试/fallback/成本追踪）；多 key/额度需求时前置 New-API 网关。OpenAI Chat Completions 协议为统一接口（事实标准）。
- **Provider 优先级（默认配置顺序）**：
  1. 本地优先：vLLM/Ollama 跑 `qwen2.5-coder:7b`（命令生成）+ `qwen3:8b` 或 `glm-4:9b`（通用/工具调用/日志解读）。
  2. 第三方增强（国产合规、需 key、脱敏后）：DeepSeek、智谱 GLM、Kimi、通义千问。
  3. 兜底：OpenAI（合规允许 + 脱敏后）。
- **兼容性注意点**：通义千问 `tools` 与 `stream` 互斥（function calling 请求强制非流式）；不支持 tools 的模型降级为「prompt 内嵌工具说明 + 解析 JSON」。
- **Tool calling 工具集草案**：`list_hosts / get_host_detail / query_metrics / list_alerts / get_sessions / search_audit_logs / get_task_status`；工具调用一律非流式，后端做权限校验（只读优先）。
- **自然语言转命令**：system prompt 强制 JSON schema `{command, explanation, risk_level, requires_confirm}`；Few-shot 3–6 例；注入主机上下文（OS/发行版/shell/用户/目录）；**三层高危拦截**（黑名单正则预检 + risk_level 分级 + 高危强制人工确认，绝不自动执行）。
- **长日志/报告**：map-reduce 分段总结 + 向量检索 RAG（本地 embedding 如 bge-m3 / Qwen3-Embedding）+ 结构化字段抽取；优先用长上下文模型。
- **安全**：默认零外发（含敏感数据任务仅本地模型）；第三方调用前正则+字典脱敏（密钥/token/路径/IP/手机号 → 占位符），回填反向还原；provider 分级（本地=原文，国产合规=脱敏，境外=强脱敏或不送）。
- **审计**：每次调用记录 call_id/timestamp/user/scenario/provider/model/tokens/cost/latency/risk_level/blocked/fallback 等。
- **降级**：未配任何模型 key / 本地引擎未启动 → AI 功能静默关闭，堡垒机主流程不受影响。

## A.8 关键参考仓库（精读优先级）

1. **1Panel** `github.com/1Panel-dev/1Panel` — Go 后端 + Web 终端 + Docker/文件/监控/应用商店，产品形态最接近，通读 `agent` 目录。
2. **MeshCentral** `github.com/Ylianst/MeshCentral` — Agent 反向回连 + Web 控制台整体形态，最贴近牧云架构（Apache-2.0）。
3. **ttyd** `github.com/tsl0922/ttyd` — WS 帧协议 + ZMODEM 文件传输最小可行实现。
4. **gotty** `github.com/yudai/gotty` — Go `creack/pty + gorilla/websocket` 经典骨架，直接抄 PTY↔WS 桥接。
5. **JumpServer** `github.com/jumpserver/jumpserver` — 会话录像/命令审计/RBAC 模型参考（注意它是网关式而非 Agent 回连式）。
6. **Tactical RMM** `github.com/amidaware/tacticalrmm` — Go agent + NATS + Django + Vue 的 RMM 组合参考。
7. **frp** `github.com/fatedier/frp` — 反向隧道底座；**Headscale** `github.com/juanfont/headscale` — 可选 overlay 控制面。
8. **LiteLLM** `github.com/BerriAI/litellm` — AI 适配层；**New-API** — 可选网关。

---

# 附录 B：详细设计（MVP 范围）

> 目标：把附录 A 的技术选型落到可开工的工程设计。覆盖目录结构、Agent 协议（gRPC）、数据模型、控制端 REST API、部署与配置。范围对齐 Phase 1 + Phase 2 前段，标注「(P2/P3)」的为后续阶段。

## B.1 目录结构与模块划分

采用单仓多模块（monorepo）以便共享 `.proto` 与类型定义。

```
watchman/                       # 仓库名（暂定，可改）
├── go.mod                      # Go 模块根
├── proto/                      # gRPC IDL（控制端 ↔ Agent 共享）
│   └── agent.proto
├── server/                     # 控制端（Go 单二进制）
│   ├── cmd/watchman/main.go    # 入口
│   ├── internal/
│   │   ├── api/                # REST API（Gin/Echo，RESTful + JWT）
│   │   ├── rpc/                # gRPC server：接收 Agent 回连 stream
│   │   ├── ws/                 # 浏览器 WebSocket 网关（xterm 终端 / 文件传输）
│   │   ├── tunnel/            # 反向隧道协调（虚拟 IP/端口映射，组网）
│   │   ├── audit/              # 操作审计 + 会话录像（asciicast v2）落盘
│   │   ├── monitor/           # 监控数据入库 + 告警引擎
│   │   ├── ai/                 # AI 适配层（LiteLLM 封装）+ tool registry
│   │   ├── auth/               # 用户/JWT/分组权限/RBAC
│   │   ├── store/              # 存储层（SQLite/PG，GORM 或 sqlc）
│   │   └── config/             # 配置加载
│   ├── web/                    # 前端构建产物（嵌入二进制，go:embed）
│   └── Dockerfile
├── agent/                      # Agent（Go 单二进制，装在被管机）
│   ├── cmd/watchman-agent/main.go
│   ├── internal/
│   │   ├── conn/               # gRPC 反向回连 + 断线重连 + 心跳
│   │   ├── pty/                # 统一 PTY 接口（build tag: linux/darwin/windows）
│   │   ├── shell/              # 终端会话管理
│   │   ├── file/               # 文件操作（read/write/stat/list/分片）
│   │   ├── exec/               # 命令/脚本执行
│   │   ├── sysinfo/            # 系统状态采集（进程/端口/账号/登录）
│   │   ├── monitor/           # 资源监控采集（CPU/内存/磁盘/网络）
│   │   ├── docker/            # Docker 容器/镜像操作（P2）
│   │   ├── enroll/            # 首次注册（token → 长期凭证）
│   │   └── config/            # 本地配置 + 凭证存储
│   └── Dockerfile
├── web/                        # 前端（Vue3 + TS + Vite）
│   ├── src/
│   │   ├── views/             # 主机列表/详情/终端/文件/Docker/监控/设置
│   │   ├── components/        # xterm.js 终端、文件树、图表等
│   │   ├── api/               # REST/WS 客户端
│   │   └── store/             # Pinia 状态
│   ├── package.json
│   └── vite.config.ts
├── deploy/                     # 部署脚本/compose
│   ├── docker-compose.yml     # 控制端 +（可选）PG/Redis/vLLM
│   └── install-agent.sh       # 一键绑定脚本模板
└── docs/
```

模块依赖方向：`api/rpc/ws` → `audit/store/auth`；`ai` 独立可裁剪；前端构建产物 `go:embed` 进控制端二进制（单文件部署）。

## B.2 Agent ↔ 控制端协议（gRPC）

`proto/agent.proto` 要点。Agent 主动回连，控制端为 server，Agent 为 client，所有业务通过一条 bidi stream 复用（控制端也可主动下发指令）。

```protobuf
syntax = "proto3";
package watchman.agent;
option go_package = "watchman/proto/agentpb";

// 一条双向流承载所有通道（注册/心跳/终端/文件/命令/监控/系统状态/Docker）
service AgentService {
  rpc Connect(stream AgentMessage) returns (stream ServerMessage);
}

message AgentMessage {            // Agent → 控制端
  oneof payload {
    RegisterRequest  register  = 1;
    Heartbeat         heartbeat = 2;
    TerminalOutput    term_out  = 3;   // PTY 输出（带 session_id）
    FileChunk         file_chunk= 4;   // 文件传输数据块/读结果
    ExecResult        exec_result=5;   // 推送命令/脚本执行结果
    MetricsSample     metrics   = 6;  // 资源监控上报
    SysInfoSnapshot   sysinfo   = 7;  // 进程/端口/账号/登录快照
    DockerEvent       docker    = 8;  // (P2)
    Ack ack = 99;
  }
}

message ServerMessage {           // 控制端 → Agent
  oneof payload {
    RegisterResponse  register  = 1;   // 颁发长期凭证 / 拒绝
    HeartbeatAck      heartbeat = 2;
    TerminalInput     term_in   = 3;   // 键盘输入（session_id）
    TerminalResize    term_resize=4;
    TerminalOpen      term_open = 5;   // 开新会话：指定 shell/账号/cwd/尺寸
    TerminalClose     term_close= 6;
    FileOp            file_op   = 7;   // read/write/stat/list/resume
    ExecRequest       exec      = 8;   // 下发命令/脚本（shell 类型 bash/sh/cmd/pwsh）
    SysInfoQuery      sysinfo_q = 9;
    MetricsQuery      metrics_q = 10;  // 拉取当前/历史
    DockerOp          docker_op = 11;  // (P2)
  }
}

message RegisterRequest {
  string enroll_token = 1;        // 一次性，首次注册用
  string agent_id     = 2;        // 本地持久化的 agent 标识
  string hostname     = 3;
  string os           = 4;        // linux/windows
  string arch         = 5;        // amd64/arm64
  string distro       = 6;
  string agent_version= 7;
}
message RegisterResponse {
  bool   ok = 1;
  string agent_id   = 2;
  string auth_token = 3;          // 长期凭证（后续回连携带）
  int32  heartbeat_interval_sec = 4;
}

message TerminalOpen {
  string session_id = 1;
  string shell = 2;               // bash/sh/powershell/cmd
  string account = 3;             // 登录账号（可选，空则用默认）
  string cwd = 4;
  uint32 cols = 5; uint32 rows = 6;
  string share_mode = 7;          // none/view/control (P2)
}
message TerminalOutput { string session_id=1; bytes data=2; }
message TerminalInput  { string session_id=1; bytes data=2; }
message TerminalResize { string session_id=1; uint32 cols=2; uint32 rows=3; }
message TerminalClose  { string session_id=1; }

message FileOp {
  string op_id = 1;
  string op = 2;                  // read/write/stat/list/mkdir/move/remove/upload/download
  string path = 3;
  int64  offset = 4;
  int64  length = 5;
  int64  total_size = 6;
  uint32 chunk_seq = 7;
  bytes  data = 8;
  bool   overwrite = 9;
  string dest_path = 10;          // move/copy 目标
}
message FileChunk {
  string op_id = 1; uint32 chunk_seq=2; bytes data=3; int64 offset=4; bool eof=5;
  string sha256 = 6;              // 校验
}

message ExecRequest {
  string exec_id = 1;
  string shell = 2;               // bash/sh/cmd/powershell
  string command = 3;             // 单行或脚本
  bool   is_script = 4;
  int32  timeout_sec = 5;
}
message ExecResult {
  string exec_id = 1; int32 exit_code=2; bytes stdout=3; bytes stderr=4;
  int64  duration_ms = 5; string error = 6;
}

message MetricsSample {
  string agent_id = 1;
  int64  ts = 2;
  double cpu_usage = 3;
  double mem_usage = 4;            // 百分比
  int64  mem_total = 5; int64 mem_used = 6;
  double net_rx = 7; double net_tx = 8;   // bytes/s
  double disk_read = 9; double disk_write = 10;
  repeated Mount mounts = 11;      // 挂载点容量
  message Mount { string path=1; int64 total=2; int64 used=3; }
}

message SysInfoQuery { string kind=1; }   // process/port/user/login
message SysInfoSnapshot {
  string kind = 1; bytes json_payload = 2; // 结构化 JSON（不同 kind 不同 schema）
}
```

约定：
- **鉴权**：首次 `RegisterRequest(enroll_token)` 换 `auth_token`；后续回连在 gRPC metadata 带 `authorization: bearer <auth_token>`。
- **心跳**：`heartbeat_interval_sec`（默认 30s），3 次未响应判定离线。
- **多路复用**：所有业务在一条 `Connect` bidi stream 上以 oneof 分发；终端/文件等高频通道按 `session_id`/`op_id` 路由，互不阻塞。
- **会话恢复**：控制端按 `session_id` 持有路由表，Agent 短暂断连时保留 PTY ~60s，重连续接。
- **未开放端口**：Agent 仅出站，不监听任何端口。
- **维护握手**：控制端重启前下发 `MaintenanceNotice`，Agent 自升级前落盘 `reconnect_reason = "upgrade"`，重连带理由消解告警（见 8.6）。

## B.3 数据模型（核心表）

存储用 SQLite（默认）/ PostgreSQL。下表为逻辑模型，列名示意。

```
users(id, username, password_hash, email, role, status, created_at)
  role: admin / operator / viewer

groups(id, name, parent_id, description, created_at)          # 分组树
user_groups(user_id, group_id, role)                          # 用户-分组授权
host_groups(host_id, group_id)                               # 主机-分组关联

hosts(id, agent_id, hostname, os, arch, distro, agent_version,
      status, last_seen_at, tags json, created_at)
  status: online/offline/maintenance
  agent_id 唯一；一台主机 = 一个 agent 注册

accounts(id, host_id, name, type, credential_ref, is_default) # 登录账号配置
  type: password/key/token；credential_ref 指向加密存储

login_policy(id, host_id, behavior)                           # 禁止/口令/缺省
  缺省：取 accounts.is_default；单机配置优先于全局

term_sessions(id, host_id, user_id, account, shell, cols, rows,
              started_at, ended_at, status, share_mode, recording_path)
  recording_path 指向 asciicast v2 文件

audit_logs(id, ts, user_id, action, target_type, target_id,
           detail json, ip, user_agent, risk_level, result)
  action: login/term_open/exec/file_op/host_op/ai_call ...

metrics(agent_id, ts, cpu, mem_usage, mem_total, net_rx, net_tx,
        disk_read, disk_write)                                # 时序，保留 7 天（可配置）
  PG 可用 TimescaleDB hypertable；SQLite 用带索引普通表 + 定期清理

alerts(id, host_id, rule_id, severity, title, message,
       status, fired_at, acked_at, acked_by)

alert_rules(id, name, host_scope, metric, op, threshold,
            window_sec, severity, notify_channels, enabled)

commands_lib(id, user_id, name, shell, content, description, created_at)  # 常用命令

exec_tasks(id, user_id, shell, command, is_script, host_ids json,
           status, started_at, finished_at)                  # 批量推送命令
exec_task_results(task_id, host_id, exit_code, stdout, stderr, duration_ms)

ai_configs(id, key, value json)                               # provider/base_url/model/key/开关/限速
ai_tool_calls(call_id, ts, user_id, scenario, provider, model,
               prompt_tokens, completion_tokens, cost, latency_ms,
               tool_names json, risk_level, blocked, status, fallback_info)

settings(key, value json)                                     # 通用设置（首选页/提示语等）
```

加密：账号凭证用对称加密（AES-GCM，主密钥来自配置/环境变量）存 `credential_ref`，不明文落库。

## B.4 控制端 REST API（节选）

风格：RESTful + JWT（`/api/v1`）。WS 端点单独列出。仅列 MVP 必要项。

```
# 认证
POST   /api/v1/auth/login                 {username,password} → {token}
POST   /api/v1/auth/logout
GET    /api/v1/auth/me

# 主机
GET    /api/v1/hosts?group=&status=&q=
POST   /api/v1/hosts/enroll                → 生成 enroll_token + 安装命令（install=Linux curl 一键脚本，install_win=Windows irm|iex 一键脚本）
POST   /api/v1/hosts/:id/unbind
GET    /api/v1/hosts/:id
PATCH  /api/v1/hosts/:id  (tags/group)

# Docker（ op 透传给 Agent，清单见 B.8.3 ）
GET    /api/v1/hosts/:id/docker/ps
GET    /api/v1/hosts/:id/docker/images
GET    /api/v1/hosts/:id/docker/all       # 容器+镜像一次往返
GET    /api/v1/hosts/:id/docker/mirrors   # 获取 Docker daemon registry-mirrors
PUT    /api/v1/hosts/:id/docker/mirrors   # 更新 Docker registry-mirrors 并 reload daemon
POST   /api/v1/hosts/:id/docker/:op        # body {container,image,args_json?}；写操作记审计，rm/rmi/prune 为 high risk；pull/run 下发超时 5 分钟

# 系统状态
GET    /api/v1/hosts/:id/processes
GET    /api/v1/hosts/:id/ports
GET    /api/v1/hosts/:id/users
GET    /api/v1/hosts/:id/logins

# 监控
GET    /api/v1/hosts/:id/metrics?metric=&from=&to=&live=
POST   /api/v1/alerts/:id/ack

# 终端（控制面；数据走 WS）
POST   /api/v1/hosts/:id/terminals          → {session_id, ws_url}
POST   /api/v1/terminals/:sid/resize
POST   /api/v1/terminals/:sid/share         (P2)
DELETE /api/v1/terminals/:sid
GET    /api/v1/sessions?host_id=&from=&to=   # 会话历史 + 录像

# 文件
GET    /api/v1/hosts/:id/files?path=        # list/stat
GET    /api/v1/hosts/:id/files/content?path=&offset=&length=
POST   /api/v1/hosts/:id/files               # mkdir/move/copy/remove
# 上传/下载走 WS（大文件分片 + 断点续传）

# 推送命令
POST   /api/v1/exec/tasks                    # {shell,command,is_script,host_ids}
GET    /api/v1/exec/tasks/:id                # 结果聚合
GET    /api/v1/exec/tasks/:id/results?host_id=

# 分组/权限
GET/POST/PATCH/DELETE /api/v1/groups
POST   /api/v1/groups/:id/hosts              # 关联主机
POST   /api/v1/groups/:id/users             # 授权用户
GET/POST/PATCH/DELETE /api/v1/users

# 常用命令库
GET/POST/PATCH/DELETE /api/v1/commands

# 告警规则
GET/POST/PATCH/DELETE /api/v1/alert-rules

# 设置
GET/PUT  /api/v1/settings
GET/PUT  /api/v1/ai/config                   # AI provider/模型/开关/限速
GET/POST /api/v1/ai/chat                     # 自然语言问答（tool calling）
POST   /api/v1/ai/nl2command                  # 自然语言转命令（带 host_id 上下文）
GET    /api/v1/ai/audit                       # AI 调用审计

# 审计
GET    /api/v1/audit?user=&action=&from=&to=

# WebSocket
WS /api/v1/ws/terminal/:sid          # 终端 IO（浏览器 ↔ 控制端 ↔ Agent）
WS /api/v1/ws/file/:op_id            # 文件上传/下载分片
WS /api/v1/ws/agent/:agent_id        # (可选) 调试用 Agent stream 透视
```

鉴权：除 `auth/login`、`hosts/enroll` 外均需 JWT；按分组权限校验 `host_id` 访问范围；写操作记审计。

## B.5 终端数据通路（落地细节）

```
浏览器 xterm.js
  │ onData → WS 帧 {type:"input", sid, data}
  │ resize → WS 帧 {type:"resize", sid, cols, rows}
  ↓ WS(443/TLS)
控制端 ws.TerminalHandler
  │ 校验 session_id/JWT/权限；按 sid 路由到 Agent stream
  │ 旁路：录制 goroutine → asciicast v2 落盘（"o"=输出,"i"=输入,"r"=resize,"m"=marker）
  │ (P2) 旁路：广播给分享订阅者；viewer 输入丢弃
  ↓ ServerMessage{term_in/term_resize}
Agent internal/shell
  │ 按 sid 找到本地 PTY，写入 ptmx；pty.Setsize 调整
  │ PTY 输出 → AgentMessage{term_out}
  ↑ 回传至控制端 → 控制端转发 WS {type:"output", data} → xterm.js
```

- **录像**：控制端对每个 session 开一个 writer，实时追加行：`[t,"o",out]` / `[t,"i",in]` / `[t,"r","COLSxROWS"]`；首行 header 含 `width/height/timestamp/title`。回放：前端用 asciinema-player 或 xterm.js + 时间轴驱动。
- **分享(P2)**：控制端维护 `session_id → [subscribers]`；Agent 输出 fan-out 给所有订阅者；`controller` 可发输入，`viewer` 输入被丢弃；控制权切换写 `"m"` marker + 审计。
- **Windows**：Agent 用 `UserExistsError/conpty` 实现 `PTY` 接口，注意 ConPTY 死锁坑（通信分线程、关闭前先排空）。

## B.6 部署与配置

### 控制端单机部署（Docker Compose，推荐）

```yaml
# deploy/docker-compose.yml
services:
  watchman:
    image: watchman:latest
    ports: ["443:443"]            # 唯一对外端口（WS/gRPC/REST 统一 443）
    volumes:
      - ./data:/data              # SQLite + 录像 + 审计
      - ./config.yaml:/etc/watchman/config.yaml
    environment:
      - WATCHMAN_MASTER_KEY=${MASTER_KEY}   # 凭证加密主密钥
```

`config.yaml` 关键项：

```yaml
server:
  listen: ":443"
  tls:
    cert: /etc/watchman/cert.pem
    key:  /etc/watchman/key.pem
  public_url: https://watchman.example.com   # 生成安装命令用

store:
  driver: sqlite            # 或 postgres
  dsn: /data/watchman.db

agent:
  heartbeat_sec: 30
  session_keep_sec: 60      # 断连保留 PTY 时长

ai:
  enabled: true
  default_provider: ollama
  providers:
    ollama: {base_url: http://host.docker.internal:11434/v1, model: qwen2.5-coder:7b}
    deepseek: {base_url: https://api.deepseek.com, model: deepseek-chat, api_key: ${DEEPSEEK_KEY}}
  fallbacks: [ollama, deepseek]
  limits: {rpm: 60, tpm: 100000, monthly_budget_tokens: 5000000}
  mask_on_external: true    # 外送第三方前脱敏

tunnel:                     # 组网
  enabled: true
  cidr: 100.64.0.0/10        # 虚拟 IP 段

retention:
  metrics_days: 7
  recordings_days: 90
```

### Agent 一键绑定

控制端生成：

```bash
# deploy/install-agent.sh 模板（控制端渲染后下发）
curl -fsSL https://watchman.example.com/install?token=<enroll_token> | bash
```

脚本行为：
1. 下载对应 OS/arch 的 `watchman-agent` 二进制。
2. 写入 `enroll_token` + `server_url` 到本地配置。
3. 注册系统服务（systemd / Windows Service），启动。
4. Agent 回连 → `RegisterRequest` → 换取 `auth_token` 持久化 → 上线。

### 配置项清单（关键）

| 项 | 默认 | 说明 |
| --- | --- | --- |
| server.listen | :443 | 唯一对外端口 |
| server.public_url | — | 生成安装命令/分享链接用 |
| store.driver | sqlite | sqlite/postgres |
| agent.heartbeat_sec | 30 | 心跳间隔 |
| agent.session_keep_sec | 60 | 断连保留会话 |
| retention.metrics_days | 7 | 监控保留 |
| retention.recordings_days | 90 | 录像保留 |
| ai.enabled | true | AI 总开关 |
| ai.fallbacks | [ollama,...] | 降级链 |
| ai.limits.* | 见上 | 限速/预算 |
| ai.mask_on_external | true | 外送脱敏 |
| tunnel.cidr | 100.64.0.0/10 | 组网虚拟 IP 段 |

## B.7 MVP 推进顺序（落地路线）

1. **打通回连骨架**：`proto` 定义 → 控制端 gRPC server + Agent 反向 Connect + 注册/心跳；前端主机列表显示在线状态。
2. **终端 MVP**：Agent `creack/pty` 开 bash → bidi stream → 控制端 WS → xterm.js；Linux 单机跑通。
3. **审计/录像**：asciicast v2 实时落盘 + 会话历史接口 + 回放。
4. **文件管理**：list/stat/read/write + 上传下载分片。
5. **推送命令**：exec_task 批量下发 + 结果聚合。
6. **资源监控 + 系统状态**：MetricsSample 上报 + 历史查询 + 进程/端口/账号/登录。
7. **用户/分组/权限 + 登录账号配置 + 登录行为**。
8. **Windows Agent**：conpty 实现 `PTY` 接口，跑通 PowerShell/CMD。
9. **AI 适配层**：LiteLLM 封装 + `nl2command` + 命令解释（带高危拦截）。
10. **部署打包**：go:embed 前端 + Docker Compose + 一键绑定脚本。

## B.8 后端架构补充（查漏补缺）

> 对照第 3 节需求清单逐项核对后补充的设计点，覆盖 MVP 与 P2/P3 预留。

### B.8.1 补充数据模型

```
# 注册凭证管理
enroll_tokens(token, host_id_hint, created_by, created_at, expires_at,
              used_at, used_by_host, revoked)        # 一次性、可吊销、可过期

# 主机登录账号凭证（替代/细化 accounts.credential_ref）
credentials(id, host_id, name, type, secret_enc, created_at, updated_at)
  type: password/key/token；secret_enc = AES-GCM 密文；引用主密钥见 B.6

# 控制端用户登录会话/审计（区别于主机登录历史）
login_sessions(id, user_id, ip, user_agent, login_at, logout_at, status)
  status: success/failed/locked；用于 3.15 操作审计的登录部分

# 主机登录历史（来自 Agent sysinfo login kind，缓存/归档）
host_login_history(id, host_id, username, src_ip, login_type,
                   login_at, status)                 # status: success/failed

# 统一设置（system/user 双作用域，点分键；旧 term_prefs 表已并入，落库以跨设备一致）
settings(scope, key, value_json)          # 如 user/alice.terminal.theme, user/alice.appearance.theme_mode

# 文件编辑版本/锁（防并发覆盖）
file_locks(path, host_id, user_id, locked_at, expires_at)
file_versions(path, host_id, version, etag, size, modified_by, modified_at)
  # MVP 可只做 etag 乐观锁，P2 起保留版本

# 通知渠道（3.11 告警通知方式可扩展）
notify_channels(id, type, name, config json, enabled)
  type: email / webhook / (扩展)；config 如 {url, secret} / {smtp,to}
notify_rules(channel_id, alert_severity, enabled)

# AI 工具注册表（tool registry，权限 + 开关）
ai_tools(name, schema json, backend_handler, read_only, enabled, min_role)
  # list_hosts/get_host_detail/query_metrics 等，见 A.7 草案

# 应用中心（3.7 统一部署入口；模板目录为服务端声明式配置）
# 持久化实体走 apps.Store 的 Application/Deployment（JSONL 落盘），逻辑模型：
applications(id, name, host_id, source_type,        # git | image | template | raw_compose
             template_id, template_params json,
             repo_url, branch, auth_vault_id, auto_deploy,
             build_type, dockerfile, build_context, build_timeout_sec, compose_content,
             image, env_vars json,
             ports json,                              # [{host, container, bind_scope}]，bind_scope: public|mesh
             volumes json, healthcheck_url, container_name,
             domain, proxy_mode,                      # local | gateway
             proxy_gateway_host_id,                   # gateway 模式下的网关主机
             proxy_upstream,                          # 127.0.0.1:port 或应用主机 Mesh IP:port
             current_commit, last_deploy_at, created_at)
deployments(id, app_id, commit_hash, commit_message, trigger, status,
            started_by, started_at, finished_at, duration_ms, exit_code, error, build_log)

# 动态组网（P2，预留）
network_nodes(host_id, virtual_ip, status, joined_at)
network_routes(host_id, dest_host_id, type)          # direct/relay
```

### B.8.2 补充控制端 REST API

```
# 注册凭证
POST   /api/v1/enroll-tokens              # 生成（含安装命令）
GET    /api/v1/enroll-tokens
DELETE /api/v1/enroll-tokens/:token       # 吊销

# 主机登录账号/凭证
GET/POST/PATCH/DELETE /api/v1/hosts/:id/accounts
GET/PUT  /api/v1/hosts/:id/login-policy   # 禁止/口令/缺省

# 主机登录历史
GET    /api/v1/hosts/:id/login-history

# 终端偏好（旧接口 GET/PUT /api/v1/me/term-prefs 已于 2026-09-26 被统一设置取代）
GET    /api/v1/me/settings                 # 当前用户设置（data + schema），作用域 user
PUT    /api/v1/me/settings                 # {data: {key: value}} 部分更新，原子校验
GET    /api/v1/settings                    # 全局设置（admin），作用域 system
PUT    /api/v1/settings                    # {data: {key: value}} 部分更新（admin）
# 键命名空间（点分）：appearance.theme_mode, terminal.theme, terminal.default_shell,
# terminal.font_family, terminal.font_size, terminal.cursor_blink, terminal.scrollback
# 秘密（AI key / Git token 等）不进统一设置，留在各自专用存储

# 文件锁/版本（MVP 仅 etag；P2 起开放）
POST   /api/v1/hosts/:id/files/lock
GET    /api/v1/hosts/:id/files/versions?path=

# 通知渠道与规则
GET/POST/PATCH/DELETE /api/v1/notify-channels
GET/POST/PATCH/DELETE /api/v1/notify-rules

# AI 工具
GET/PUT /api/v1/ai/tools                  # 开关/权限

# 应用中心（3.7）
GET    /api/v1/apps                        # 应用列表（全部来源统一管理）
GET    /api/v1/apps/catalog                # 内置模板目录（声明式 Catalog）
POST   /api/v1/apps                        # 创建应用 {source_type, host_id, template_id/template_params | image | compose_content | repo_url/branch, ports[{host,container,bind_scope}], ...}
GET    /api/v1/apps/:id                    # 应用详情
PUT    /api/v1/apps/:id                    # 更新（如 Compose 内容）
DELETE /api/v1/apps/:id                    # 删除
POST   /api/v1/apps/:id/deploy|rollback    # 部署 / 回滚
POST   /api/v1/apps/:id/stop|start|restart
GET    /api/v1/apps/:id/deployments        # 部署历史（分页）
POST   /api/v1/apps/:id/deployments/:depID/diagnose   # AI 排障
PUT    /api/v1/apps/:id/proxy              # 绑定域名 {domain, mode: local|gateway, gateway_host_id, cert_id?, upstream?}；无证书时用默认 ACME 账户自动签发
DELETE /api/v1/apps/:id/proxy              # 解绑（同时清理应用主机与网关主机配置）
POST   /api/v1/apps/webhook/:token         # Git push 自动部署（token 鉴权）

# 组网（P2）
POST   /api/v1/networks/:id/join          # 选中主机加入
GET    /api/v1/networks/topology
DELETE /api/v1/networks/:id/hosts/:hid

# 备份/迁移
POST   /api/v1/system/backup              # 导出 SQLite/配置/录像打包
POST   /api/v1/system/restore             # 从备份恢复（离线运维操作）
GET    /api/v1/system/health              # 自检：DB/AI/Agent 连接数等
```

### B.8.3 协议补充

- **DockerOp/DockerEvent（P2）**：`op` 含 `ps/images/start/stop/restart/rm/logs/inspect/remove_image/prune_images/pull/run`；Agent 侧 shelling out to the docker CLI 执行（`agent/internal/docker/docker.go`）。`run` 的参数走 `DockerOp.args_json`：`{"name":"...","ports":["hostPort:containerPort"],"volumes":["/host:/container"],"env":["KEY=VALUE"],"restart_policy":"no|on-failure|always|unless-stopped","command":["arg1","arg2"]}`，空值条目跳过；**每个字段先过字符白名单正则（容器名/端口/卷/环境变量键/重启策略/镜像引用，拒绝以 `-` 开头等伪装 docker flag 的载荷）再进 argv，exec 不走 shell**；镜像缺失由 docker CLI 自动拉取，故 `pull/run` 的服务端下发与 agent 执行均取 5 分钟超时。
- **安全扫描（P3）**：新增 `ScanRequest(type, rule_set)` / `ScanResult(progress, findings json, report_ref)`，走 exec + 结果回传，不引入新通道。
- **Agent 自更新**：新增 `UpgradeRequest(version, url, sha256)` / `UpgradeProgress`，控制端可下发升级，Agent 校验签名后热更新（解耦发布）。
- **证书轮换**：`RegisterResponse` 增加可选 `client_cert`/`cert_expires_at`；Agent 持有证书，控制端在到期前通过现有 mTLS 通道下发新证书（带签名），Agent 热加载。

### B.8.4 安全与运维补充

- **备份/迁移**：控制端内置 `system/backup` 导出 SQLite 文件 + config.yaml + 录像目录打包（tar.gz），支持 `system/restore` 从备份恢复；满足 5.1「数据自主可控、一键备份与跨机迁移」。定时备份可由系统 cron 或外层 cron 触发。
- **enroll token 管理**：一次性、可过期、可吊销（`revoked` 标志 + 删除即失效）；一机一 token，注册成功后 `used_at` 置位并失效；防止 token 被多次复用注册。
- **凭证加密**：`credentials.secret_enc` 用 AES-GCM，主密钥来自 `WATCHMAN_MASTER_KEY` 环境变量（B.6）；主密钥轮换需提供 re-encrypt 命令。
- **凭证脱敏**：API 返回账号时 `secret` 永不回传，仅返回 `has_secret: true`。
- **JWT**：access token 短 TTL（如 30min）+ refresh token；写操作记 `audit_logs`。
- **限流/防爆破**：`auth/login` 按 IP+username 限速 + 失败锁定（`login_sessions.status=locked`）。
- **数据保留与清理**：定时任务按 `retention.*` 清理 metrics/recordings/audit；Agent 离线超过阈值（如 30 天）的主机可在 UI 标记为 stale，支持归档。

### B.8.5 可观测性（控制端自身）

- `/api/v1/system/health`：自检 DB、AI provider 可达性、在线 Agent 数、磁盘占用、录像/DB 体积。
- 结构化日志（zerolog/zap）+ Prometheus `/metrics`（可选）便于接入监控。
- Agent 侧：本地日志 + 回连后将关键日志摘要上报控制端（便于远程排障）。

至此后端架构覆盖：目录、协议、数据模型、REST API、终端通路、部署配置、安全运维、可观测、备份迁移，以及 P2/P3（Docker/应用市场/组网/扫描/证书轮换/Agent 自更新）的协议与模型预留。

(P2 起：终端分享、Docker 管理、应用市场、告警规则、动态组网、会话回放完善、AI 诊断/问答。)

---

## 7. 自动 Code Review 与代码质量校验规范

为了确保 AI 开发助手产出的代码稳定可靠、零语法/编译错误，所有代码生成与变更操作必须严格遵守以下自动 Review 机制：

1. **零错误防线 (Zero-Error Defense)**：
   - **前端 (Vue 3 / TypeScript)**：任何 `.vue`、`.ts` 文件修改后，必须验证组件与 Icon 组件导入正确、Vue template 结构完整无断句错误、全量去除废弃字符（如 Emoji 表情符号），且符合 TypeScript 类型约束。
   - **后端 (Go)**：修改 Go 代码后必须进行 `go vet` / `go build` 校验，确保导出的函数、结构体字段、Protobuf IDL 接口声明与实现完全一致。

2. **修改前精准对齐 (Precise Match Verification)**：
   - 使用 `Edit` 工具前必须先完整读取目标代码段落，确保 `old_string` 与文件原文逐字逐行完全匹配，防止产生不匹配报错或损坏文件内容。

3. **修改后自动 Review 流程**：
   - **编译/构建检查**：代码编写完成后，自动执行前端与后端构建测试，验证没有打破既有功能。
   - **功能与视觉对齐**：修改完毕后进行自动化与人工视觉 Review，确保 UI 布局契合长亭百川云/牧云控制台风格规范。

---

## 8. 前端布局规范

### 8.1 页面留白统一

- 路由页面的外层内边距由 `AppShell` 的 `.app-body` 统一施加，取值来自 `--page-padding`（桌面 16px / 窄屏 10px）。
- **页面组件自身不得再设置外层 `padding`**，否则各页面留白会重新出现不一致。
- 页面内主卡片/面板的内边距统一取 `--card-padding`，与页面留白同一档位。

### 8.2 大数据表格：整页高度自适应 + 表头分页固定

凡是承载**不定长数据列表**的表格（审计日志、进程清单、端口清单、系统账号、登录历史、会话记录、文件列表、容器/镜像列表、扫描结果、告警消息、用户、凭据、分组、命令拦截审计等），一律采用「整页填满视口 + 表头与分页固定 + 仅数据区滚动 + **必须分页**」的布局。不允许让整页跟着表格一起长高，把分页条推到首屏之外；也不允许不分页地一次渲染全量数据。

实现要求：

1. **页面根容器**撑满可用高度并作为纵向 flex 容器：

   ```scss
   .xxx-view {
     display: flex;
     flex-direction: column;
     height: 100%;
     min-height: 0; // 必须，否则子项无法收缩
   }
   ```

2. **表格容器**吃掉剩余空间，且**必须**同时写 `min-height: 0`。flex 子项默认 `min-height: auto`，不写则永远不会收缩，滚动条会跑到页面外层：

   ```scss
   .table-card {
     flex: 1;
     min-height: 0;
     display: flex;
     flex-direction: column;
   }
   ```

   包一层 naive `NCard` 时，还需让其内容区同样具备 flex 能力：

   ```scss
   .table-card :deep(.n-card-content) {
     flex: 1;
     min-height: 0;
     display: flex;
     flex-direction: column;
     overflow: hidden;
   }
   ```

3. **`NDataTable` 一律加 `flex-height`**，让表格按父容器高度计算滚动区，表头随之固定：

   ```vue
   <NDataTable flex-height :columns="columns" :data="rows" />
   ```

   - `flex-height` 与写死的 `:max-height` 是互斥语义，用了前者就不要再写后者。
   - 横向可滚动的宽表继续保留 `:scroll-x`，两者可共存。

4. **分页条固定在表格下方**，用 `flex-shrink: 0` 钉住，不参与滚动。分页条需要固定条高与上下对称内边距，让分页组件在条内垂直居中；只加 `padding-top` 会使分页紧贴卡片底边，视觉上像被裁切，「每页条数」下拉也会顶到边缘。

5. **必须有分页，不允许一次渲染全量数据**。上面第 4 条只规定了分页条的位置，不是「有分页时才管位置」——凡是不定长列表都必须分页。没有分页时，进程清单、目录列表、扫描发现这类数据会一次性渲染成千上万行，滚动卡顿且用户永远找不到末尾；配合写死的 `:max-height` 更会把超出高度的记录彻底藏起来（既没有分页也没有提示，用户以为数据只有这么多）。

   两种实现，按数据来源二选一：

   - **服务端分页**（数据会持续累积，且后端支持分页参数）：请求带 `offset` / `page_size`，响应的 `total` 必须是**未分页的总数**，前端用独立 `NPagination` 放进 `.table-pagination-bar`。换页、改每页条数都重新请求。已有实现：审计日志 `AuditList.vue`、会话审计 `SessionList.vue`（后端 `GET /sessions` 的 `offset`/`page_size` 为可选参数，不传返回全量，老调用方不受影响）。
   - **前端分页**（数据已整份在内存里，或后端本身有条数上限）：把全量数组交给 `NDataTable` 的 `:pagination`，由 naive-ui 负责切片并把分页条渲染在表格滚动区下方。统一用 `web/src/composables/useTablePagination.ts`：

     ```ts
     const rowCount = computed(() => rows.value.length)
     const { pagination, resetPage } = useTablePagination({ pageSize: 20, rowCount })
     ```

     ```vue
     <NDataTable flex-height :columns="columns" :data="rows" :pagination="pagination" />
     ```

     `rowCount` 用于**页码收敛**：删除某页最后一行、或筛选条件收紧后行数变少时，页码会被夹回最大有效页，否则表格会在「还有数据」的情况下渲染成空白。数据源重新加载（换目录、切页签、刷新）后调用 `resetPage()` 回到第一页。筛选条件变化也要 `watch` 到 `resetPage()`。

6. 全局提供可复用类（见 `web/src/styles/global.scss`）：`.page-flex-column`、`.table-flex-fill`、`.table-pagination-bar`。新页面直接套用，避免每处重复写 flex 三件套。

7. **不要用原生 `<NTable>` 承载不定长列表**。`NTable` 只是带样式的 `<table>`，没有 `flex-height`、没有分页、没有固定表头，写出来的页面一定违反本节。需要表格就用 `NDataTable` + `columns` render 函数；单元格样式因此由 render 生成，scoped 样式要用 `:deep()` 穿透。

**例外**：卡片内的辅助性小表格（常用命令库、备份列表、批量执行的主机勾选框、告警规则清单、单个分组的授权名单等），数据量天然有限且不是页面主体，可继续用固定 `:max-height` 限高、不分页，但**必须在代码注释里写明它属于本例外**，便于后续 review 区分「有意为之」和「漏了」。

### 8.3 滚动容器约定

- 一个可滚动区域只允许有一个滚动源。页面外层把滚动交给表格时，外层容器必须 `overflow: hidden`，防止出现双层滚动条。
- 长列表滚动容器统一带 `-webkit-overflow-scrolling: touch` 与 `overscroll-behavior: contain`（`global.scss` 已按类名集中声明）。

### 8.4 页签工作区与 KeepAlive

顶部页签是工作区语义：切走再切回**不能丢状态**。终端会话、已填未提交的表单、翻到的页码、展开的树节点，都必须原样保留。

实现约定：

1. `AppShell` 的 `RouterView` 用 `KeepAlive` 包裹，`include` 取「仍在页签栏里的视图组件名 + 当前路由的视图名」：

   ```vue
   <RouterView v-slot="{ Component, route: r }">
     <KeepAlive :include="keepAliveInclude">
       <component :is="Component" :key="cacheKeyOf(r)" />
     </KeepAlive>
   </RouterView>
   ```

   必须并上当前路由的视图名：主机详情这类页签是在组件挂载后的异步 `load()` 里才注册的，首屏渲染时 `include` 里还没有它，Vue 会给这次 vnode 打上「不缓存」标记，之后再改 `include` 也救不回来。

2. **每个路由组件必须用 `defineOptions({ name: 'Xxx' })` 声明稳定组件名**，同时在路由表写 `meta.viewName`，两者保持一致。`KeepAlive` 的 `include` 按组件名匹配，`<script setup>` 推断出的 `__name` 在生产构建里可能被压掉，不显式声明缓存就会静默失效。页签可能由 `HostList`、`AlertList` 等处打开而没传 `viewName`，`AppShell` 会按 `route.meta.viewName` 统一回填。

3. **缓存键要带路由参数**。同一个组件在不同参数下必须各自缓存，否则从主机 A 的详情切到主机 B，B 会复用 A 的实例并显示上一台主机的数据：

   ```ts
   function cacheKeyOf(r: RouteLocationNormalizedLoaded): string {
     const id = r.params.id
     return typeof id === 'string' && id ? `${r.name as string}:${id}` : (r.name as string) || r.path
   }
   ```

4. **页签关闭即释放缓存**。`cachedViews` 由页签列表推导，页签一关组件名从 `include` 消失，`KeepAlive` 随即卸载实例并触发 `onUnmounted`——终端断连、定时器清理、图表 dispose 都依赖这一步。不要在页签之外另建缓存清单，两份状态一定会漂移。

5. **页内子页签要写进页签的 `path`**。主机详情的「文件管理 / 资源监控 / 在线终端」是 `?tab=` query，点顶部页签回来时走的是 `tab.path`；不带 query 就会退回默认子页签，正在用的终端随之被卸载。切换子页签时用 `openTab` 把带 query 的完整路径回写到页签上（见 `HostDetail.vue` 的 `rememberSubTab`）。

6. **组件被缓存后仍会收到路由 watch 通知**，那时 `route` 已经指向别的页面。所有依赖 `route.query` / `route.params` 的 watch 都要先确认自己仍是当前路由（`if (route.name !== 'host-detail') return`），否则会把状态重置成默认值。

7. **被缓存的组件不会卸载，副作用要按激活状态管理**：
   - 轮询、`setInterval`：`onDeactivated` 停、`onActivated` 起，并在恢复时立刻取一次数据，避免图表留空档。参考 `MetricsPane.vue`。
   - 依赖容器尺寸的组件（xterm、echarts）：隐藏时容器量到 0，`onActivated` 里 `nextTick` 后重新 `fit`/`resize`，终端还要把新尺寸同步给远端 PTY，否则回显错行。参考 `TerminalPane.vue`。
   - 长连接（WebSocket / PTY）**不要**在 `onDeactivated` 里断开，那正是页签要保住的东西。

### 8.5 终端会话的断线与重连

终端不是普通页面，它承载着一个真实运行的 shell。网络抖动、合盖休眠、切后台都不能让用户丢掉正在跑的进程。

服务端（`server/internal/ws/terminal.go`）：

1. **PTY 比 socket 活得久**。最后一个客户端离开时不立即发 `TermClose`，而是挂一个 `sessionGrace`（60 秒）定时器；期间有客户端按同一 `sid` 接回来就取消定时器，直接续上原 shell。这就是设计文档 A.3 承诺的「会话恢复」。

2. **重连不能重开 PTY**。`getOrCreateHub` 的第二个返回值表示「该会话已有活着的 PTY」，为真时必须跳过 `TermOpen`，否则会在原 shell 之上再开一个，用户看到的是被清空的终端。

3. **宽限期到期要彻底清理**：`TermClose` + `UnregisterSession(sid)` + 审计 `End`。少了 `UnregisterSession`，`sessionIdx` 会无限增长。

4. **必须设读写超时**。浏览器每 25 秒发一次 ping，服务端 `SetReadDeadline(pongWait = 70s)`，任何客户端帧或 pong 都刷新它。没有这个，硬断线（拔网线、断电）要等 TCP keepalive 两小时才被发现，PTY 一直挂着占资源。写侧用 `SetWriteDeadline(10s)`，避免一个卡死的观众阻塞整个广播循环。

前端（`web/src/components/host/TerminalSession.vue`）：

5. **`onclose` 必须重连**，指数退避 1s→2s→4s→…→15s 上限；累计超过 60 秒（服务端宽限期）就放弃并提示用户新开标签——那时 PTY 已被回收，继续重试只会连到一个不存在的会话。

6. **重连复用同一个 xterm 实例**，只重建 WebSocket。重建终端等于清屏，用户会以为会话丢了。`term.onData` / `term.onResize` 只在 `start()` 里绑一次。

7. **区分「我们主动关」和「意外断开」**：组件卸载时先置 `closedByUs = true` 再 `ws.close()`，否则清理过程会触发一次无意义的重连。收到服务端 `ended` 帧同样要停止重连——那是 shell 真的退出了。

8. 重连成功后要重新 `sendResize()`，把当前终端尺寸同步给远端 PTY。

### 8.6 维护握手与告警防抖规范

控制端重启升级与被控端 Agent 在线自升级是运维高频动作，绝不能因为短暂的断线与重连导致告警引擎向运维人员推送大面积虚假的「主机离线 / 主机上线」轰炸。系统必须采用**「双向维护握手信令（Controlled Maintenance Handshake）」**闭环机制。

实现约定：

1. **协议信令支撑（`proto/agent.proto`）**：
   - `ServerMessage` 增加 `MaintenanceNotice { reason, expected_duration_sec }`；
   - `RegisterRequest` 增加 `reconnect_reason`（支持 `"upgrade"` / `"maintenance"`）。

2. **控制端优雅退出广播（Server 重启/升级场景）**：
   - 控制端（`server/cmd/watchman/main.go`）捕获 `SIGTERM` / `SIGINT` 时，在调用 `GracefulStop` 之前，必须通过 `Registry.BroadcastMaintenance("server_restart", 120)` 向所有在线 Agent 广播下发维护预告帧；
   - Agent 收到后将 `reconnect_reason = "maintenance"` 落盘至 `agent-state.json`；
   - 控制端拉起后，在维护窗口内连回并携带 `reconnect_reason = "maintenance"` 的 Agent，告警引擎必须静默对齐状态，**严禁触发上线通知**。

3. **被控端在线自升级（Agent 自升级场景）**：
   - 控制端在 `/api/v1/hosts/:id/upgrade` 发出升级指令前，必须自动调用 `alertMonitor.SetHostMaintenance(hostID, 180s, "agent_upgrade")`，为目标主机开启专属维护静默期；
   - 处于维护期内的主机，告警引擎在 `checkRule` 中评估 `RuleOffline` 时直接静默跳过，**严禁触发离线告警**；
   - Agent 在 `upgrade.Manager` 完成二进制自替换、准备 `os.Exit(0)` 重启前，必须将 `reconnect_reason = "upgrade"` 写入本地持久化状态；
   - Agent 重启后发送携带 `reconnect_reason = "upgrade"` 的 `RegisterRequest`；控制端收到后清除维护标记，平滑同步为在线，**严禁触发上线通知**。

4. **一次性理由自愈消费**：
   - Agent 收到服务端的 `RegisterResponse`（即鉴权注册成功）后，必须立即在本地状态文件中清空 `reconnect_reason`，确保后续运行过程中的异常掉线能恢复常规告警感知。

5. **版本号必须是构建产物，不能硬编码**：
   - Agent 上报的 `RegisterRequest.agent_version` 与控制端的 `CurrentAgentVersion` 一律取自 `internal/version`（`version.Get()`），发布时用 `make VERSION=v1.2.3 build-all` 注入（`-ldflags -X watchman/internal/version.Version=...`）。
   - 若两处都写死同一个常量（例如都是 `0.1.0-dev`），「哪些 Agent 需要升级」的判定恒为 false，批量升级与前端「需升级」提示会永久失效；此场景可用批量升级接口的 `force=true` 强制重推。
   - Agent 本地状态（含 `reconnect_reason`）只能经 `config.Update` / `SetReconnectReason` 读写（带互斥锁 + 临时文件原子落盘），升级回调、收包协程、注册协程会并发访问。

6. **广播维护通知不得持锁下发**：
   - `Registry.BroadcastMaintenance` 先在 `r.mu.RLock()` 内快照 hub 列表，解锁后并发发送，并用 `broadcastTimeout` 兜底；`Hub.Send` 单台最多阻塞 5s，持锁发送会拖垮 `ListAgents`/HTTP 请求与告警 tick。

### 8.7 异地组网（Tailscale/Headscale）实现约定

实现位置：`server/internal/network/`（handlers/store）+ `web/src/views/network/NetworkList.vue`。

1. **`--reset` 必须显式授权，严禁默认携带**：
   - `tailscale up --reset` 会丢弃本地节点状态并以**新节点身份**重新注册（新 node key），控制面（Headscale）会重新分配 100.x.y.z —— 这正是「重新组网后 IP 变化」的根因。
   - `joinNode` 仅在请求体 `reset: true`（前端弹窗默认关闭的开关）时追加 `--reset`；日常重连/失败重试绝不能带。
   - 加入成功后须回报 `ip` / `previous_ip` / `ip_changed`，前端在 IP 变化时给出醒目提示。

2. **下发到 Agent 的命令必须按主机 OS 选择 shell，严禁硬编码 `bash`**：
   - 统一使用 `agentShellFor(agent)`（Windows→powershell，Alpine/BusyBox→sh，其余→bash）；`runExec(hub, hostID, cmd, timeout)` 内部已按 hostID 解析。
   - 硬编码 bash 会让 Windows/Alpine 主机上所有探测（status/ping）立即失败，表现为「功能坏了」而非报错。

3. **连通性测速（`tailscale ping`）命令与解析约定**：
   - 命令必须加 `--until-direct=false --timeout=5s`（v1.24+ 默认 until-direct=true，会持续探测直到直连，导致命令长时间不退出）；目标值须经 `shellQuoteArg` 转义。
   - 解析须兼容新旧输出：`pong from X via DERP(tok) in Nms`（旧）、`pong from node (100.x.y.z) via 1.2.3.4:41641 in Nms` / `via direct in Nms`（新），取**最后一条** pong 行，latency 支持 ms 与 s 两种单位。
   - 命令因不认识的 flag 失败时（老版本），降级为 `tailscale ping -c N <target>` 重试。
   - 失败时返回 `hint`（`pingFailureHint`：no reply / unknown node / not logged in 等），前端展示诊断建议，不能只给一个空白结果框。
   - 「Ping 测速」入口在**未连接**分支也要渲染（Agent 在线即可测），否则弱网/掉线节点无诊断入口。

### 8.8 组件选用与页面布局进阶规范

1. **优先使用 Naive UI 原生组件，非必要不封装**：
   - 凡能由 Naive UI 官方组件（`NCard`, `NDataTable`, `NTabs`, `NTabPane`, `NSpace`, `NButton`, `NModal`, `NAlert`, `NInput` 等）满足的交互与视觉结构，**一律优先直接使用官方组件**，严禁在外部包裹多层无意义的自定义 `div` 模拟组件结构；
   - 杜绝低效套壳封装，非跨多页面特化的高复用业务逻辑（如终端 `TerminalPane` 等）严禁私自封装，统一由 Naive UI 原生组件绑定全局主题变量（如 `var(--bg-card)`）。

2. **页面主标题统一为外层单文字**：
   - 独立页面的主标题统一在视口顶部 `.page-header` 中使用单文字 `<h2 class="page-title">标题</h2>`，右侧配合 `NSpace` 放页面级全局操作；
   - 严禁图标与文字混排、严禁中英文杂糅（如消除括号英文）、严禁在标题下放置冗余副标题说明、严禁将页面主标题塞在卡片内置标题中。

3. **Tab 栏与表格卡片必须彻底分层**：
   - 以消息与告警中心（`AlertList.vue`）为基准拓扑，全局 Tab 栏（`<NTabs>`）必须外置在 `.tabs-container` 中，**严禁将 Tab 与表格混放在同一个卡片容器内**；
   - 各个 `NTabPane` 内部由 `.tab-pane-content` 垂直弹性承载，提示用 `NAlert`，表格独立包裹在 `<NCard :bordered="false" class="table-flex-fill">` 中，实现表头与分页条固钉、仅数据区滚动。

4. **表格卡片背景色与无边框规范**：
   - 所有承载表格的卡片容器背景统一绑定为 `var(--bg-card)`，杜绝因未定义 `--n-color` 导致回退到发灰的伪透明背景；
   - 消除表格外层硬编码的 `border: 1px solid var(--border-color)` 与内缩 `padding`，保持平整、沉浸的控制台无边框视觉。

### 8.9 顶部 Workspace 多页签拖拽排序

顶部多工作区页签（`AppShell.vue` 中的 `workspace-tab-item`）承载多任务多视图上下文，支持鼠标拖拽水平调整页签顺序：

1. **零第三方库原生拖拽**：使用 HTML5 Drag and Drop API（`draggable="true"`, `@dragstart`, `@dragover`, `@drop`, `@dragend`），不引入任何臃肿的外置拖拽包。
2. **位置动态感知与视觉指示线**：
   - 拖拽经过目标页签时，计算鼠标在元素内的水平相对偏移量（左半区 / 右半区），确定插入方位为 `left` 或 `right`；
   - 使用 CSS 伪类 `.drop-left::before` 与 `.drop-right::after` 渲染 3px 品牌色插入垂直指示线，定位在页签间隙中（带 `pointer-events: none`）；
   - 正在拖拽的源页签赋予 `.is-dragging` 状态（半透明 `opacity: 0.45` 与 `cursor: grabbing`）。
3. **状态同步与偏移校准**：
   - Pinia `workspace` store 提供 `moveTab(fromIndex, targetIndex, position)`，在数组切片 `splice` 移动时自动校准由于元素抽取导致的索引位移，实现无缝平滑重排；
   - 页签关闭按钮附加 `@mousedown.stop`，避免点击关闭图标时误触发父级拖拽。

### 8.10 RPC Hub 生命周期与锁纪律（告警引擎冻结事故复盘）

`server/internal/rpc/registry.go` 的 `Hub` 是所有 Agent 长连接的会话锚点，`unbind`/`Send`/`ReapStale` 的并发正确性直接决定告警引擎能否运转。曾因 `unbind` 在持有 `hub.mu` 时直接调用响应处理器回调，而处理器（metrics 自动采集）第一条语句就重入 `SetRespHandler` 注销自身——`sync.Mutex` 不可重入，`hub.mu` 被永久钉死，所有读者（`LastMetrics` → 告警 `Monitor.evaluate` → `ListHosts`）随之冻结，表现为「主机离线/上线没有任何通知」但 HTTP 接口照常响应。事故复盘出的铁律：

1. **持锁期间严禁调用外部回调**：`unbind` 必须先在 `hub.mu` 内快照并清空 `respHandlers`/`termHandlers`，解锁后再逐个以 `fn(nil)` 通知断连；任何处理器都可能重入 `SetRespHandler` 注销自己，持锁调用即自死锁。回调用 `nil` 入参表达「连接已断」，处理器必须容忍 `msg == nil`。
2. **关闭共享 channel 必须带 closed 守卫**：`sendCh` 由 `unbind` 关闭，`Send` 可能并发投递；先在锁内置 `closed = true` 再 `close(h.sendCh)`，`Send` 在锁内检查 `closed` 后再入队，否则「向已关闭 channel 发送」直接 panic。
3. **读谁的字段就持谁的锁**：`ReapStale` 遍历 hub 判定过期时，`lastSeen`/`heartbeat` 与 `handleAgentMessage` 的写侧并发，必须在 `hub.mu` 内读取（快照后解锁再处理），裸读是数据竞争。
4. **回归测试钉住死锁**：`registry_test.go` 的 `TestUnbindWithReentrantHandlerNoDeadlock`（处理器先注销自身再返回，`unbind` 5 秒内必须完成）与 `TestUnbindThenRebindChannelFreshness`（旧 hub 关闭后新 hub 可正常收发）任一失败即并发语义回归，禁止删除或放宽。
5. **诊断手段**：`kill -QUIT <pid>` 可让 Go runtime 把全量 goroutine 栈Dump 到 stderr（journald 可查），`[sync.Mutex.Lock, N minutes]` 即锁等待时长，是定位「接口活着但后台协程全冻结」类问题的首选；systemd 会自动拉起被 QUIT 杀掉的进程，生产可用。






