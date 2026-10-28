# 自研云堡垒机 / 主机管理助手 需求文档

> 参考对象：长亭百川云「牧云·主机管理助手 / 云堡垒机」
> 参考来源：<https://rivers.chaitin.cn/product/co1som8hp38s73e07jog> 及其官方文档 `/docs/zh/cloudwalker/{intro,function,start}`
> 背景：原 SaaS 服务说停就停、不提供服务，数据与可用性不可控，故自研一套可自部署的等价系统，核心目标是「自己掌控、永久可用、不依赖第三方 SaaS」。
>
> **必读 + 维护约定**：
> 1. 本文档**每次会话/每次任务开工前必须完整阅读**，并且**必须严格遵循其中的全部约束**（§7 代码质量校验、§8 前端布局规范、§8.5 终端断线重连、§8.6 维护握手与告警防抖等）。开工前先读、改代码时对照，不得凭印象或惯例行事；与文档冲突时以文档为准，若文档确实过时则先更新文档再改代码。
> 2. 本文档是需求与设计的唯一事实来源。**所有需求的改动和新增，实现代码的同时必须同步维护本文档**——涉及协议字段、数据模型、REST/WS 接口、配置项、前端布局与交互规范、构建与部署方式的，都要在对应章节补齐或更正，不允许只改代码不更新文档。
> 3. **版本唯一真相源**：**版本完全由官方 Release 提供，严禁任何自行决定或拼凑版本的行为**。系统版本、安装包与升级清单必须 100% 来源于 Git tag 触发 GitHub Actions 产出的官方 Release（形如 `v1.2.3` / `v1.2.3-beta.1`）及 CI 生成的 `manifest.json`；严禁在任何环境部署或升级流程中私自捏造/拼接版本号（如 `0.1.0-<hash>`）。

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
- **网络质量监控（三网延迟 / 丢包率）**：按**电信 / 联通 / 移动**三个运营商分别探测（键固定为 `telecom` / `unicom` / `mobile`，map 化设计为后续扩展第四线路留出空间）。Agent 每 30 秒对每个运营商的探测目标各发起一轮 **TCP 连接探测**（`net.DialTimeout`，4 次尝试、单次 3 秒超时、尝试间隔 200ms），`MetricsSample.probe_results`（field 26，`map<string, ProbeResult>`）上报各运营商最近一轮结果：`ProbeResult.latency_ms` 为成功尝试 RTT 的**中位数**（毫秒；0 = 该轮无成功尝试），`ProbeResult.loss_pct` 为失败尝试占比（0~100，连不上即失败）。探测在独立协程运行、不阻塞指标采样，采集时只读最近一轮结果；某运营商至少完成过一轮探测才上报对应条目。探测目标按主机在「财务与规格」以**三个可编辑输入框**配置（电信 / 联通 / 移动，值形如 `zj-ct-v4.ip.zstaticcdn.com:80`，留空 = 不探测该运营商），经 `RegisterResponse.probe_targets`（field 9，`map<string,string>`，键 = 运营商、值 = `host[:port]`，端口缺省 80）在每次（重）连时下发；面板保存「财务与规格」后经 `ServerMessage.config_update`（field 20，`ConfigUpdate.probe_targets`）对**在线 Agent 即时下发**（`Registry.PushRuntimeConfig`，保存即生效），离线 Agent 收不到、由下次重连时的 RegisterResponse 兜底；每台主机带独立开关 `probe_enabled`（默认关闭）：开启才下发目标并在资源监控展示两张图，关闭时下发空 map、Agent 停止探测、前端不展示。三个目标全部留空时使用内置默认 **Zstatic CDN 三网节点** `{省份}-{ct|cu|cm}-v4.ip.zstaticcdn.com:80`（ct=电信、cu=联通、cm=移动），省份按主机注册时的 `location`（如「浙江省-杭州市」→ `zj`，兼容省/市/自治区后缀与无分隔拼接）自动解析，无法识别回落北京 `bj`；服务端校验目标必须是 `host[:port]` 形式的 TCP 端点（端口 1~65535、主机名 ≤253 字符、拒绝 URL scheme 与空格、键限三个运营商）。控制端存储降采样时**按运营商分别**累加：延迟仅对有数据的样本（latency>0）求平均，丢包率对有该运营商条目的样本求平均。前端资源监控提供「网络延迟」「丢包率」两张曲线图，每张三线（电信 #3b82f6 / 联通 #f59e0b / 移动 #10b981），tooltip 一次展示三家，无探测数据时图内明示暂无数据。**兼容说明**：本改造移除了早期的单目标协议（`RegisterResponse.probe_url` field 8 与 `MetricsSample.net_latency_ms`/`net_loss_pct` field 24/25）；更早版本的 Agent 收不到 `probe_targets`（字段 8 已删、字段 9 被旧代码忽略）即停止探测，其旧字段被新服务端按未知字段跳过，新旧组合均不会报错，升级 Agent 后恢复三网探测。`ConfigUpdate`（2026-10-10 起）同理：旧 Agent 收到未知 oneof 字段静默忽略、行为退化为下次重连生效，新 Agent 连旧服务端则永远收不到该消息、同样靠重连兜底，双向组合均无错误。
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
  - `git`：链接 Git 仓库（GitHub 授权选择或手动 URL），Dockerfile / Compose 构建，支持推送自动部署（Webhook；GitHub App 路径见下条）。
- **GitHub App 自动部署（2026-10-26，github.com 主推路径，逐仓 Webhook 降级为备选）**：
  - **一次安装覆盖全部/部分仓库**：面板「系统设置 → 代码源集成」新建 GitHub App（Manifest 两步流：填名称 + 目标（个人/组织）→ 跳 GitHub 创建 → 回调换 code（1 小时内，state 防 CSRF，state 嵌在 redirect_url 的 `github_app_state` 参数里带回——submit URL 的 query GitHub 不保证透传）→ 配置落盘；或手动填已有 App 的 App ID/私钥/Webhook Secret）→ 到 GitHub 安装页选账号/组织与仓库范围 → 面板「同步安装」拉取安装与仓库清单。
  - **事件入口**：公开端点 `POST /api/v1/github-app/webhook`（HMAC-SHA256 验签、恒时比较、`X-GitHub-Delivery` 去重、body 4MB 上限；未配置 503；ping 200；仅 `refs/heads/` 分支推送触发，tag 推送跳过；push 按 `repository.full_name` 归一化匹配应用，分支匹配 + auto_deploy 门 + 在途 409，复用 `StartDeployment`，`trigger=github-app`；`installation`/`installation_repositories` 事件同步安装状态与仓库清单，卸载则相关应用降级提示；安装事件的仓库清单刷新走异步，upsert 本身同步落盘，持久化失败返回 5xx 供 GitHub 重投）。安全入口开启时该端点仍可达（secentry 豁免）。
  - **部署取数优先级**：应用 vault 凭据 > installation token（现取现用、内存缓存 1h、过期前 5 分钟刷新；仓库不在安装范围时全量刷新后重试）> 现有 provider token。**刷新合并语义**：部分安装刷新失败时保留其旧缓存（只丢 GitHub 侧已不存在的安装），失败数如实返回调用方；配置变更/断开时清空内存 token 缓存。
  - **凭据存储**：App ID/私钥/Webhook Secret 存 `github_app.json`（dataDir、0600、tmp+rename 原子写、API 永不回显），**不用 Vault**——Vault 是用户自管凭据的 UI，程序性塞入系统私钥反而会暴露在 Vault 界面；与 `git_provider.json` 同款先例。
  - **前端**：GitProviders.vue GitHub App 分区（创建/手动/安装/同步/断开、事件地址复制，同步请求超时 60s）；AppDetail 三种自动部署状态（GitHub App 托管 / GitHub 自动托管 / 手动模式）+「切换到 GitHub App」按钮（切换前置条件：App 已配置且仓库在未暂停安装覆盖内，否则 409 并引导先安装/同步；切换时后端删旧逐仓 hook，失败 502 不改库）；AppCreate 仓库下拉含 App 安装仓库（标 `[GitHub App]`，分支走 App 令牌拉取）；App 来源应用默认 `auto_deploy=true`。
  - **前提与边界**：面板必须有 GitHub 可达的公网地址（webhook_url 由 `server.public_url` 或请求 Host 推导；public_url 未配时创建页 warning）；内网面板不在本方案范围内，需轮询兜底（未实现）。权限最小化：`contents:read` + `metadata:read`，只订阅 push；v1 不含 PR 预览与 Watch paths。
  - **审计**：`app_github_app_switch`（medium）/ `github_app_manifest`（low）/ `github_app_config`（high）/ `github_app_disconnect`（high）/ `github_app_install_sync`（medium）。
- **集成组网端口转发（4 层 TCP 流量中继，2026-10-16）**：
  - 组网转发彻底从独立的假应用收敛为**具体业务应用的内置 4 层网络中继能力**（与 7 层 HTTP/HTTPS 域名反代并列）。
  - 用户在部署或维护任意具体应用（模板/镜像/Compose/Git）时，可开启「组网端口转发」，指定**入口主机**（`mesh_forward_host_id`，如带公网 IP 的跳板机/网关节点，不能是应用主机自身）、**入口监听端口**（`mesh_forward_entry_port`，1~65535）与**目标端口**（`mesh_forward_target_port`，缺省预填应用暴露端口）。
  - **部署引擎自动联动**：主应用在应用主机成功部署后，引擎自动在入口主机调度运行 companion `alpine/socat` 容器（`watchman-fwd-<appID>`，`--entrypoint /usr/bin/socat`，`TCP-LISTEN:<entryPort>,fork,reuseaddr TCP:<targetIP>:<targetPort>`）。
  - **双路径自动解析**：应用主机与入口主机双方均在异地组网内时，流量经 Tailscale 组网直接指向应用主机 Mesh IP（`100.x:targetPort`，应用主机无需公网 IP）；若未组网则平滑回退直连路径（应用主机公网 IP 或内网 IP）。
  - **全生命周期协同**：应用的主机启停（`stop`/`start`/`restart`）及彻底删除（`purge`）自动协同操作入口主机上的中继容器；关闭转发或修改配置时自动平滑更新或清理旧中继容器（2026-10-19 加固）：
    - **创建与更新同一套校验**（`validateMeshForward`）：开启转发时入口主机必填、入口主机不能是应用主机自身、入口/目标端口均须 1~65535；更新时应用主机不可变更，故以存储的 `host_id` 为准校验。
    - **（入口主机， 入口端口）全应用唯一**（`meshForwardConflict`）：创建与更新均校验（更新排除自身），冲突返回 409 并提示占用方应用名。
    - **入口主机变更/关闭转发即拆除旧中继**：入口主机变更时先按旧坐标 `CleanMeshForwarder`（新表单已清空旧字段，不清理会遗留孤儿容器）；purge 以**记录过入口主机**为准（而非仅看 Enabled 开关），历史遗留配置仍可被回收。
    - **更新响应同步化**：`PUT /apps/:id` 在转发配置变化时同步执行 `SyncMeshForwarder`，结果经响应字段 `mesh_forward_target`（生效的中继目标）/ `mesh_forward_warning`（同步失败原因）返回，不再只依赖异步协程。
    - **中继清理失败不再静默（2026-10-25）**：`CleanMeshForwarder`/`SyncMeshForwarder` 改为返回 error；更新时入口主机变更或禁用转发，先预清理旧中继，失败则 409 且不落库（旧坐标得以保留，杜绝孤儿中继）；部署成功路径的中继失败仍只记 WARN（P2，不在本轮）。
    - **部署完成重读存储**：`finishDeploySuccess` 创建中继前重新 `GetApp`，应用在部署期间被删除则跳过并在部署日志说明（消除用过期指针建中继的竞态）。
    - **启停联动结果可观测**：`containerAction`（stop/start/restart）对中继容器的联动失败（入口主机离线/执行失败/非零退出）以响应字段 `relay_warning` 上抛，主操作仍按自身结果返回。
  - **拓扑预览协同**：创建页与详情页的流量拓扑实时呈现 TCP 4 层中继链路与 7 层 HTTP/HTTPS 反代链路。
- **端口绑定范围**（`PortMapping.bind_scope`）：每条端口映射可选「公网开放」（默认，`-p host:container` 绑定 0.0.0.0）或「仅异地组网」（`-p <tailscaleIP>:host:container` 只绑定主机 Tailscale IP，公网不可达）。选择「仅异地组网」时目标主机必须已加入组网，引擎找不到 Mesh IP 时降级为公网绑定并在部署日志中告警。
- **域名与反代集成**（`PUT /apps/:id/proxy`）：
  - **系统托管反代容器（2026-10-20，对齐 1Panel OpenResty / dokploy-traefik 的面板托管反代模式）**：反代 Nginx 不要求宿主机安装任何 nginx 服务（宝塔式宿主安装污染系统、跨发行版负担，不采纳），而是由系统在绑定域名时于目标主机**自动初始化并持续保障**专用伴生容器 `watchman-proxy-nginx`（`nginx:alpine`，`--restart unless-stopped --network host --label watchman.managed=proxy`，挂载 `/opt/apps/nginx/conf.d:/etc/nginx/conf.d:ro` 与 `/opt/apps/nginx/certs:/opt/apps/nginx/certs:ro`，两个宿主目录由保障脚本 `mkdir -p` 自建，不假定运维手工准备）。命名**刻意脱离 `watchman-app-*` 用户应用命名空间**（防用户部署同名 nginx 应用劫持/冲突，与 dokploy-traefik 保留名同哲学）。`ensureProxyNginx` 幂等保障：先跑 30s 检查脚本（mkdir 两目录 + `command -v docker` + `docker info` + `docker inspect` 容器状态与两挂载），容器运行中且挂载齐全则跳过重建（秒回、不动用户在线服务）；缺失/停止/挂载不符/用户手工改坏 → 执行 300s 重建脚本（`docker rm -f` 吞不存在 → `docker run` 重建 → sleep 2 验 running，起后即退则抓 `docker logs --tail 30` 分类），与 socat 中继容器同一套幂等替换先例（`server/internal/apps/proxy.go`）。bind 在目标主机 hub 解析后、**证书解析前**执行 ensure（基础设施坏了快速失败，不浪费同步 ACME 签发）；ensure 与文件推送/续期回推共用 `pushMu` 全串行（重建不与在途 reload 竞态），但**不跨证书解析持锁**（慢 ACME 不阻塞他人）。`RepushAppCerts` 系统动作**不做 ensure**（保持轻量；容器被手动删除由下次用户绑定自愈）。**`--network host` 的因果**：local 模式上游是 `127.0.0.1:<port>`，bridge 网络下容器内 127.0.0.1 指向容器自身（流量黑洞），host 网络下语义正确且 80/443 直监听宿主（无 `-p` 映射需维护）。**错误分类**（`classifyEnsureError`，纯字符串表驱动可测）：Docker 不可用 → 409「一键安装或启动 Docker」；80/443 被占（nginx 日志含 `address already in use`）→ 409；`nginx:alpine` 拉取失败 → 409「配置镜像加速」；其余 → 500 + 日志摘要（≤200 字符截断），绝不静默吞错。**Windows 主机前置 400**（「反代绑定暂仅支持 Linux 主机」）：保障脚本假定 Linux 宿主，明确拒绝而非在 docker exec 深处晦涩失败。vhost 443 块含 `client_max_body_size 64m;`（nginx 默认 1m，走反代的上传会被 413 静默截断）。**保留 nginx 而非 Traefik 的取舍**：gateway 模式上游是另一台主机的 mesh/公网 IP（Dokploy 的 docker.sock label 发现无法表达跨主机）；nginx 容器零 docker.sock 攻击面；证书中心是唯一证书真相源（ACME/续期/回推闭环，不建第二套）；conf.d 文件推送 + agent reload 与既有实现零迁移成本。
  - `local` 模式：应用所在主机运行系统托管反代容器，上游 `127.0.0.1:<port>`。
  - `gateway` 模式：另选一台有公网 IP 的网关主机运行 Nginx，公网流量在网关卸载 TLS 后转发到应用主机。**异地组网不是必需条件**，上游按两条路径自动解析（`defaultUpstream`）：
    - **组网路径**：应用主机与网关主机**双方都已在组网内** → 上游取应用主机的 Tailscale Mesh IP（100.64.0.0/10），应用主机无需公网 IP、可隐藏在 NAT 后；
    - **直连路径**：否则 → 上游取应用主机的**公网 IP**，无公网 IP 再回退**内网 IP**（同局域网/VPC 场景），仅两者皆无时才报错并要求在详情页手动填写 Upstream。
    - 上游永不用 `127.0.0.1`（会指向网关自身造成流量黑洞）；网关主机**不能是应用主机自身**（相同场景即 local 模式）。
  - **多域名与证书显式绑定（2026-10-23，彻底移除自动匹配与隐式自动签发）**：
    - **契约重构（`ProxyDomains []DomainBinding`）**：`Application.Domain` 单值字段升级为 `ProxyDomains []DomainBinding`（`{domain, cert_id}`），一个应用支持绑定多个独立域名，且每个域名独立选定证书（通配符证书与多厂商单域名证书可混用）。
    - **显式证书绑定与严格校验**：`PUT /apps/:id/proxy` 全量替换语义接收 `domains: [{domain, cert_id}]`（兼容无 domains 时的旧单值形式）。服务端废弃按域名 `FindByDomain` 隐式自动匹配与 `GetDefaultAccount`+`Issue` 自动签发兜底，强制逐域名校验：合法域名格式、应用内域名去重、跨应用域名全局唯一（409）、证书存在性（400）与 **SAN 覆盖域名（`cert.DomainCovers`，未覆盖报 400）**、证书材料完整性（409）。无证书时明确报错引导「先到证书中心签发或上传」。
    - **多域名 Nginx Virtual Host 渲染**：统一生成至单文件 `watchman-<appID>.conf`，为每个域名独立渲染 HTTP 80 重定向块与 HTTPS 443 反代 server 块，各自引用专用证书与密钥文件（`/opt/apps/nginx/certs/<domain>.crt` 与 `<domain>.key`）。解绑应用反代或彻底删除应用时，联动清理全部绑定域名的证书与配置文件。
    - **身份优先证书回推（Identity-First Re-push）**：证书自动化续期成功后，`ProxyHandler.RepushAppCerts` 遍历各应用，优先匹配 `binding.CertID == crt.ID`（或存量未物化空 CertID 条目且域名覆盖）；更新并热重载 Nginx，杜绝多厂商/新签证书跨应用漂移劫持。
    - **确定性最佳证书匹配（Deterministic SAN Ranking）**：`FindByDomain` 改为绝对确定性排序：精确 SAN 匹配 > 通配符覆盖；通配符按标签深度（label count）多者优先；再按剩余有效期 `NotAfter` 新者优先；证书 ID 字典序作为确定性 tie-breaker。驱动覆盖探测端点、存量条目解析与面板 SNI 降级。`storeResult` 保存续期证书时严格保留管理员既有 `Remark`。
    - **证书删除守卫与强制清绑（Delete Guard & Force Override）**：`DELETE /certs/:id` 检查证书是否正被任何应用的 `ProxyDomains` 显式引用；若有引用则返回 409 Conflict 并列出占用方应用名称，防止误删生产在线证书；支持传入 `?force=true` 强制删除，删除时通过回调解耦将相关应用的绑定 `cert_id` 安全置空（降级存量语义，不破坏正在运行的 Nginx 实例）并完成删除。**面板守卫（2026-10-25）**：面板显式绑定的证书（`panelCertInfo` 返回的 `SSLCertID`）另受面板守卫，一律 409，`?force=true` **不可绕过**——面板绑定只能从「系统设置 → 面板域名与证书」解除后才能删除（cert 包内无安全的反向解绑入口，放行 force 会回到删证后面板静默换证）。**守卫触发条件收紧（2026-10-25）**：仅当面板 SSL 启用且证书来源模式为 `cert_center` 时拦截；切到 custom PEM 或关闭 SSL 后残留的 `SSLCertID` 不再拦删除（面板此时并不实际服务该证书）。残留风险：面板 SSL 开启但未显式绑定时，面板实际服务的是 `FindByDomain(panelDomain)` 或 `list[0]` 的证书，删掉那张证书不会被守卫拦截（回退链本身未收紧，属下一批）。
    - **存量数据平滑迁移与物化冻结**：Store 加载 `apps.json` 时自动将历史 `Domain` 字段迁移为 `ProxyDomains=[{domain: Domain, cert_id: ""}]`；存量空 `cert_id` 条目在首次证书回推或迁移同步时，由 `BestCertForDomain` 自动解析并**立即物化冻结回写**到应用元数据，实现无缝平滑升级。
    - **前端多域名行式编辑器与交互**：`AppDetail.vue` 与 `AppCreate.vue` 均升级为多域名动态行式编辑器（域名输入 + 独立证书选择 NSelect + 删行 + 加行），证书选项显式格式化为 `SAN 列表 · 颁发者 · 剩余天数`，彻底移除「自动匹配」虚假占位。创建页在域名输入时防抖 500ms 探测覆盖，已覆盖时可见地自动预选；存在未选择证书域名时引导 ACME 签发并显式绑定新证书 ID。`AppMigrateModal` 与 `AppTopologyPreview` 支持多域名展示与拓扑节点证书状态联动。
  - 网关主机 ID 持久化在 `Application.proxy_gateway_host_id`，解绑时同时清理应用主机与网关主机两侧配置，并清空该字段（2026-10-20 修复：此前解绑残留旧网关 ID 的脏字段）。
  - **反代加固（2026-10-25）**：手动 `upstream` 实行严格白名单（仅 IP/严格主机名 + 1~65535 端口，拒绝空格与 `;{}#$/?@` 及缺失端口，非法 400）；`SyncProxyForHost` 两分支改用存量 upstream（空值/非法值回退默认计算）与存量 `ProxyWebSocket`（不再硬编码 true），网关模式下存量 upstream 若仍指向旧应用主机则重新计算（防迁移后网关指回旧主机，旧主机离线无法确认时按手动值保留）；`Application.ProxyWebSocket` 为 `*bool`——nil 表示未记录选择（存量记录、未传该字段的绑定），渲染时等同 true（与改动前恒 true 一致），仅显式 false 关闭，杜绝存量应用迁移后静默丢失 Upgrade 头。`SyncProxyForHost` 网关分支补齐 Windows 拒绝、`ensureProxyNginx` 与逐域名证书推送（与 local 分支对称）；绑定失败时补偿删除本次写入的 conf 与已推证书并再 reload（破损 conf 不再滞留 `conf.d` 毒化后续 `nginx -t`，但写 conf 之前失败时不删已推的新证书——旧 conf 仍引用旧证书路径，删了反而让旧 conf 引用缺失文件）；换绑时清理旧目标主机的旧 conf 与被剔除域名的证书，清理失败进响应 `warnings`（首绑跳过旧目标清理）；解绑时离线主机/exec 失败/reload 非零各进 `warnings`，`UpdateApp` 失败改返 500（此前静默丢弃却报成功），解绑同时清 `ProxyWebSocket`；绑定/解绑成功响应新增可选 `warnings` 字段。**补偿双守卫与上游手动标志（2026-10-25）**：`pushVhost` 的 `confWritten` 只在写 conf 成功后才置位（此前先置位，写失败时补偿会删掉完好的旧 conf 与已推新证书）；`nginx -t` 失败时补偿只在 `-t` 输出归因到本应用时触发（提及本应用 conf 文件名或本次推送的证书文件——证书缺失时 -t 报的是证书路径而非 conf，必须算归因），输出未提及本应用时保留本次 conf+证书并记日志（超时等无法归因的失败保持原有补偿行为）；手动 `upstream` 端口串必须全数字（`+8080` 此前被 `Atoi` 放过、到 nginx 才拒收，现直接 400）。新增 `Application.ProxyUpstreamManual`：`bindProxy` 在请求带 `upstream` 时置 true、无 upstream 的重绑清零、解绑清零；`SyncProxyForHost` 网关分支对 manual 记录直接沿用存量、跳过旧主机启发式重算（此前手动值撞旧主机+首发布端口会被当计算值覆盖），存量未标记记录继续走既有启发式，local 分支恒沿用不变。
- **创建时证书配置与签发编排**（2026-10-20，2026-10-23 多域名升级）：
  - **覆盖探测端点**：新只读端点 `GET /api/v1/certs/cover?domain=<d>`（登录即可、免操作审计，与 detail/download 同例）复用 `hub.FindByDomain` 确定性同源匹配（精确 SAN 或通配符覆盖子域名、不含通配符裸域名本身），响应 `{covered, cert?{id,domains,source,account_name,not_after}}`（PEM 经 `json:"-"` 永不出网），前端永不复刻匹配规则、预检与最终绑定不会分叉；域名非法/空/通配符 → 400（校验语义与反代 `validProxyDomain` 镜像一致）。
  - **创建页多域名证书区**（`AppCreate.vue`）：支持配置多条域名记录，每行域名输入防抖 ~500ms 实时调覆盖端点——已覆盖 → 可见地自动预选该证书（用户可手动切换选择其他覆盖证书）；未覆盖 → 给出未覆盖提示。若存在尚未选定证书的域名，激活下方签发配置区：选择 ACME 账户（预填默认账户）、challenge 单选（自动推荐 / HTTP-01 / DNS-01）；无账户 → 提示并深链 `/certs`；选 DNS-01 且 dns-mng 未配置（`enabled && base_url` 同款判定）→ NAlert + 深链。**手动 DNS-01 两阶段不进创建页**（深链证书中心策略）：通配符域名提示「先到证书中心手动 DNS-01 签发后回来选择」+ 深链。探测失败静默降级（提示条注明不影响创建），手动流程不受影响。
  - **提交编排**（`bindDomainsWithCert`）：创建应用成功后，遍历待绑定域名列表——已显式选定证书的直接保留 `cert_id`；未显式选定证书且有 ACME 账户的，按域名逐个 `POST /certs/issue`（202 异步受理，携带所选账户与 challenge）→ 轮询覆盖端点 5s×24 次（2 分钟）获取新证书 ID 并显式赋予该域名；全部域名解析出 `cert_id` 后调用 `bindProxy` 下发多域名绑定与 Nginx 反代配置。签发超时/受理失败时提示并跳过绑定（不回滚应用创建、不阻断跳转，引导到详情页重试）。轮询为有界 await 循环（无 setInterval 遗留），视图卸载提前终止；创建按钮 loading 文案阶段化「签发证书 (domain)… / 绑定反代中…」。**逐域处理（2026-10-25）**：改为逐域收集 `ready`/`failed`——已有证书的直接进待绑定集，无证书且无账户、签发受理失败、轮询超时只记入失败清单并继续下一域名；最后仅对已备妥的域名调一次 `bindProxy`，失败清单汇总为一条 warning（注明各域名原因、引导到详情页重试），不再任一域名失败即整批 return。
  - **拓扑协同**：`AppTopologyPreview` 接收 `:domains` 与 `:cert-status`（多域名综合状态），Nginx/SSL 节点实时展示「已有证书 / 提交时签发 / 指定证书」小标签（探测中/失败/未填域名不展示）。
  - **AI 一致性**：AI 草稿仅填充首行域名、**不新增任何证书字段**（证书选择与 `auth_vault_id` 同理必须人工选）；`sanitizeAppDraft` 现状已不含证书字段，仅在此注明边界。
- **创建页异地组网防错与拓扑预览**（2026-10-16）：
  - **网关主机约束**（前端禁用 + 提交校验 + 后端兜底）：网关主机**不能与应用主机相同**（相同场景即 local）。**异地组网非必需**：网关到应用主机可走组网（双方都在组网内 → 100.x）或直连（应用主机公网 → 内网 IP），下拉按「有公网 IP」优先排序并标注 `组网直达`/`直连` 路径，未组网主机不再禁选。后端 `bindProxy`：同主机返回 400；两条路径都无法解析出上游时返回 409 并提示手动填写 Upstream。
  - **组网 IP 实时探测**：选中主机即调只读端点 `GET /network/nodes/:id/mesh-ip`（§8.7 第 8 条），提示区分「实时探测/缓存」；mesh 端口但主机未组网时，端口区给出 NAlert 与「前往加入组网」深链（`/hosts/:id?tab=network`），不再等到提交才报错。
  - **组网全局未配置引导**：创建页拉取 `GET /network/config`，未配置 Auth Key/控制面时顶部 NAlert 提示并深链 `/network`（可关闭）。
  - **local 模式与主机公网能力不匹配**：填写域名后若 local 模式但应用主机无公网 IP，NAlert 建议切「统一网关」并提供一键切换。
  - **流量拓扑预览卡片**（`web/src/components/apps/AppTopologyPreview.vue`，创建页选中主机后渲染）：随表单实时展示 公网用户 → 网关主机(Nginx/443) → （`Tailscale 组网` 或 `直连`）→ 应用主机 → 应用容器 的链路与每条端口的绑定范围（`0.0.0.0:host` 公网开放 / `meshIP:host` 仅异地组网），节点离线时降亮；链路箭头的「组网/直连」由双方是否都有组网 IP 决定。视觉语言复用 AppDetail「域名与反代」页 topology-box（§8.8 组件规范）。
  - **AppDetail 域名与反代同步**：网关下拉同样标注组网状态与 `组网直达`/`直连` 路径、仅禁用应用主机自身；保存前校验网关非应用主机自身（两路径均由后端解析，不强制组网）；已绑定应用的拓扑展示按 `proxy_upstream` 是否落在 100.64.0.0/10 判定并显示「Tailscale 组网」或「直连」。
  - **AI 草稿一致性**：`app_assist` 系统规则第 11 条与对话规则要求 `gateway_host_id` 为在线主机、不能等于 `host_id`（网关可走组网或直连，不强制组网）；`sanitizeAppDraft` 仅在同主机时丢弃并告警，未组网时追加「将按直连转发，请确认网络可达」提示而不丢弃。
- **创建页 AI 辅助**（2026-10-10）：创建应用页（`AppCreate.vue`）右侧挂载 AI 部署助手（`web/src/components/apps/AppAiAssistant.vue`），复用统一 LLM 适配层（§3.16），提供两种能力：
  - **智能填充**：用户用自然语言描述需求，`POST /api/v1/ai/app-draft` 返回结构化表单草稿（`Assistant.AppDraft`，实现位于 `server/internal/ai/app_assist.go`）。服务端把在线主机清单（仅 id/主机名/系统/公网IP/组网IP）与模板目录（含 `env_fields`）作为接地上下文注入提示词；模型只能从清单中选 `host_id`/`template_id`，不得输出 `auth_vault_id`（Git 凭据必须人工选）。返回前经 `sanitizeAppDraft` 消毒（来源类型白名单、端口 1~65535、模板参数按声明字段过滤、`bind_scope=mesh` 与主机组网IP 一致性告警等），并计算 `missing`（还需用户补充的字段）；前端点「填充到表单」才合并进表单，**AI 永不直接创建应用**，最终提交仍走原有已审计的 `POST /apps` 与前端校验（§3.16.7 草稿-确认模型）。
  - **对话辅助**：`POST /api/v1/ai/app-chat` 每次请求携带**当前表单快照**与最近 10 轮对话，回答可附带 `form_patch`（结构化改动，同样经消毒），前端以「建议修改」卡片呈现，用户点「应用到表单」才生效，禁止静默改表。
  - **脱敏**：表单快照送模型前在服务端脱敏（`redactFormForLLM`）：环境变量值、模板密钥参数（`is_secret`）一律掩码为 `***`（只给键名与已填标记），Compose 内容中疑似口令行掩码，`auth_vault_id` 清空；不信任前端脱敏。AI 未启用/未配置时右侧面板明示并可跳转系统设置，手动填表完全不受影响（§3.16.1 降级原则）。两个接口均不改变系统状态，属 AI 诊断类，按 §7 豁免操作审计（与 `ai/nl2command` 同例）。
- **镜像/仓库真实元数据探测（免拉取，防 AI 臆造端口）**（2026-10-14）：面对任意镜像引用（如 `ghcr.io/xerxes-2/clewdr`）或 GitHub 仓库，系统在生成草稿/填表前先**客观读取**目标的真实配置，彻底取代凭先验知识猜测：
  - **镜像探测**（`server/internal/registry`，新包，纯标准库无 SDK）：走 Docker Registry V2 / OCI 协议——`GET /v2/<repo>/manifests/<tag>`（Accept 声明 manifest v2/list + OCI manifest/index）→ 401 时按 `Www-Authenticate` 握手匿名 Bearer token（仅 pull scope）→ manifest list 自动选 `linux/amd64`（跳过 attestation 伪条目）→ 跟进 30x 读 config blob（上限 1MB）抽取 `ExposedPorts/EnvVars/Volumes/Cmd/Entrypoint`。**免拉取镜像层**，秒级往返；SSRF 防护拒绝解析为环回/私网地址的 registry；registry 传输不可达时回退 Docker Hub 镜像 `docker.m.daocloud.io` 重试一次。默认单请求超时 12s（ghcr.io 的 config blob 会 302 到 CDN）。
  - **仓库探测**（`server/internal/gitprovider/inspect.go`）：不 clone，`GET /repos/<o>/<r>` 取默认分支，再按 `docker-compose.yml`/`compose.yaml` → `Dockerfile`/`docker/Dockerfile` → `.env.example` → 语言栈标记的优先级梯队拉取并解析：compose 经 yaml 解析 `services.*.ports/environment`（原文上限 64KB），Dockerfile 行级正则解析 `EXPOSE/ENV/VOLUME/ENTRYPOINT/CMD`；仅支持 github.com（公开仓库匿名、私有仓库用已接入的 provider token），非 GitHub 地址返回明确的「暂不支持探测」而非报错。
  - **AI 管线集成**（`server/internal/ai/app_facts.go`）：智能填充/对话辅助在调用模型**前**，从需求文本与表单快照扫描镜像引用（`registry.LooksLikeImageRef`，排除时间串/URL 等误报）与 `github.com/<o>/<r>` URL 并探测（镜像 6s / 仓库 8s ctx 上限，5 分钟内存缓存容量 64）；命中则把 `【已客观读取的真实配置】`事实区块（含真实端口、变量名、构建方式，密钥类变量值掩码 `***`）置顶注入 prompt 并附「事实优先级最高」规则。`sanitizeAppDraft` 再做**确定性兜底**（`alignDraftWithFacts`）：草稿端口与真实暴露端口不符时自动对齐/补齐并向 `warnings` 追加中文说明；仓库仅检出 Dockerfile 而草稿选了 compose（或反之）时自动纠正 `build_type` 与路径；探测失败不改行为、仅在 warnings 提醒核实。模型输出始终只是草稿，创建仍走 `POST /apps` 审计链路。
  - **表单手动触发**：创建页「镜像地址」失焦/`Enter` 自动探测并展示紧凑提示条（「已客观读取：端口 8484 · 预置变量 N 项」）+「按真实配置填入」一键带入（仅替换未填的端口占位/空的环境变量与默认健康检查，不覆盖用户已填值）；Git 来源仓库失焦探测展示「默认分支 / Dockerfile（EXPOSE x）/ compose / 语言栈」徽章，并在用户未手动改动构建方式时自动对齐 `build_type`、分支与端口预填。探测失败静默降级为提示条文字（「（不影响手动填写）」），不打断手动流程。单请求超时放宽为镜像 20s / 仓库 30s（必须覆盖服务端 15s/20s 的 ctx 预算，短了会把慢响应误报为失败）。接口为只读、无状态，按 §7 豁免操作审计。**角色与限速（2026-10-25）**：两探测端点改为仅 admin/operator 可用（viewer 调用 403——此前所有登录角色可用，且仓库探测无条件取用管理员存量 GitHub token，可读私有仓库完整 compose 与 env 值），每用户每分钟 30 次限速（超限 429，viewer 被拒不消耗配额）。**registry SSRF 纵深（2026-10-25）**：拨号层改为解析后校验公网 IP（拒绝环回/私网/链路本地/CGNAT/保留段，混合解析 fail-closed，拨号直连校验过的 IP 以关闭 DNS 重绑定窗口，TLS ServerName 仍取 URL 主机名）；重定向目标与 token realm 主机走同一套校验（ghcr blob 302 到 CDN 只要解析为公网 IP 即放行）；上游非 2xx 错误不再回显响应体（仅状态码）。拒绝清单另补 `0.0.0.0/8`（`0.1.2.3` 类此前可过，Linux 上 0.x 常路由本机）与 `198.18.0.0/15`（RFC 2544 benchmarking 段）。收紧注意：此前可用「解析到内网 IP 的自建 registry 主机名」探测，现会被拨号层拒绝（探测不影响部署本身）。
- **系统设置面板安全与应用反代职责彻底解耦**（2026-09-28）：
  - 应用中心独立负责业务容器的反向代理与域名绑定（在应用详情 `AppDetail.vue` 的「域名与反代」页签中配置，经 Nginx 容器路由流量到应用容器或网关节点）；
  - 「系统设置 → 面板域名与证书」（`web/src/views/settings/CertDomain.vue`，`sections.ts` 注册 `key: 'cert-domain'`，`adminOnly` 仅管理员可见）**专用于 Watchman 控制台/面板自身的访问与安全配置**，与应用反向代理彻底解耦，采用宝塔式简化模型。**域名绑定**：`security.panel_domain` 为空表示通过 IP 访问；一旦绑定域名即**自动启用严格域名限制**（无独立开关），面板只能通过该域名访问，直接 IP/其他域名请求被 403 拦截（回环地址与 Agent 注册/数据接口等基础设施路径豁免）。**面板证书**：已绑定域名时「为绑定域名申请免费证书」（经证书中心 ACME 签发：普通域名优先 HTTP-01，需服务器 80 端口可被 CA 访问、签发时临时监听；80 端口不可用或 HTTP-01 验证失败时自动回退 DNS-01，需预先在证书中心配置 ACME 账户与 dns-mng；通配符域名仅支持 DNS-01；另支持「DNS-01（手动解析）」两阶段签发——服务端创建订单并展示 TXT 记录，管理员手动到 DNS 服务商添加解析后确认，CA 验证通过后自动绑定，不依赖 dns-mng）；未绑定域名时「为 IP 申请免费证书」（为公网 IP 签发 Let's Encrypt 等免费 IP 证书，走 HTTP-01 验证，签发期间临时监听 80 端口，证书有效期短、自动续期阈值按有效期 1/3 自适应，基准天数可在「系统设置 → 证书时间设置」中配置 `certs.auto_renew_days`）；签发成功后自动写入证书中心并绑定为面板证书、开启 HTTPS。另支持手动上传已有证书（先导入证书中心并校验、私钥不再直存 settings，再以 cert_center 模式绑定）与「强制 HTTPS 访问」（`security.panel_force_https`，明文 HTTP 自动 301 重定向至 HTTPS）。
  - 面板服务采用**端口级协议动态嗅探（Dynamic Listener）**技术，在控制台默认端口（18789）上原生自适应处理 HTTP 明文与 TLS 握手，热重载或开关 SSL 无需重启服务或更改端口。
  - **防失联与紧急恢复**：若配置错误域名或无效证书导致无法登录，提供控制机命令行救援开关：`watchman-server -reset-panel-domain -data <数据目录>`（解绑面板域名；绑定即严格，解绑后恢复 IP 访问）以及 `watchman-server -reset-panel-ssl -data <数据目录>`（关闭面板 SSL 与强制 HTTPS 重定向）。
  - 独立「证书中心」（`/certs`，`CertList.vue` + `CertManager.vue`）保持作为全局基础设施，管理 ACME 自动化证书与证书凭证，供面板及各业务应用按需选用；另提供只读覆盖探测端点 `GET /certs/cover?domain=`（登录即可、免操作审计，与 `FindByDomain` 同源匹配，见 §3.7「创建时证书配置与签发编排」）。已签发证书列表每行提供「详情」弹窗：只读端点 `GET /certs/:id/detail`（登录即可、免操作审计）对存储的证书链现场解析 leaf（序列号、SHA-256 指纹、密钥算法/长度、签名算法、链张数，解析失败降级为基本元信息 + `parse_error`），并反查用途（面板证书显式绑定或按 `panel_domain` 兜底匹配 `panel_bound`/`panel_fallback`、应用按域名经 `hub.FindByDomain` 同源规则匹配 `used_by_apps`）及来源推断（`source` = acme / manual_dns01 / manual_upload）；弹窗内可展开查看并复制证书链 PEM 与私钥（2026-10-20 起「证书材料」区并列展示）：私钥明文经显式查看端点 `POST /certs/:id/key` 返回（adminOnly + 审计 `cert_key_reveal`/high，与 AI API Key 明文查看同惯例；用 POST 是因为共享审计中间件跳过 GET），打开详情弹窗时随证书链一并拉取、每次查看各落一条审计；常规响应（列表/单查/详情/下载）仍一律剥离私钥，该端点是唯一出口。证书列表「操作」列提供「下载证书」按钮：`GET /certs/:id/download`（登录即可、只读免操作审计、浏览器导航下载、JWT 走 `?token=` 查询参数——与审计导出/备份下载同惯例，鉴权中间件为无法带 header 的导航请求提供查询参数回退）以附件返回存储的公钥链，文件名取主域名 `<域名>.crt`（通配符 `*` 置换为 `_wildcard`、其余文件名不安全字符折叠为 `_`、无域名回退证书 id），下载仅公钥链、不打包私钥（私钥查看/复制走上述显式端点）。操作列「续期」按钮仅对有 ACME 账户的证书展示：手动上传（`manual_upload`）的证书无账户，服务端续期会回落到默认账户重新签发并覆盖上传材料，其续命方式应为「重新上传」，故不展示续期按钮（`manual_dns01` 两阶段证书有账户，保留续期）。（2026-10-20 详情弹窗视觉打磨）弹窗为 `NModal preset="card"` 自定义头部：主域名 + 状态标签一行可读；内容按「指纹与算法 / 有效期与续期 / 使用情况」三区展示（分区标题 13px 加品牌色强调条；原第四区「基本信息」2026-10-20 移除——来源/颁发机构/备注/SAN/最近错误与列表列及弹窗头部全重复，失败原因全文已由日志列悬停弹层承载），序列号与 SHA-256 指纹行内提供一键复制，有效期以进度条呈现（颜色随剩余天数分档：>30 天绿 / >7 天琥珀 / 其余红），PEM 折叠区冠以与三区同一视觉语言的「证书材料」分区标题（2026-10-20 补充，自渲染节点复刻 13px + 品牌色竖条样式），等宽字体 + 主题变量底色；内容区 `maxHeight calc(100vh - 120px)` 内部滚动、头部固定。注意 scoped CSS 锚定：`NModal` teleport 到 body 后卡片根节点不带组件 `data-v` 作用域，`:deep()` 必须锚定在模板内自渲染的 `.cert-detail-body` 包裹层上才能命中 Naive 内部节点。（2026-10-20）已签发证书列表为宝塔式 11 列：域名 / 其他域名 / 申请方式 / ACME 账号 / 状态 / 日志 / 颁发组织 / 备注 / 自动续签 / 过期时间 / 操作；其中「申请方式」「ACME 账号」由服务端在列表响应中派生（`source`/`account_name`，与 detail 端点同一推断规则，前端不再自行推断），「日志」列为最近一次失败原因 `last_error` 摘要（暂无逐次签发日志），悬停单元格以 `NPopover` 限宽弹层展示全文（小标题「最近一次失败原因」+ 12px 小字、`break-all` 换行、内容区 `max-width: 340px`、`max-height: 200px` 超长内部滚动，单元格内自行省略截断）——不用内置省略 tooltip（对长错误实测单行铺出 1088px 宽、溢出屏幕），「状态」在续期中/异常之外按有效期给出已过期/未生效。每张证书带管理员备注字段 `Certificate.remark`（≤200 字，仅元信息，不影响证书）；行内铅笔按钮弹层编辑，经 `PUT /certs/:id/remark` 保存（adminOnly + 审计 `cert_remark`/medium，2026-10-25 补齐此前缺失的审计规则，前端 `audit.ts` 标签同批补齐，另含 `cert_key_reveal`、`ai_config_key_reveal` 两处同类缺口；列表/详情/导入/签发等响应不再回显私钥的既有约束不变）。（2026-10-20）
- **全生命周期管理**：部署历史、构建日志、健康检查探活失败自动保留旧版本（`-next`/`-prev` 滚动替换）、回滚、启停、AI 排障诊断，对所有来源的应用一致生效。
  - **部署取消与强制重新部署（2026-10-21）**：
    - **取消执行（Context 级全链路熔断）**：活跃部署由 `Engine` 记录其 `context.CancelFunc`、Agent 通道与正在构建的替换容器名（`-next`）。调用 `CancelDeployment` 时立即触发 context 取消，各阶段边界（代码拉取、Docker 构建、启动新容器、健康探活、swap 切换前）检测 `ctx.Done()` 并提前熔断；
    - **安全清理（零停机保障）**：若新容器（`watchman-app-<appID>-next`）已被拉起，取消时异步调用 `removeContainer` 仅清理半拉起容器，在线服务容器全程不受影响；部署记录状态置为 `cancelled`，记录 `CancelledBy` 操作员与 `FinishedAt`；
    - **强制重新部署（`force=true`）**：在已有部署执行时，`POST /apps/:id/deploy` 支持 `force=true`（Query 或 JSON Body），服务端自动先终止当前在途任务、释放槽位并立即启动全新部署，前端弹窗确认「当前已有部署任务正在进行中，继续将终止当前任务并启动全新部署」；
    - **历史记录与多点控制**：顶部操作栏展示醒目「终止部署」按钮与二次确认；部署历史列表针对 `queued`/`building`/`deploying` 记录提供行内「停止」按钮；部署状态 Tag 增加「已取消」（warning 琥珀色），提交信息列展示取消人。
    - **取消与终态加固（2026-10-25）**：Compose 部署取消时自动 `compose down` 清理已上线栈（raw_compose 与 git-compose 两分支；清理失败时记录 Error 明确标注栈仍在运行，请手动 down，禁止静默记干净取消）；部署记录终态守卫——success/failed/cancelled 一旦写入，任何后续阶段/取消/成功路径不得重写（此前探活取消可被 fail 覆盖丢 `CancelledBy`、swap 后取消可翻 success）；`-next` 容器名带 depID 后缀（`watchman-app-<id>-next-dep_<8字符>`，新旧两 run 容器名必然不同，旧管线清理删不掉新容器，purge 与迁移清理同步覆盖新旧两种命名）；swap 前与 `finishDeploySuccess` 入口查应用存活，已删则删 `-next` 并 fail（不建中继）；`deployApp` 补 engine nil 守卫（503）。
  - **部署 Docker 前置检查（2026-10-26）**：管线在 Agent 在线校验通过后、拉取代码/构建之前，先对目标主机执行 Docker 可用性检查（`command -v docker` 与 `docker info` 两段、退出码区分，复用反代保障的分类口径）：未安装 → 快速失败「目标主机未安装 Docker：请先在主机详情 Docker 页一键安装 Docker 后重试」；daemon 未起 → 「目标主机 Docker 服务未运行：请先启动 Docker 服务后重试」；检查本身传输失败（Agent 断连/exec 超时）不归类为 Docker 问题。覆盖全部来源（git/镜像/模板/Compose 终点都是 docker），此前缺失该检查时，无 Docker 主机会一路跑到起容器才以裸 `docker: command not found`（exit 127）失败。创建即部署同样走该检查，失败时经 `deploy_warning` 带出同一文案。不覆盖：`docker compose` 插件缺失（`docker info` 通过但 compose 子命令不存在）仍在 Compose 步骤以原错误失败，属后续缺口。**前端安装提醒（2026-10-26）**：光给文字提示不够便捷，`AppDetail.vue` 在最新一次部署死于“未安装 Docker”时给两条路——详情页眉与正文之间的常驻警告横幅（一键安装 Docker 按钮）与本页会话内发起的部署刚失败时的自动弹窗（问是否安装，安装中禁关、装完给「立即重新部署」）；安装走现有 `POST /hosts/:id/docker/install-script`（服务端内置官方脚本，前端 330 秒超时）。识别靠部署记录 `error` 含固定文案“未安装 Docker”（记录无 error_code 字段，改后端文案须同步前端）；提醒状态取最新一条记录（单独拉第 1 页第 1 条，不依赖历史页签当前页码）；部署结束（部署中→非部署中）时补刷一次历史与最新记录，否则 1 秒内落终态的快速失败永远刷不出来。
  - **创建即部署（2026-10-21）**：`POST /apps` 持久化成功后立即 `engine.StartDeployment(app, "create", …)` 入队首轮部署（`Deployment.trigger` 新增 `create`，详情页展示为「创建」），响应附 `deployment`；引擎忙/启动失败不失败创建本身，仅附 `deploy_warning`（应用记录合法，可手动重试）。前端创建页提示「已创建，正在部署…」，详情页既有轮询自然展示 排队→构建→发布→成功/失败 与日志。
  - **应用列表（2026-10-21）**：列为 应用名称 / 服务器 / 来源 / 当前版本 / 状态 / 最近部署 / 最近更新 / 操作。「服务器」由前端并行 `listHosts()` 按 `host_id → hostname` 映射（`/apps` 不 join 主机，主机接口失败不阻断应用列表）；「来源」替代原只对 Git 有意义的 `repo_url @ branch` 列（git → 仓库+自动部署标记、image → 镜像、template → 模板、raw_compose → Docker Compose）；「最近更新」取 `Application.updated_at`。右上角入口文案「创建应用」（原「链接 Git 应用」）。
  - **`updated_at` 语义（2026-10-21）**：`Application.UpdatedAt` 在任何持久化变更时推进，写入点唯一——`Store.UpdateApp` 内统一打点（覆盖配置编辑、部署成功/失败收尾、反代绑定/解绑、组网中继同步），`PutApp` 在空值时以 `CreatedAt` 播种；时间戳单调（`nextUpdatedAt`，时钟回拨不回退）。历史 `apps.json` 缺该字段时，加载期回填 `max(CreatedAt, LastDeployAt)`，老应用不显示空白。
  - **`PUT /apps/:id` 为部分更新（2026-10-21）**：改用独立 `updateAppReq`，全部可变字段为 `*T` 指针——未提交的键保持原值，显式 `false`/`""`/`{}` 才清空。此前复用 `createAppReq` 会无条件覆盖 `auto_deploy`/`auth_vault_id`/`healthcheck_url`/`mesh_forward_*`，导致「只保存环境变量」或「只保存 Compose」静默清空这些配置（已修复）。组网转发按「存储值 + 本次改动」的有效配置校验，且仅当请求触及任一 `mesh_forward_*` 字段时才校验（无关编辑不会因入口主机离线而失败）；`mesh_forward_enabled=false` 时坐标清空。**镜像地址仅 `source_type=image` 可改**（模板镜像由模板解析、Git/Compose 走各自构建产物，其余来源 400）；`host_id`/`source_type`/`container_name` 不可改。前端 `AppDetail.vue` 概览页签「编辑配置」弹窗（名称/镜像/健康检查/端口映射/挂载卷，仅提交差异字段，已部署应用可「保存并重新部署」），环境变量页签改为行式编辑器（支持 `{{ vault:… }}` 原文引用、模板来源提示变量会被模板参数重渲染）；**值首尾成对的引号在展示时剥除**（`syncEnvRows` 经 `stripEnvQuotes`），口径与后端解析 `.env.example` / Dockerfile `ENV`（`parseEnvFile`/`parseEnvPairs` 的 `strings.Trim(v, "'\"")`）一致——`.env` 风格粘贴（`KEY="value"`）带进来的引号不属于值本身，不应在编辑器里占位；无引号时不改动值（不清洗两侧空格）。仅在载入表单时剥除，用户重新输入并保存的引号仍按原文入库。**显式清空真正生效（2026-10-25）**：`dockerfile`/`build_context`/`build_timeout_sec`/`compose_content`/`build_type` 五字段的显式空值此前被非空守卫静默忽略（文档写「显式清空」而代码不清空），现已改为指针非 nil 即赋值（`build_timeout_sec` 显式 0 入库后由管线按 900 默认执行）。
  - **挂载卷备份与恢复（2026-10-22）**：应用声明的挂载卷（`Application.Volumes`，`host:container[:ro]`）此前只在 purge 时被区分对待（bind mount 绝不删），既不能备份也不能找回，本条目补齐卷数据生命周期。实现位于 `server/internal/apps/volbackup.go`（引擎与 `VolumeStore`）与 `handlers_volbackup.go`（HTTP 面）：
    - **卷形态覆盖**：bind mount（`/opt/apps/xxx/data:/data`）直接 tar 宿主机路径；Docker 具名卷（`volname:/data`）先 `docker volume inspect --format '{{.Mountpoint}}'` 解析挂载点再 tar（与 purge.go「src 含 `/` 即宿主机目录」的区分规则同源）。`parseVolumeSpec` 按 `:` 切分并把容器路径前的段落重新 join，兼容 Windows 盘符路径（`C:\data:/data`）；`..` 穿越与空路径一律拒绝。
    - **归档直传 S3（唯一存储位置，主机不留归档）**：归档统一写入快照备份模块的 S3 存储目标（详情页手动备份在页内**选择目标**（Dokploy 式，2026-10-26 起：目标列表走 s3-targets 接口、默认目标预选、无默认时须显式选、未知目标 400；省略 `target_id` 时回落默认目标以兼容旧调用），快照 `app_volume` 任务可选默认或指定 S3 目标；均经 `snapshots.Store` 的 S3 目标表解析，`Engine.SetS3TargetStore` 在 `main.go` 注入）；无可用目标时备份前置 400 并引导「快照备份 → S3 存储」，绝不回退宿主本地落盘。流程遵循 direct-to-storage 原则（GB 级归档过控制端会打满上行带宽）：主机只做临时暂存——先 tar 到 `/var/backups/watchman/apps/<appID>/<id>.tar.gz`，再 `curl -X PUT` 到服务端预签名的 PUT URL（2 小时有效），上传结束（无论成败）立即删除暂存文件。对象键为 `<目标前缀>/apps/<appID>/<archiveID>.tar.gz`，由 `S3Client.ObjectKey` 拼装。元数据（`VolumeArchive`：`volume_spec`/`host_path`/`container_path`/`storage_target`/`storage_target_name`/`storage_path`（完整对象键）/`size`/`sha256`/`status`/`note`/`last_restore_*` 等）落控制端 `app_volume_archives.json`，原子全量重写（记录会 running→success/failed 迁移，非追加型 JSONL），每应用上限 100 条——内存与落盘同步裁剪（`saveLocked` 溢出即从索引删除），控制端启动时把残留的 `running` 记录对账为 `failed`（「控制端重启，任务已中断」）并落盘，避免 UI 永久转圈。`storage_target`/`storage_target_name` 在备份时**冻结**：切换默认目标后旧归档仍按其记录的 S3 目标下载/恢复/删除，不会被孤立。备份失败会尽力删除 S3 上可能已部分上传的对象（DELETE 对 404 视为成功）并清理宿主暂存。**产物标记强制（2026-10-25）**：tar 成功后若 SIZE/SHA256 未回传（size=0 或 sha 为空），归档记 failed，不再产生 success 记录（空卷 tar 仍有固定大小，不会误杀）；恢复拒绝 SHA256 为空的 success 归档（停机前拒绝，提示重新备份），存量脏记录不自动改 failed（S3 字节本身可能完好，仅拒绝恢复、由操作员决定是否删除）。
    - **保留策略**：手动备份每卷保留最新 5 份**成功**归档（`volumeRetention`；快照 `app_volume` 任务按任务配置的保留份数经 `keep` 传入，两者按「应用 + 卷规格」共享同一份归档集合；失败记录不占配额、也绝不挤掉可恢复归档），超出后先删 S3 对象与宿主暂存/安全副本，再删元数据；S3 删除瞬时失败（网络/5xx）时**保留元数据**，避免对象还在桶里却失去索引（与快照 engine 默认保留同款语义）；归档记录的 S3 目标已被删除（`errVolumeS3TargetMissing`）时对象不可达，直接丢弃元数据并记日志。`S3Client.DeleteObject` 对非 2xx/404 返回错误（此前静默成功会让元数据先丢而对象残留）。
    - **恢复语义（下载校验→安全停机→安全副本→解压→重启）**：先校验归档记录完整（`storage_path`/`host_path`）、状态为 `success`、目标 S3 仍存在；用 2 小时预签名 GET 把对象下载到主机暂存路径（`<id>.restore.tar.gz`），再 `sha256sum` 与记录的 `sha256` 比对——**下载/校验失败不产生任何停机**。校验通过后 `docker inspect -f '{{.State.Running}}'` 判断容器运行态，运行中先 `docker stop`（stop 失败即中止恢复，绝不在线覆盖运行中的数据，杜绝文件锁与写盘竞态），恢复脚本先做 `<id>.pre-restore.tar.gz` 安全副本（尽力而为，空目录不阻断）再 `tar -xzf` 覆盖，进程内 `defer` 保证无论恢复成败都把容器拉回原状态（先停必起），并清理下载的暂存文件。**注意**：该保证以控制端请求协程存活为前提——控制端在恢复中途崩溃/被强杀时容器会停留在停止态，需人工 `docker start`；恢复结果（`last_restore_at`/`last_restore_status`/`last_restore_error`）无论成败都写回归档记录，并在详情页「最近恢复」列展示，超时窗口后仍可回溯。**恢复前置卷规格校验（2026-10-25）**：归档卷规格须仍在应用声明中（trim 后比对），bind 卷要求 `host_path` 等于当前声明 src 且容器路径一致，具名卷重解析 mountpoint 比对，不一致拒绝并提示「卷配置已变更，请重新备份」（校验在下载/停机之前；快照历史同步预检无归档上下文，规格已变的恢复在协程内被拒并经 defer 写回 `last_restore_*`）。恢复的 SHA 校验改为无条件执行（此前仅 SHA 非空时校验）。归档清理（`PurgeVolumeArchives`）由调用方 purge 全程持槽保障，函数本身不再重复抢槽（槽不可重入，自抢槽恒失败会致归档清理被跳过，属 2026-10-25 收口修正）。
    - **同应用串行**：`volMu/volBusy` 按 `appID` 互斥，第二个备份/恢复/删除请求返回 409「该应用已有备份或恢复任务在执行」——两条 agent 脚本交错操作同一数据目录只会互相污染。**部署互斥双向生效**：部署启动前后各查一次卷忙槽（`errVolumeOpInProgress`），卷操作持有槽后再查 `IsRunning`，任一方向命中即拒绝，且两次检查不嵌套持锁；purge 同样先抢占卷槽，避免删除应用与在途备份/恢复互相踩踏。
    - **边界约束**：未选目标且无默认 S3 目标时备份 400（列表响应仍返回 `s3` 默认目标块供展示；按钮改由页内所选目标门控，无目标时警告并深链 `/snapshots`）；Compose 来源（`raw_compose` / git+compose）的卷在编排 YAML 内声明、记录不持有，明确 400 拒绝并引导「快照备份」；Windows 主机前置 400（脚本为 POSIX `tar`/`set -e` 语义，与反代绑定的 Windows 前置拒绝同款因果）；备份只接受应用已声明的卷规格（防备份链路被用于打包任意宿主机目录）；备注上限 200 字符（按 rune 计，超限 400）；恢复要求应用已部署（有 `ContainerName`）；`running` 归档禁止删除（409），失败/中断归档不可用于恢复或下载（400）。**一致性提示**：备份为在线文件级快照（不冻结写入），数据库类卷应停写后备份或在应用侧导出，恢复会覆盖归档时间点之后的所有数据。
    - **下载与入口**：新增只读端点 `GET /api/v1/apps/:id/volume-backups/:archiveID/download`：校验应用/归档存在且状态为 `success` 后 **302 到 15 分钟预签名 S3 GET**（目标已删除返回 410），字节由 S3 直达浏览器、不经控制端中转；前端 `volumeArchiveDownloadUrl` 拼 `?token=` 走导航下载。详情页「数据卷备份」页签：顶部展示 S3 目标状态（无目标时警告并深链 `/snapshots`）；应用挂载卷清单表 + 备份范围多选/备份位置选择（默认目标预选）/备注（输入框带 200 字计数）+ 备份任务表（本应用 app_volume 任务的新建/编辑/删除/立即执行/启用开关，2026-10-26，复用快照模块 `/backups/jobs` 接口）+ 上传备份并恢复区块（选卷 + 选文件 + 可选 SHA，2026-10-26，见恢复语义后一 bullet）+ 归档历史表（时间/卷/存储位置（S3 目标与对象键）/大小/状态/最近恢复/备注/错误/SHA256 截断，操作列恢复（NPopconfirm 高危文案明示自动停机重启）、下载、删除）。**归档表视觉约定（2026-10-22 打磨）**：三个子区块（应用挂载卷 / 发起备份 / 备份归档）共用同一 `.vol-section-title` 层级，归档标题右侧常驻「刷新」按钮（`volumeListLoading` 驱动）；时间列按「日期 / 时刻」两行排版（日期 12px 弱化、时刻等宽）；存储位置以弱化目标名 + `· ` + 等宽对象键单行呈现，超出由列 `ellipsis tooltip` 悬停给全文；`render` 生成的单元格拿不到本组件 scoped 作用域，等宽一律用全局 `.mono-font`（`global.scss`）而非组件内 `.mono`，其余单元格样式写内联 `style`；大小/SHA256 空值用 `—` 而非 `-`，失败归档不显示 0 字节；SHA256 截断 12 位并配「复制完整 SHA256」文本按钮；「最近恢复」成功/失败用 NTag + 等宽时间，失败原因仅在失败行追加第二行小字（成功行保持单行高，列表可扫读）；操作列 `fixed: 'right'` 三按钮带图标（恢复/下载/删除），整表 `scroll-x` 1240 兜住窄视口；分页沿用 `useTablePagination`，仅归档超过 10 行时出现分页条（挂载卷表因数量天然有限，属 §8.2 例外不分页）。轮询采用**有界窗口**（有 `running` 归档或本地正处于恢复中时续期至 30 分钟、页面激活时续 60 秒），不会因单条脏记录永久 5s 轮询；恢复完成以 `last_restore_at` 变化判定并解除按钮 loading。**前端打磨（2026-10-27）**：数据卷备份页签四个子区块标题统一为 28px 行高 + 底部分隔线；发起备份与上传恢复的并排输入改 `.vol-form-row`（flex + 换行、最小宽 240px，窄屏自动单列，备注/SHA 不再固定 320px）；顶部提示的快照深链不再夹在句子中间，改为段末独立按钮「前往快照备份」；备份任务弹窗 Cron/保留份数改 `.vol-modal-row`（Cron 占满、保留 140px 固定）。快照页聚合视图切换改 `NRadioButton` 分段控件（与 MetricsPane/AlertList 同款），切换栏左侧加聚合说明，组卡片内容区 padding 归零使表格与标题对齐，平铺表补 NEmpty 空态。行为与接口均未变。
    - **上传备份并恢复（2026-10-26）**：详情页「数据卷备份」新增「上传备份并恢复」区块：选已声明挂载卷 + 本地 `.tar.gz` 文件 + 可选 SHA256，`POST /api/v1/apps/:id/volume-backups/upload-restore`（multipart，202 受理；文件上限 2GB，超限 413）。浏览器到不了被管主机，文件先落面板 `dataDir/volume-uploads/<archiveID>.tar.gz`（0600），同时建一条 `Status=uploaded`、`Source=upload` 的归档记录（无 S3 对象，状态终身不变、恢复结果只写 `last_restore_*`；下载/恢复按钮按 `success` 门控，对上传记录天然不可见）；面板签发一次性下载 token（2 小时有效、用后失效），Agent 用现有下载脚本从面板回拉到主机 staging（下载地址按上传请求的 scheme+Host 拼装，Agent 须能通过面板地址回拉，拉取失败在停机前即报错、不产生停机；安全入口豁免 `/api/v1/volume-uploads/` 前缀，否则启用入口会 404 掉 Agent 下载）。执行复用恢复核心（`runRestoreCore`）：卷声明校验（bind 比对宿主路径、具名卷上传时先解析挂载点落记录、恢复时重解析比对）、部署互斥、SHA 有填才验（无 SHA 时 `tar -tzf` 探查格式，坏包停机前即拒）、停机→安全副本→解包→重启、终态回写；临时文件与 token 在恢复终态后删除（无论成败，失败重传再恢复）。删除上传记录只删记录与面板临时文件（无 S3 对象可删）；应用 purge 连带清临时文件与 token；面板重启清 `volume-uploads` 残留（在途恢复随进程死亡，残留文件不可能再被合法下载）。审计 `app_volume_restore_upload`（high）。**边界**：一次只恢复一个卷；上传文件不长期存档、不转存 S3；恢复仍要求应用已部署（有 ContainerName）。
    - **审计与互斥**：备份/恢复/删除归档均为 admin+operator 写操作并落审计——`app_volume_backup`（medium）/`app_volume_restore`（high，恢复覆盖在线数据）/`app_volume_backup_delete`（high）；归档列表与下载是只读、免审计。HTTP 面预检顺序为「卷忙（409）→ 部署中（409）→ Agent 离线（503）」并在备份入口先解析目标（显式优先、缺省回落默认，400）再受理，卷槽在请求内**同步抢占**（失败即返回），协程结束 `defer` 释放，消除双击/重试造成的 TOCTOU；仅调用无槽的 `run*` 引擎方法。快照任务历史的 app_volume 恢复入口（`POST /backups/jobs/:id/archives/:archiveID/restore`）同契约：受理前先同步 `CheckAppVolumeRestore` 预检（卷忙/部署中 409、Agent 离线 503、其余前置失败 400），拒绝即返回、不再「202 已开始」后静默落日志；预检与协程执行之间的竞态（状态在两步之间变化）由 `RestoreAppVolumeByID` 兜底并把失败写回 `last_restore_*`，保证用户总能从归档记录看到恢复失败原因。同时补齐了上轮遗漏的 `POST /apps/:id/deployments/cancel` 审计条目（`app_deploy_cancel`/high）。应用删除（purge）会遍历删除该应用全部 S3 归档对象（逐个尝试，目标缺失记日志）、清理宿主暂存目录并删除全部元数据（尽力而为，失败计入 warnings），并连带删除绑定该应用的 `app_volume` 快照任务（见下条）。
    - **与快照备份统一调度（2026-10-22）**：应用数据卷备份不再只是详情页的手动动作，快照备份模块新增一等任务类型 `app_volume`（`snapshots.KindAppVolume`）：任务定义新增 `Job.AppID` 绑定应用，`HostID` 由应用推导（创建时不要求用户选主机）。职责边界——快照模块持有任务定义、定时调度（复用 `cron.go` 每分钟捡拾）、保留份数、统一归档历史；应用引擎持有执行语义（声明卷白名单、S3-only、SHA 校验、停机恢复）。两者经 `snapshots.AppVolumeHost` 接口解耦（`apps` 已 import `snapshots`，反向 import 会成环）：接口声明在 `snapshots`，由 `apps.Engine` 实现，`main.go` 中 `snapshotEngine.SetAppVolumeHost(appEngine)` 注入；恢复预检 `CheckAppVolumeRestore(appID)` 不做任何操作、只报告「为什么现在不能恢复」，可重试冲突返回 `snapshots.ErrVolumeRestore{Busy,Deploying,Offline}` 哨兵错误供 HTTP 层映射 409/503。同一份 `VolumeArchive` 记录同时出现在应用详情「数据卷备份」与快照任务「历史归档」：定时/手动任务产生的归档可在详情页下载/恢复，详情页手动备份的归档也出现在对应任务历史；快照归档响应为 app_volume 携带派生的 `storage_target_name`/`status`/`error`/`note`/`last_restore_*`。保留策略按「应用 + 卷规格」共享，任务保留份数经 `runBackupAppVolumesWith(..., keep)` 传入（缺省 `volumeRetention`=5，失败记录不占配额）。**保留份数语义提示**：手动备份固定 `keep=5`、快照任务按任务配置（默认 7），作用于同一份归档集合——**谁后跑谁定上限**（手动备份会把任务保留的 6/7 份裁回 5 份）；需要稳定保留 N 份时请把两边设为一致，或以其中一边为唯一调度入口。任务 PUT 更新为**部分更新**（`updateJobReq` 全字段 `*T` 指针，与 `PUT /apps/:id` 同款契约）：未提交的键保持原值，显式 `""`/`0`/`false` 才写入（`cron` 空串=仅手动、`db_name` 空串=全部库）；`target`/`storage_target` 以合并后的有效值过 `resolveAppVolumeJob` 白名单校验，不再因省略而清空或误报 400（前端契约 `Partial<CreateJobReq>` 与此一致）。创建/更新任务时 `resolveAppVolumeJob` 实时校验：应用存在、非 Compose、非 Windows、卷在声明白名单内、存储目标为存在的 S3 目标（明确拒绝 `local`，仅 `default` 或具体 S3）；运行时 `RunAppVolumeBackup` 再次解析应用与白名单，卷被移除或应用已消失则本轮任务失败并写 `LastError`。**purge 删除应用即连带清理任务**：`apps.Engine.PurgeVolumeArchives` 先调 `snapshots.Store.DeleteJobsByApp`（按 `Kind==app_volume && AppID` 精确匹配、原子重写 `snapshots.json`、返回删除条数记 info 日志）再清归档，避免 cron 每分钟对着已消失的应用反复失败并追加失败记录（`dir`/`volume`/`database` 任务即使带 `AppID` 也绝不受影响，测试 `snapshots/store_test.go` 锁住）；`apps` 对快照的反向依赖同样走窄接口 `apps.SnapshotJobStore` + `SetSnapshotJobStore`，在 `main.go` 装配。快照历史里的恢复/删除委托 `RestoreAppVolumeByID`/`DeleteAppVolumeByID`（与应用级 `RestoreAppVolume`/`DeleteAppVolumeArchive` 区分命名），恢复语义与详情页一致（停机→校验 SHA256→安全副本→解压→重启）；`running` 归档禁止删除（409）、非 `success` 禁止恢复（400）。前端「新建备份任务」选择「应用数据卷」后展示应用/挂载卷下拉（仅列出已声明挂载卷的非 Compose 应用），存储位置只列 S3 目标（无默认目标时警示并引导去「S3 异地存储配置」）；任务表「目标对象」展示应用名 + 卷规格，归档弹窗展示状态/最近恢复/错误并按状态禁用恢复/删除；应用详情页「数据卷备份」新增「备份任务」区块（2026-10-26）：本应用的 app_volume 任务在详情页直接新建/编辑/删除/立即执行/开关启用，复用快照模块 `/backups/jobs` 接口（任务定义与调度仍归快照模块，执行与归档仍走应用引擎；任务列表前端按 `app_id` 过滤，不新增接口）；快照页任务表新增「按应用聚合」视图（app_volume 按应用分组折叠，其他种类平铺，默认聚合视图，可切回平铺）。备份日志 = 归档表（每次运行一条，含状态/错误）+ 任务行的最近执行/上次错误（逐行 tar/上传输出服务端不落盘，不在日志范围内）。
  - **服务迁移（应用跨主机迁移，2026-10-23）**：把一个已部署的应用从当前主机整体搬到另一台主机，可选携带声明的挂载卷数据，并同步重定向反代与组网转发；实施落在 `server/internal/apps/migrate.go`（引擎）与 `handlers_app.go`（HTTP 面）。四条硬约束：
    - **契约**：`POST /api/v1/apps/:id/migrate`（写组 + 审计 `app_migrate`/high）受理 `MigrateAppReq{target_host_id, migrate_volumes, clean_source}`，**同步校验后 202 返回 `Deployment`**（`trigger="migrate"`），进度与日志沿用部署历史轮询（前端迁移成功后自动切到「部署历史」页签）；`POST /api/v1/apps/:id/migrate/precheck`（读组、免审计、只读）返回 `MigratePrecheckResp{target_host_id, agent_online, docker_running, volume_count, s3_ready, s3_bucket, port_conflicts, warnings}`，**一切问题都只以 `warnings` 上报、不阻断**（操作员仍可决定），供弹窗选中目标主机后实时展示 Agent/Docker/S3/端口占用四张状态卡。
    - **同步前置校验**（`StartMigration`，失败即 HTTP 409、不产生排队任务）：目标主机非空且不等于当前主机；目标 Agent 在线；目标**非 Windows**（迁移管线是 POSIX `tar`/`set -e` 语义，与反代绑定同款前置拒绝）；应用无在途部署（`IsRunning`）与卷操作（`VolumeBusy`）；`migrate_volumes=true` 且应用有卷时必须已配置默认 S3 目标（否则 409「数据卷迁移需要 S3 存储目标」），并**同步抢占卷槽**（`acquireVolumeSlot`，协程 `defer` 释放）——迁移与备份/恢复共用同一把按应用的互斥。**2026-10-25 新增**：源主机 Agent 在线校验（离线 409「源主机 Agent 不在线」）与源非 Windows 校验（409）；此前从未部署过的应用若源离线也无法再受理迁移，属行为收紧。
    - **六阶段执行与自动回滚**（`runMigration`）：① 源容器先停（迁移期不写入，保证 tar 快照一致）；② 逐卷 `tar.gz` → 预签名 PUT 直传 S3 → 目标主机预签名 GET 下载 → **SHA256 逐卷比对** → 解压到目标（bind mount 直接 tar 宿主路径，具名卷先 `docker volume inspect` 解析挂载点，与 §3.7 卷归档同一套脚本契约；暂存文件与 S3 中转对象在每卷结束或失败时立即清理）；③ 落库切换 `HostID` 并 `SyncProxyForHost`（local 模式在目标主机 `ensureProxyNginx` + 推证书/vhost + reload 并清理源主机旧 vhost；gateway 模式向网关重新推上游）与 `syncMeshForwarder`（socat 中继改指目标主机 Mesh IP/直连 IP）；④ 按四种 `source_type` 在目标主机完整构建 + 拉起 + 健康探活；⑤ 可选清理源主机容器（**bind mount 宿主目录严格保留**，绝不删除）；⑥ `finishDeploySuccess` 收尾。**任一步失败均调用 `rollback`**：恢复 `HostID=oldHostID`、反代与组网转发指回源主机、源容器重新 `start`（Compose 则 `compose up -d`）、清理目标主机残留容器——服务绝不会停在半迁移状态；日志全程写入 `build_log` 并以 `[ROLLBACK]` 标记回滚过程。**2026-10-25 失败语义收紧**：阶段 3 反代/组网同步失败改为 fail + rollback（此前仅 WARN 继续并打「迁移完成」，域名会指向已停的源容器，违反本节「任一步失败均回滚」）；阶段 1 停源失败（transport 错误或非零退出码）同样 fail + rollback；阶段 2 全部失败点（卷目录解析、tar、预签 PUT/GET、上传/下载、SHA 校验、解压）统一 fail + rollback；阶段 3 前取消改为先回滚再置取消（此前只置取消不回滚）；Compose 迁移探活改为仅 `HealthcheckURL` 非空时做 HTTP 探活，无 URL 不探活（此前无 URL 时必败回滚）；回滚卷清理改为仅删本 run 解压的文件清单（解析 `tar -tzf` 成员，跳过目录/`..`/绝对路径，绝不清整个目标目录、不删具名卷；解压本身仍是 overlay，备份后新增文件在回滚后仍保留——与「恢复覆盖归档时间点之后所有数据」的表述仍有偏差，文档待二选一澄清）；回滚失败时记录 Error 追加「回滚未完成，需人工检查源主机容器与反代状态」后缀并记 server Error 日志（告警中心/webhook 未接线，属已知缺口）；阶段边界存活检查：开局、阶段 3/4/5 之前查应用存活，已删则尽力清目标残留并 fail；Stage 5 git-compose 清理改为 `compose down`（原 `docker rm -f <container> <container>-next` 对 `<project>-<service>-1` 容器名无效，源栈会继续运行）。**2026-10-25 第二批收紧**：阶段 3 之后取消同样走取消回滚（阶段 3 失败分支、阶段 3 成功后、阶段 4 入口见终态时各补一次 `cancelled()` 检查；此前阶段 3 内 HostID 已切、反代/组网已指目标、源容器已停，取消落在此窗口会静默中断服务）；迁移管线全部 compose 命令统一带 `-p watchman-<appID>`（回滚清理、阶段 5 源清理、abortIfGone 清理、阶段 1 停源、回滚重启——此前 bare 命令命中目录派生项目名为空操作，目标/源栈双活残留）；阶段 1 状态探测 exec 失败 fail-closed（此前静默跳过停源，随后 tar 可能拍到在线写入的卷）；阶段 2 源 SHA 为空即 fail（此前跳过校验）；回滚卷清理看退出码（`set -e` 脚本中途 abort 不再静默通过）；abortIfGone 对 Compose 应用改用带 `-p` 的 compose down 并尽力清理本 run 已解压的卷文件；迁移开局 Building 写走终态守卫；已删应用部署路径的 compose down 失败不再静默，失败信息进记录 Error 与构建日志。
    - **边界**：Compose 来源的应用卷在编排 YAML 内声明、记录不持有，卷迁移无意义（前端对 Compose 关闭该开关并说明）；预检额外提示两类拓扑冲突——组网转发入口主机恰为目标主机（迁移后入口不再可行）、网关模式网关主机恰为目标主机（上游会指向自身）；`clean_source` 不勾选时源容器保持停止状态作为冷备，可再迁回。
    - **删除互斥（2026-10-25）**：`deleteApp` 在 purge 前查 `IsRunning`（在途部署/迁移）与 `VolumeBusy`（卷备份/恢复），命中即 409；此前删除是全包唯一不查在途状态的写入口，删除后在途部署 swap 上线孤儿容器（迁移中途删除同根因，另由阶段边界存活检查兜底；检查与删库之间仍有微小 TOCTOU 窗口，无法 100% 消除）。purge 第 2 步追加 `-next`（含 dep 后缀新命名）与 `-prev` 清理；purge 全程持卷槽（入口抢占、defer 释放），卷操作在 purge 期间无法新起；**检查与 purge 之间新起的卷操作**：purge 抢槽失败返回哨兵错误、`deleteApp` 映射为 409（与前置 `VolumeBusy` 检查同码），其余 purge 失败仍 503。
  - 实现备注（2026-09-28）：`deploySingleContainer` 的起新容器命令只清理残留的 `-next` 容器（`docker rm -f <name>-next`），旧版本容器在 swap 前全程运行、失败时原样保留——此前曾误删旧容器（`rm -f` 目标写成了正式容器名），与"探活失败自动保留旧版本"的设计相悖，已修复。
  - 实现备注（2026-09-28，发布流程 review）：git 拉取脚本必须 `set -e`——此前 `fetch` 失败会落到 `checkout --force` 用本地旧代码继续部署并上报成功；Git token 只持久化在 server 端 vault，下发时经 `git -c http.extraHeader="Authorization: Basic <base64(git:token)>"` 随单次 clone/fetch 使用，不再 `remote set-url` 写进 agent 的 `.git/config`（自动发布每次从 vault 现取，不受影响）；下发到 agent 的命令里所有用户输入变量必须 shell 转义（环境变量值曾因空格打散 `docker run` 参数）；swap 脚本 `set -e`，`docker rename` 失败不得被末尾 `docker inspect` 的退出码掩盖；镜像清理只针对 `watchman-app-*` 自建镜像、按创建时间保留最新 5 个（此前 `docker images repo:tag` 只匹配单个 tag 导致从不清，且 `head -n 6 | tail -n +6` 语义错误）；store 的 `GetApp/ListApps/FindAppByWebhookToken` 返回深拷贝——部署协程 `ResolveTemplate` 原地写 EnvVars map 时 HTTP 层正 marshal 同一对象，曾有 crash 风险；反代域名做严格 hostname 校验并检查跨应用重复绑定；网关模式上游按「组网（双方都在组网内 → 100.x）→ 直连（应用主机公网 IP → 内网 IP）」顺序解析，绝不回退 `127.0.0.1`（会指向网关自身造成流量黑洞），两条路径都拿不到才报错要求手动填 Upstream（2026-10-16 起：早期版本在拿不到 Mesh IP 时直接报错，现改为直连兜底）；`raw_compose` 每次部署快照 compose 内容到部署记录，回滚时恢复快照（此前"回滚"实际重部署了当前内容）；healer 跳过有部署在途的应用。
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
  - 实现备注（2026-10-08，流量计量重写）：Agent 按**网卡逐个**跟踪内核累计计数器的增量并落盘（`agent/internal/metrics/traffic.go`），**回环网卡（lo / Windows Loopback）不计入**；网卡消失（docker 网络删建、VPN 启停等）只丢弃其基线、**不再把剩余网卡的累计总量误作增量**（旧逻辑的幽灵流量 bug）；主机重启经 boot time 变化判定，当前计数器值即为重启后流量。docker/tailscale 等虚拟网卡仍会与其底层网卡重复计数同一报文——docker 重度主机可用 Agent 启动参数 `-traffic-ifaces=eth0` 把统计限定到外网网卡。
  - **流量重置日**由面板「财务与规格」统一下发：`RegisterResponse.traffic_reset_day`（proto field 7）在每次 Agent（重）连时推送，Agent 经 `SetResetDay` 热应用并持久化，**面板保存后同时经 `ServerMessage.config_update`（field 20，`ConfigUpdate.traffic_reset_day`）对在线 Agent 即时下发**（`Registry.PushRuntimeConfig`，离线 Agent 下次重连兜底，见 §3.2 兼容说明）；取值范围统一为 1~28（server 侧 `SetAgentBilling` 同步钳制，前端输入框 `max=28`）。
  - **单位为十进制 GB**：配额换算与前后端展示统一按 `1e9`，与面板「流量限制 (GB)」标签及云厂商账单口径一致（此前按 GiB `1<<30`，差 7.4%）。
- **到期续费提醒（内置，非规则）**：主机在「财务与规格」中配置了「到期时间」后，距到期不足 30 天时自动发起一次续费提醒（触发 ID `builtin-expiry-remind`，警告级别，走告警中心与 webhook 通知）；到期时间在 30 天以外的主机不提醒，续费（到期时间推后）后提醒自动解除，逾期未续保持提醒且不重复触发。
- **证书到期提醒（内置，非规则）**：不会自动续期的证书（手动 DNS-01 两阶段签发的、手动上传的）在到期阈值内自动发起一次到期提醒（触发 ID `builtin-cert-expiry`，警告级别，走告警中心与 webhook 通知，告警可跳转证书中心）；提前天数可在「系统设置 → 证书时间设置」中配置（`certs.expiry_reminder_days`，默认 30 天，1~90）；阈值与自动续期缩放一致（`min(配置天数, 有效期/3)`）；证书被续期/替换（到期时间推后）或删除后提醒自动解除，逾期未处理保持提醒且不重复触发。手动 DNS-01 签发的证书不自动续期（续期走自动验证通道，与管理员选择手动的原因相悖）。
- **自定义监控项**：可添加监控参数并设置告警条件。
- **消息通知**：支持配置告警通知方式（邮件/Webhook 等，可扩展）。
- **秒级感知**：主机状态变化及时告警。

### 3.12 系统设置

> 实现备注（2026-09-26，设置中心 Phase 2）：所有设置项走统一存储 `server/internal/settings`
>（`settings.json`，system/user 双作用域、点分键、注册表校验），前端经 `stores/settings`
> 统一读写；`appearance.theme_mode` 已从浏览器 localStorage 迁到服务端（localStorage 仅作首屏缓存）。
> 秘密类配置（AI Key / Git Token）不进统一设置，保留专用存储。
> 实现备注（2026-09-26，设置中心 Phase 1）：`/settings` 改为左右布局——`SettingsLayout.vue`
>（左侧分组导航 + 搜索框，右侧内容区；`views/settings/sections.ts` 为分区注册表：
> key/label/icon/component/keywords/group）。813 行旧 `Settings.vue` 已删除，内联区块抽取为
> `SystemStatus.vue`、`AiConfig.vue`、`GitProviders.vue`、`InstallDeploy.vue`（样式随组件走）。
> 分区切换走 `?s=` 查询参数（与主机详情 `?tab=` 同模式：深链优先、非法值回退 general、
> 选中后把完整路径回写工作区页签），路由保持单条 `settings`（组件名仍为 `Settings`，
> `meta.viewName` 不变），避免嵌套路由导致 AppShell 的 KeepAlive 按 key 反复重建布局。
> 分区组件用内层 `<KeepAlive>` 缓存，切分区不丢已填表单；搜索按 label+keywords 过滤，
> 回车跳首个匹配。AGENTS.md 3.12 中「设置搜索/拆分/导航」条目至此落地。
> 实现备注（2026-09-26，设置中心 Phase 3）：补齐 3.12 剩余可配置项——
> `navigation.default_host_tab`（主机默认页签，URL `?tab=` 深链优先于首选项；选项与
> HostDetail 的 subNavItems 同源）、`appearance.show_tips`（功能提示语开关：前端给说明性
> `<p class="muted">` 加 `tip-hint` 类，`hide-tips` 根类由 App.vue 按设置切换隐藏；警告类
> NAlert 不受影响）、`files.default_path`（文件管理默认路径，空=按 OS 默认；Linux `/root`、
> Windows `C:\`，后端校验必须为绝对路径）。新组件 `views/settings/GeneralPrefs.vue`
> 挂在 Settings.vue 的 CommandLibrary 之后。

- **通用设置**：
  - 首选页面（进入主机管理时默认展示的模块：资源负载/系统状态/Docker/在线终端/文件管理/应用市场）。
  - 开关加载页面时的功能提示语。
- **面板时区**（`server.timezone`，system 作用域，管理员在「系统设置 → 系统状态」中配置）：IANA 时区名称（如 `Asia/Shanghai`），空 = 跟随服务器系统时区。控制端进程本地时间（日志、API 时间戳、日界计算）与面板全部时间展示（告警、审计、会话、监控图表）统一按此时区渲染，所有登录用户一致；保存即时生效，无需重启。Agent 的月流量计费周期仍按各 Agent 所在主机的本地时间，不受此项影响。
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
- **安全入口（宝塔式，2026-09-27）**：系统设置 → 安全入口（`web/src/views/settings/SecureEntry.vue`，`sections.ts` 注册 `key: 'secure-entry'`，`adminOnly` 仅管理员可见）。开启后只能通过秘密入口地址访问/登录面板：`GET /api/v1/secure-entry/<路径>` 校验通过后签发 HttpOnly cookie（`wm_secure_entry=<路径>.<HMAC>`，密钥由 JWT 签名密钥域分离派生 `server/internal/secentry`）并 302 到 `/`；之后才能看到登录页并发起登录。直接访问常规页面/API、无有效 JWT 又无入口 cookie 的请求一律 404（不暴露面板存在）。豁免：Agent 安装/注册（`/install*`、`/agent/*`、`/api/v1/hosts/enroll` 等，B.4 公开注册设计）、应用 webhook、终端分享链接、入口自身端点。Web 控制台经 `go:embed` 打进 server 二进制（`server/web`，构建见 DESIGN.md §8.1），由 Go 直接提供静态页面——无需 nginx；安全入口启用时，guard 中间件对 `/`、`/index.html`、`/assets/*` 同样返回 404（无 cookie/JWT 时），静态页面层同样隐藏。配置键：`security.secure_entry_enabled`（bool）、`security.secure_entry_path`（6~64 位 URL 安全字符，`settings` 注册表校验）；`PUT /settings` 本就 adminOnly + 审计。**忘记路径**：控制机执行 `watchman-server -reset-secure-entry -data <数据目录>`（置 false 后退出）或改 settings.json 后重启。

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

### 3.17 按需运维隧道（kubectl port-forward 式）

> 落地日期：2026-10-19。解决「临时打开某台主机/某个应用的某端口做运维调试」场景：不要求在主机、交换机或安全组上开任何面向公网/内网的监听端口，由运维本机发起、经既有 Agent 反向回连通道中继、进程退出即关闭的按需隧道。

- **数据通路**：`运维本机 127.0.0.1:<本地端口> → watchmanctl → 控制端运维隧道端口（默认 :9443，TLS+用户 JWT）→ rpc.Coordinator 隧道腿 → 目标主机 Agent 的既有 gRPC 长连接（复用 TunnelOpen/TunnelData/TunnelClose 帧）→ 目标主机上的目标端口`。目标主机**零新开口**；无伴生容器、无安全组改动。
- **三端口物理隔离（铁律）**：
  - **面板端口**（默认 18789）：仅承载 Web 控制台与 REST 控制面（登录、查应用、取 CLI 信息），**不流任何业务字节**；
  - **Agent 回连端口**（默认 9090）：被管主机出站长连接，隧道腿帧复用其上（`ServerMessage.tunnel_open` field 17 / `tunnel_data` field 18 / `tunnel_close` field 19 与 `AgentMessage.tunnel_data` field 11 / `tunnel_close` field 12）；
  - **运维隧道数据面端口**：新启动参数 `-opgrpc`（**默认关闭**，启用时**强制 TLS**，无 `-tls-cert/-tls-key` 时拒绝明文启动），仅服务 `OperatorService`（一条 bidi 流代理一条本地 TCP 连接，首帧强制 `OperatorOpen`，回 `OperatorOpenAck{ok,error,conn_id}`，随后 `OperatorData` 双向传输/`OperatorClose` 拆除）。
- **watchmanctl CLI**（`cmd/watchmanctl`，独立二进制，随 Release 发布 linux amd64/arm64、windows amd64、darwin amd64/arm64 共 5 个形态）：
  - `watchmanctl login --server https://<面板>:18789 [--insecure]`：交互输入用户名/密码换取面板 JWT，落盘 `~/.watchmanctl.json`（写入后强制 `Chmod 0600`，已存在文件同样收紧）；`ls`/`close`/`tunnel` 对已用 `--insecure` 登录的配置在输出前打印一次明文告警；
  - `watchmanctl tunnel --app <appID> --port <端口> [--listen [ip:]port]`：应用定向模式，本地随机或指定端口转发到应用暴露端口（服务端按 `app.Ports[].Host` 白名单校验，`bind_scope=mesh` 时上游自动解析为主机组网 IP，否则 `127.0.0.1`）；
  - `watchmanctl tunnel --host <hostID或主机名> --target 127.0.0.1:<端口> [--listen ...]`：主机模式，`--target` 任意内网地址仅 admin 可用且记高危审计（防网络跳板滥用），operator 仅允许回环目标；
  - `watchmanctl ls` / `watchmanctl close <connID>` / `watchmanctl version`；
  - 每次启动隧道**实时**调 `GET /api/v1/tunnels/client-info` 取数据面端点（不缓存，防面板漂移；`enabled` 反映数据面监听器真实状态——`-opgrpc` 未配置证书或监听失败时服务端 fail-closed 上报未启用；`endpoint` 推导链：面板公网地址设置 → `-public-url` 启动参数 → 请求 Host，IPv6 字面量去重围括号后 `JoinHostPort` 保证 `[::1]:9443` 形态）；断流语义对齐 kubectl——不自动重连，干净退出（隧道是会话态工具），代理首帧 15 秒 ack 看门狗超时即取消整条流（per-stream ctx，防 `stream.Recv` 永久阻塞）。HTTP/2 流控窗口增大（流 4MB / 连接 16MB，两端对称），TLS 启用会话恢复（`ClientSessionCache`）。
- **Web 控制面入口**：应用详情页「端口映射」每行端口带「隧道」按钮、主机详情组网页工具区带「临时隧道」按钮，打开共享弹层（`web/src/components/tunnels/OpsTunnelModal.vue`）：展示可复制的 `watchmanctl login`（以 `window.location.origin` 为面板地址）与 `watchmanctl tunnel` 命令、5 平台 CLI 下载链接（GitHub Release 直链）、当前用户活动隧道列表及逐项关闭；数据面未启用时给出管理员开启 `-opgrpc` 的引导。
- **安全边界**：角色仅限 admin/operator（viewer 拒绝并记审计）；分组可见性按 `groups.CanAccessGroup` 实时校验；开腿鉴权**每次 open 实时重查**（不缓存）；`target_port` 强制 1~65535（应用模式白名单之前先过界值）；目标存在性对外不透露——应用不存在/主机不存在/跨分组无权访问统一回 `目标不存在或无权访问`（消除存在性预言机；2026-10-25 鉴权顺序修正：`resolveTarget` 在应用/主机解析成功后、任何端口白名单与目标地址校验之前先做分组校验——此前跨分组应用 + 非法端口会先报白名单错误，泄露存在性）；**错误分类**：策略拒绝（角色、分组、端口白名单、并发腿数超限）→ `PermissionDenied` + `op_tunnel_deny`（ResultBlocked）；基础设施失败（Agent 下线、腿建立失败）→ `Unavailable` + `op_tunnel_open`（ResultFailed），两者绝不混记；`OperatorOpenAck` 发送失败时拆除腿并补记一条成对的 `op_tunnel_close`，保证 open/close 审计配平。审计动作 `op_tunnel_open`（app 模式 medium，任意 target_host 的 host 模式 high）、`op_tunnel_close`（low）、`op_tunnel_deny`（ResultBlocked）。
- **限额与回收**：每用户并发隧道腿 ≤ 8（`maxLegsPerOperator`），单 Agent 流式腿 ≤ 64；两个上限在 `Coordinator.OpenLeg` 内**同一把互斥锁下完成计数+插入**（并发开腿无 TOCTOU 窗口），超限返回哨兵错误 `ErrOwnerLegsExceeded`/`ErrAgentLegsExceeded`（`errors.Is` 可辨，归为策略拒绝类审计）；每腿空闲 15 分钟或存活 8 小时由 `Coordinator.StartReaper` 自动回收；Agent 断连时 `CloseByAgent` 连带清扫其孤儿流腿；慢消费流腿按既有策略丢弃（`toSock` 满即弃腿）。

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
├── deploy/                     # 部署与引导脚本
│   ├── install.sh             # 控制端一键安装/升级/卸载脚本（单二进制 + systemd）
│   └── quick_start.sh         # 一键引导脚本（下载最新 install.sh 并透传参数）
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
    ConfigUpdate      config_update=20; // 面板改配置后对在线 Agent 即时下发（重连兜底见 RegisterResponse）
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
  int32  traffic_reset_day = 7;   // 月流量重置日（1~28，每次重连下发）
  // 三网监控探测目标：键 = 运营商（telecom/unicom/mobile），值 = host[:port]
  // TCP 端点（端口缺省 80）；仅下发有效目标，空 map = 探测关闭。
  map<string, string> probe_targets = 9;
}

// ConfigUpdate 携带面板编辑后的主机级运行时配置，对在线 Agent 经
// ServerMessage.config_update（field 20）即时下发，语义与 RegisterResponse
// 同名字段一致；离线 Agent 收不到，下次（重）连时由 RegisterResponse 兜底。
// 旧 Agent 收到未知 oneof 字段落 default 分支静默忽略，下次重连生效；新 Agent
// 连旧服务端则永远收不到该消息，同样退化为重连生效——双向组合均无错误。
message ConfigUpdate {
  map<string, string> probe_targets = 1; // 语义同 RegisterResponse.probe_targets（空 map = 关闭探测）
  int32  traffic_reset_day = 2;          // 语义同 RegisterResponse.traffic_reset_day
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
  // 三网监控探测结果：键 = 运营商（telecom/unicom/mobile），至少完成过
  // 一轮探测才上报对应条目；latency_ms = 0 表示该轮无成功尝试。
  map<string, ProbeResult> probe_results = 26;
  message ProbeResult {
    double latency_ms = 1;         // 成功尝试 RTT 中位数（ms）
    double loss_pct = 2;           // 失败尝试占比（0~100）
  }
}

message SysInfoQuery { string kind=1; }   // process/port/user/login
message SysInfoSnapshot {
  string kind = 1; bytes json_payload = 2; // 结构化 JSON（不同 kind 不同 schema）
}

// 按需运维隧道数据面（§3.17）：挂在控制端独立 -opgrpc 端口（默认 :9443，
// 强制 TLS + 面板 JWT），一条 bidi 流代理一条本地 TCP 连接。
service OperatorService {
  rpc Connect(stream OperatorMessage) returns (stream OperatorMessage);
}
message OperatorMessage {
  oneof payload {
    OperatorOpen    open  = 1;   // 客户端首帧（强制）
    OperatorOpenAck ack   = 2;   // 服务端首帧：ok/error/conn_id
    OperatorData    data  = 3;   // 双向传输字节
    OperatorClose   close = 4;   // 任一侧拆除（带 reason）
  }
}
message OperatorOpen {
  string host_id     = 1;        // 目标主机（app_id 为空时必填）
  string app_id      = 2;        // 应用定向模式：服务端解析主机 + 端口白名单
  string target_host = 3;        // 空=自动（127.0.0.1 或组网 IP）；任意值仅 admin
  uint32 target_port = 4;
}
message OperatorOpenAck { bool ok = 1; string error = 2; string conn_id = 3; }
message OperatorData  { bytes data = 1; }
message OperatorClose { string reason = 1; }
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
GET/PUT  /api/v1/ai/config                   # AI provider/模型/开关/限速（GET 中 api_key 恒为掩码哨兵 "********"；PUT 传哨兵 = 保留原密钥）
POST   /api/v1/ai/config/key                  # 明文查看已保存的 AI API Key（adminOnly；审计 ai_config_key_reveal / high）
GET/POST /api/v1/ai/chat                     # 自然语言问答（tool calling）
POST   /api/v1/ai/nl2command                  # 自然语言转命令（带 host_id 上下文）
POST   /api/v1/ai/app-draft                   # 需求描述 → 创建应用表单草稿（服务端消毒，不创建）
POST   /api/v1/ai/app-chat                    # 创建页对话辅助（携带表单快照，可返回 form_patch 草稿）
GET    /api/v1/ai/audit                       # AI 调用审计

# 应用中心
GET    /api/v1/apps
POST   /api/v1/apps                       # 创建应用（创建即自动入队首轮部署）
GET    /api/v1/apps/:id
PUT    /api/v1/apps/:id                   # 部分更新配置
DELETE /api/v1/apps/:id?purge=            # 删除应用（可选清除容器与镜像）
POST   /api/v1/apps/:id/deploy?force=     # 触发部署（force=true 支持终止在途任务并重新部署）
POST   /api/v1/apps/:id/deployments/cancel # 终止当前正在进行的部署任务
POST   /api/v1/apps/:id/rollback          # 版本回滚
POST   /api/v1/apps/:id/stop
POST   /api/v1/apps/:id/start
POST   /api/v1/apps/:id/restart
GET    /api/v1/apps/:id/deployments
GET    /api/v1/apps/:id/deployments/:depID
POST   /api/v1/apps/:id/deployments/:depID/diagnose # AI 排障
GET    /api/v1/apps/:id/status
POST   /api/v1/apps/:id/webhook/sync
POST   /api/v1/apps/:id/github-app/switch # 切换到 GitHub App 触发（非 github.com 仓库 400；有旧逐仓 hook 时先删，失败 502 不改库）
# GitHub App（见 §3.7）
GET    /api/v1/github-app/status
GET    /api/v1/github-app/installations/repos
GET    /api/v1/github-app/repos/:owner/:repo/branches
POST   /api/v1/github-app/manifest        # 生成 Manifest（state 内存 15 分钟过期）
POST   /api/v1/github-app/manifest/exchange # 验 state 后换 code 落盘（私钥可用性校验）
POST   /api/v1/github-app/config          # 手动填写 App ID/私钥/Webhook Secret（私钥可用性校验）
POST   /api/v1/github-app/installations/sync # 同步安装与仓库清单
DELETE /api/v1/github-app                # 断开（清配置与安装）
POST   /api/v1/github-app/webhook         # 公开端点（HMAC-SHA256 验签、delivery 去重、未配置 503）
POST   /api/v1/apps/inspect-image         # 镜像元数据免拉取只读探测
POST   /api/v1/apps/inspect-repo          # 仓库元数据只读探测
GET    /api/v1/apps/:id/volume-backups?volume=&offset=&page_size=  # 挂载卷备份归档列表（登录即可；含 last_restore_at/status/error 与 s3:{ready,target_id,target_name,bucket} 去向块）
GET    /api/v1/apps/:id/volume-backups/:archiveID/download   # 下载归档（只读、免审计；仅 success，302 到 15 分钟预签名 S3 GET，目标已删除 410）
POST   /api/v1/apps/:id/volume-backups    # 备份挂载卷（body {volumes?: string[], note?≤200 字, target_id?}，202 异步；归档直传页内所选 S3 目标，target_id 省略时回落默认目标，两者皆无 400，未知 target_id 400；仅已声明卷；卷忙/部署中 409、Agent 离线 503）
POST   /api/v1/apps/:id/volume-backups/:archiveID/restore  # 恢复归档（仅 success；下载 S3 归档并校验 SHA256 后 停机→安全副本→解压→重启，结果写回 last_restore_*）
POST   /api/v1/apps/:id/volume-backups/upload-restore  # 上传备份文件并恢复（multipart {file, volume_spec, sha256?}，2GB 上限，202 异步；面板暂存 + Agent 一次性回拉，复用恢复核心；返回 archive_id 供轮询最近恢复）
GET    /api/v1/volume-uploads/:token   # Agent 一次性下载上传文件（公开、无 JWT，token 即凭证；安全入口豁免）
DELETE /api/v1/apps/:id/volume-backups/:archiveID          # 删除归档（含 S3 对象与宿主暂存/安全副本；running 归档 409）

# 审计
GET    /api/v1/audit?user=&action=&from=&to=

# 系统升级与自维护（控制端在线自升级）
GET    /api/v1/system/check-update?force=&beta=   # 查询官方 Release 最新版本、更新日志与升级包（带通道与缓存）
POST   /api/v1/system/online-upgrade              # 控制端一键在线自升级（下载官方包、校验SHA-256、原子替换、维护握手平滑重启）
POST   /api/v1/system/upgrade-server              # 离线上传 watchman-server 单二进制升级
POST   /api/v1/system/restart                     # 控制端平滑重启（广播维护预告，保护全网 Agent 零误报）

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

### B.6.1 控制端自托管部署架构（Go 单二进制 + systemd，对标 1Panel / 宝塔）

控制端为单个静态编译的 Go 二进制文件（`watchman-server`），Web 控制台前端产物在构建阶段经 `go:embed` 完整嵌入二进制（`server/web/embed.go`）。控制端启动后直接通过内置 HTTP 引擎提供 Web 页面（含 SPA 路由兜底）、RESTful API 与 WebSocket 终端/文件流，**无需任何外部 Nginx 或独立前端反代容器**。

- **默认监听端口**：
  - `HTTP / Web 控制台`：`:18789`（通过 `-http :18789` 指定，采用冷门高位端口，不占用宿主机 80/443/8080 等常用 Web 业务端口）。
  - `gRPC 被控端长连接`：`:9090`（通过 `-grpc :9090` 指定，所有被纳管 Agent 均主动出站回连该端口）。
  - `TLS / HTTPS`：支持 `-http-tls` 配合 `-tls-cert` 与 `-tls-key` 启用控制端 HTTPS 与加密 gRPC 通道。
- **文件与目录布局**：
  - 安装根目录：`/opt/watchman`
  - 程序文件：`/opt/watchman/bin/watchman-server`、`/opt/watchman/bin/watchman-agent-*`
  - 数据目录：`/opt/watchman/data`（存储用户凭据 `users.json`、设置中心 `settings.json`、终端会话与录像 `sessions/`、发布清单 `manifest.json` 等）
  - 密钥环境文件：`/opt/watchman/watchman.env`（`WATCHMAN_JWT_KEY` 与 `WATCHMAN_VAULT_PASS`，权限 `0600`，重装与平滑升级时持久化复用）
- **Systemd 托管规范**：
  - 控制端服务：`/etc/systemd/system/watchman-server.service`（自动加载 `watchman.env`，`Restart=always`，`RestartSec=3`）；
  - 本机自纳管 Agent：`/etc/systemd/system/watchman-agent.service`（安装时自动向控制端注册并纳管控制机自身，复用 `agent-state.json` 凭据，防止重复注册）。

### B.6.2 一键安装部署与自动升级（install.sh / quick_start.sh）

提供对标 1Panel 的单行命令极简引导与全功能安装脚本：

- **一键引导安装（推荐，自动获取 master 分支最新脚本）**：
  ```bash
  bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)"
  ```
  可追加参数（原样透传给 `install.sh`）：
  ```bash
  bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)" -- --port 18789
  ```

- **直接使用主安装脚本**：
  ```bash
  curl -fsSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/install.sh | bash
  ```

- **命令行参数清单（`deploy/install.sh`）**：
  - `--version <tag>`：精确锁定官方 Release 版本（如 `--version v1.2.3`，优先级最高）；
  - `--port <port>`：自定义 Web 控制台端口（默认 `18789`）；
  - `--mirror <url>`（或环境变量 `GITHUB_MIRROR`）：指定 GitHub 下载加速镜像（如 `https://ghproxy.example.com`）；
  - `--base-url <url>`：完全自定义归档下载源（源结构须满足 `{base}/{tag}/watchman-dist-{tag}-linux-{arch}.tar.gz`）；
  - `--repo <owner/repo>`：指定 GitHub 仓库（默认 `lizhixu/workbench`）；
  - `--force`：强制重新下载并覆盖安装当前版本；
  - `--force-new-keys`：检测到已有数据目录但缺失 `watchman.env` 时，确认强制生成新密钥；
  - `--uninstall`：停止并注销 systemd 服务，删除二进制程序（保留 `/opt/watchman/data` 与密钥）；
  - `--uninstall --purge`：完全卸载并彻底清除所有数据。

- **版本解析与升级策略**：
  - **版本来源唯一真相**：**版本完全由官方 Release 提供，严禁任何自行决定或拼凑版本的行为**。发布包与升级清单必须 100% 来源于 Git tag 触发 GitHub Actions 产出的官方 Release（如 `v1.2.3` / `v1.2.3-beta.1`）以及由 CI 生成的 `manifest.json`；
  - **正式版与测试版通道**：默认跟踪 GitHub `/releases/latest`（自动排除 pre-release）。测试版改由面板开关控制——在「系统设置 → 系统升级」中开启「加入测试计划」（`system.join_beta_program`），`install.sh` 升级时读取该开关，若开启则取 release 列表前 20 项按 semver 取最大版本（含 pre-release；GitHub 列表不保证按时间排序，不可直接取首项，2026-09-29 已因此踩坑）；
  - **安装升级幂等性**：重跑脚本即为平滑升级，同版本默认跳过，保留数据目录与密钥；升级时自动停旧服务、替换二进制、复用凭据并热启动。
  - **控制端在线检查与一键在线自升级（Web 自升级，2026-09-29）**：
    - **在线 Release 探测引擎（`server/internal/release/checker.go`）**：控制端后台主动对接 GitHub Releases API，依据用户统一设置 `system.join_beta_program` 自动选择通道（开启测试计划时拉取前 20 个 Release，用 `CompareVersions` 取最大版本，包括 pre-release——GitHub 列表不保证按时间排序，禁止取首项；关闭时拉取 `/releases/latest` 正式稳定版）；内置 5 分钟内存自愈缓存防止触发 GitHub API 频控，支持强制刷新 `force=true`；
    - **Semver 规范多段语义比对（`CompareVersions`）**：严格兼容标准 Release 与 Pre-release 优先级，支持 `v0.1.0-beta.6` < `v0.1.0-beta.7` 等带点的先行版本比对、数值段比对及生产版优先于预发布版判定；
    - **全自动防篡改原子升级管线（`server/internal/release/updater.go`）**：
      1. 流式下载对应架构的官方发布包 `watchman-dist-${tag}-${os}-${arch}.tar.gz` 与 `CHECKSUMS.txt`；
      2. 校验 SHA-256 哈希值与官方指纹完全一致，确保传输未被篡改；
      3. 解压并提取 `watchman-server`、`manifest.json` 与新版 Agent 二进制；
      4. 备份当前二进制为 `.bak.<timestamp>` 并以 `0755` 权限原子替换运行文件；
      5. 自动同步更新 `/opt/watchman/manifest.json` 与 `/opt/watchman/bin/` 目录，使随后的 Agent 批量升级权威基准即时对齐；
      6. 向全网在线 Agent 广播下发 120 秒维护预告信令（`reg.BroadcastMaintenance("server_restart", 120)`），自动进入维护免告警期；
      7. 后台延迟 1 秒触发平滑重载：优先执行 `systemctl --no-block restart watchman-server`（systemd 在 TimeoutStopSec 后会 SIGKILL 兜底，重启一定能发生；非 systemd 部署或 unit 改名时回退为自发 `os.Interrupt`），前端 3~5 秒后平滑重连刷新。注意：2026-09-29 曾用纯自发 SIGINT 方案，因旧二进制关闭路径里 `gs.GracefulStop()` 被 agent 长连接永久阻塞（sync.Cond.Wait），进程不退出、systemd 不重启，面板直接挂掉——自此重启/升级一律走 systemctl 优先路径。
    - **响应式前端通知流（`useSystemUpdateStore`）**：顶栏设置图标徽标红点、侧边栏导航「NEW」高亮角标与「系统升级」页签顶部醒目横幅全联动，支持折叠查看官方 Release Notes（变更说明），一键直达在线自升级。
  - **架构演进说明（2026-09-28）**：全面移除外置 Nginx 依赖（控制端内嵌前端 `server/web` 自提供），旧版静态目录与 `cmd/deploy/` SSH 部署工具已彻底下线；`install.sh` 在升级时自动检测并清理旧版残留的 nginx 站点配置，避免历史 `auth_request` 指向已下线端点引发静态页 404。

### B.6.3 被控端 Agent 一键绑定与自纳管

控制端对外提供动态安装脚本服务，无需向被控端传输静态脚本文件：

- **控制台生成一键安装命令**：
  在控制台主机列表点击「添加主机」，控制端通过 `POST /api/v1/hosts/enroll` 签发一次性 `enroll_token`（默认 24h 有效期），并生成跨平台绑定命令：
  - **Linux 主机**：
    ```bash
    curl -kfsSL 'http://<server-ip>:18789/install?token=<enroll_token>' | sudo bash
    ```
  - **Windows 主机（PowerShell）**：
    ```powershell
    irm 'http://<server-ip>:18789/install?os_type=windows&token=<enroll_token>' | iex
    ```

- **Agent 安装流程（`server/internal/install/install.go`）**：
  1. 安装脚本自动识别目标主机的 OS（Linux / Windows）与架构（amd64 / arm64）；
  2. 从控制端公开接口 `/api/v1/agent/binary` 下载对应架构的 `watchman-agent` 二进制；
  3. 服务持久化注册：
     - Linux：安装至 `/opt/watchman/watchman-agent`，注册 `watchman-agent.service` systemd 服务并开机自启；
     - Windows：安装至 `C:\Program Files\Watchman\watchman-agent.exe`，注册名为 `WatchmanAgent` 的计划任务开机启动；
  4. 携带参数启动：`-server <server-ip:9090> -token <enroll_token> [-upgrade-pubkey <pubkey>]`；
  5. Agent 首次向控制端 gRPC 端口发起 `RegisterRequest` 换取长期凭证 `auth_token` 并持久化到 `agent-state.json`，随后立即清空 `enroll_token` 进入稳定心跳和监控上报状态。

### B.6.4 控制端命令行参数与运行时配置清单

#### 1. 控制端启动参数（CLI Flags）

| 参数 (Flag) | 默认值 | 对应环境变量 | 说明 |
| --- | --- | --- | --- |
| `-http` | `:18080` (安装脚本设 `:18789`) | `HTTP_PORT` | HTTP 服务监听地址（Web 页面、REST API 与 WS 网关） |
| `-grpc` | `:9090` | — | gRPC 服务监听地址（供 Agent 主动回连） |
| `-opgrpc` | `""`（关闭） | — | 按需运维隧道数据面 gRPC 监听地址（§3.17，watchmanctl 数据面，建议 `:9443`）；默认零新开口，启用时强制 TLS——无 `-tls-cert`/`-tls-key` 时拒绝以明文启动该端口 |
| `-data` | `./data` (安装部署为 `/opt/watchman/data`) | `DATA_DIR` | 持久化数据目录（存储用户、会话、录像、统一设置、证书等） |
| `-jwt-key` | 随机生成 | `WATCHMAN_JWT_KEY` | JWT 鉴权签名密钥（`watchman.env` 持久化 32 字节 hex） |
| `-vault-pass` | 随机生成 | `WATCHMAN_VAULT_PASS` | 凭证密码箱加密及 Ed25519 升级签名派生根密钥 |
| `-manifest` | `/opt/watchman/manifest.json` | — | 官方 Release 清单文件路径，Agent 目标版本与 SHA-256 唯一权威基准 |
| `-public-url` | 空 (自动取请求 Host) | — | 控制端对外公网根地址（如 `https://watchman.example.com`，解决反代/NAT 下载 URL 不一致） |
| `-ws-origins` | 空 | — | 允许连接 WebSocket 的额外浏览器 Origin 白名单（逗号分隔，`*` 禁用校验） |
| `-tls-cert` | 空 | — | gRPC 与 HTTPS 服务端 TLS 证书路径 |
| `-tls-key` | 空 | — | gRPC 与 HTTPS 服务端 TLS 私钥路径 |
| `-http-tls` | `false` | — | 是否在 `-http` 端口上直接启用 HTTPS（须与 `-tls-cert`/`-tls-key` 成对配置） |
| `-reset-secure-entry` | `false` | — | 应急工具开关：关闭安全入口（置 `security.secure_entry_enabled=false`）后退出 |
| `-reset-panel-ssl` | `false` | — | 应急工具开关：关闭面板 SSL 与强制 HTTPS 重定向（置 `security.panel_ssl_enabled=false` 与 `security.panel_force_https=false`）后退出 |
| `-reset-panel-domain` | `false` | — | 应急工具开关：解绑面板绑定域名（置 `security.panel_domain=""`；绑定即自动严格，解绑后恢复 IP 访问）后退出 |

#### 2. 系统与用户统一设置（`settings.json`）

系统配置采用点分命名空间注册表统一校验，分 `system`（管理员全局）与 `user`（当前用户独立）双作用域，秘密（AI Key / Git Token）存放于加密 Vault。

| 配置键 (Key) | 作用域 | 默认值 | 校验与说明 |
| --- | --- | --- | --- |
| `server.public_url` | `system` | `""` | 面板公网根访问地址（URL 格式，如 `https://panel.example.com:18789`，用于 Agent 一键安装与分享链接） |
| `server.timezone` | `system` | `""` | 面板时区（IANA 名称，如 `Asia/Shanghai`；空 = 跟随服务器系统时区，`time.LoadLocation` 校验）。控制端进程本地时间（日志、API 时间戳）与面板全部时间展示（告警/审计/会话/图表）统一按此时区；经 `GET /version` 的 `timezone` 字段下发给所有登录用户，前端 `utils/time.ts` 统一格式化，未设置时前端回退浏览器本地时区。保存即时生效（启动与每次系统设置保存后 `ApplyTimezone`）。Agent 月流量计费周期仍按各 Agent 主机本地时间，不受此项影响 |
| `security.panel_domain` | `system` | `""` | 面板绑定域名（主机名格式，如 `panel.example.com`）；绑定后自动启用严格域名限制，面板只能通过该域名访问 |
| `security.panel_ssl_enabled` | `system` | `false` | 面板 SSL / HTTPS 总开关（通过端口级协议动态嗅探实现零停机热重载） |
| `security.panel_ssl_mode` | `system` | `"cert_center"` | 证书来源模式（`cert_center` 从证书中心选取 / `custom` 自定义 PEM 证书与私钥） |
| `security.panel_ssl_cert_id` | `system` | `""` | 证书中心证书 ID（`cert_center` 模式下选取） |
| `security.panel_ssl_cert_pem` | `system` | `""` | 自定义证书 PEM 内容（含完整证书链） |
| `security.panel_ssl_key_pem` | `system` | `""` | 自定义私钥 PEM 内容 |
| `security.panel_force_https` | `system` | `false` | 强制 HTTPS 访问（未加密 HTTP 自动 301 重定向至 HTTPS） |
| `certs.auto_renew_days` | `system` | `30` | 证书自动续期提前天数（1~90，短期证书按有效期 1/3 自适应收紧，取较小者） |
| `certs.expiry_reminder_days` | `system` | `30` | 手动证书到期提醒提前天数（1~90；手动签发/手动上传的证书到期前经告警中心提醒一次） |
| `system.join_beta_program` | `system` | `false` | 加入测试计划：开启后 `install.sh` 与批量升级将跟踪预发布版本（pre-release） |
| `security.secure_entry_enabled` | `system` | `false` | 宝塔式安全入口总开关：开启后非秘密路径一律返回 404 |
| `security.secure_entry_path` | `system` | `"entry"` | 安全入口秘密路径（6~64 位 URL 安全字符） |
| `navigation.default_host_tab` | `user` | `"overview"` | 进入主机详情时的默认页签（overview / metrics / system / docker / terminal / files / network 等） |
| `appearance.theme_mode` | `user` | `"dark"` | 外观主题模式（light / dark / auto） |
| `appearance.show_tips` | `user` | `true` | 是否展示页面功能提示语（开关说明类 `.tip-hint` 段落） |
| `terminal.theme` | `user` | `"default"` | 在线终端主题方案 |
| `terminal.default_shell` | `user` | `"bash"` | 默认登录 Shell（bash / sh / powershell / cmd） |
| `terminal.font_size` | `user` | `14` | 终端字号（10~32） |
| `terminal.scrollback` | `user` | `5000` | 终端回滚缓冲区最大行数（1000~50000） |
| `files.default_path` | `user` | `""` | 文件管理器默认进入路径（必须为绝对路径，空则按操作系统默认） |

### B.6.5 发版流程与 Release 管线（GitHub Actions + tag 驱动）

版本唯一真相源是 git tag，**版本完全由官方 Release 提供，严禁任何自行决定或拼接版本的行为**。`internal/version` 的 `Version`/`Commit`/`BuildTime` 由构建时 ldflags 注入（见 `Makefile` 的 `LDFLAGS`）；`Get()` 取值优先级：ldflags 注入 > `debug.ReadBuildInfo()`（`go install` 装 tagged commit 时工具链自带）> `0.1.0-dev` 占位。任何本地部署、发布脚本或测试工具严禁擅自使用 commit hash 等拼接伪版本（如 `0.1.0-<hash>`）；未打 tag 的工作区一律视为开发态，升级管线权威基准必须来自官方 GitHub Releases 发布的 tarball 与 CI 生成的 `manifest.json`。

- **打 tag 即发版（严格附注变更说明，严禁裸 tag）**：
  每次发布严禁创建和推送未包含说明信息的裸 tag（如仅 `git tag vX.Y.Z`）。必须使用带附注的 tag 并详细记录本次版本的变更描述（分为 Added / Changed / Fixed / Security 等清晰条目）：
  ```bash
  git tag -a v1.2.3 -m "release: v1.2.3

  ### Changed
  - 主机详情页与主机列表清晰区分「已是最新」与「可升级」状态，最新版操作按钮平滑切换为「重装」
  - 优化系统升级页面状态机，去除多余加载动画并即时捕获展示各主机升级失败详情

  ### Fixed
  - 修复 Agent 升级遇到错误（如二进制占位）时状态机未即时释放的问题
  "
  git push origin v1.2.3
  ```
  GitHub Release notes 也必须同步保留上述详细变更清单，确保版本迭代历史 100% 透明可溯。
- tag 规范：`v1.2.3` 为正式版；`v1.3.0-rc.1` / `v1.3.0-beta.1`（带 `-` 后缀）由 CI 自动标记为 pre-release。
- **必须是附注 tag（annotated），严禁轻量 tag（2026-10-28 事故根因）**：`git tag v1.2.3` 这类轻量 tag 在 `git tag -l --format='%(contents)'` 下会**静默退化为指向 commit 的 message**，而 CI 只判断附注是否非空，于是发出去的 Release 正文变成「最近一条提交的描述」，该版本其余变更全部丢失，`--generate-notes` 兜底也因正文非空而永不触发。历史遗留的轻量 tag：`v0.0.0-ci-test`、`v0.1.0-beta.2`、`v0.1.0-beta.5`、`v0.1.0-beta.6`（已发布 Release 不可变，保留原样，仅作已知异常记录）。
- **发版前必须本地校验**：`make tag-check TAG=v0.2.0-beta.1`（等价于 `bash scripts/verify-tag.sh`）。校验四件事：命名严格匹配 semver、对象类型必须是 `tag`（非 `commit`）、附注非空且 ≥3 行、附注带可识别的变更条目。CI 在 `Verify release tag` 步骤执行同一脚本，不通过直接中止发布。
- 构建矩阵：server（linux amd64/arm64）+ agent（linux amd64/arm64、windows amd64）+ watchmanctl（linux amd64/arm64、windows amd64、darwin amd64/arm64），全部 `CGO_ENABLED=0` 静态编译。
- **发布产物规范（双轨发布体系：独立单二进制 + 完整安装包）**：
  每次发布直出以下完整资产列表，满足直接运行与自动化安装双重需求：
  1. **直接可执行的独立二进制（无须解压，下载即用）**：
     - `watchman-server-linux-amd64`：Linux x86_64 控制端独立程序（内嵌 Web 控制台）
     - `watchman-server-linux-arm64`：Linux ARM64 控制端独立程序（内嵌 Web 控制台）
     - `watchman-agent-linux-amd64`：Linux x86_64 被管端 Agent 独立程序
     - `watchman-agent-linux-arm64`：Linux ARM64 被管端 Agent 独立程序
     - `watchman-agent-windows-amd64.exe`：Windows x86_64 被管端可执行程序
     - `watchmanctl-linux-amd64` / `watchmanctl-linux-arm64`：Linux 运维 CLI（§3.17 按需运维隧道客户端）
     - `watchmanctl-windows-amd64.exe`：Windows x86_64 运维 CLI
     - `watchmanctl-darwin-amd64` / `watchmanctl-darwin-arm64`：macOS（Intel / Apple Silicon）运维 CLI
  2. **完整套件安装包（供 install.sh 脚本与系统化一键部署）**：
     - `watchman-dist-${TAG}-linux-amd64.tar.gz`（内含控制端、各平台 agent 二进制、`install.sh` 与 `manifest.json`）
     - `watchman-dist-${TAG}-linux-arm64.tar.gz`
  3. **权威元数据与安全校验**：
     - `manifest.json`：官方发布清单，记录版本号、Commit 与各平台 Agent 二进制权威 SHA-256
     - `CHECKSUMS.txt`：包含上述所有独立单二进制、归档包与清单文件的全局 SHA-256 校验和
- `manifest.json`：`{version, commit, build_time, agents: {"linux/amd64": {file, sha256}, ...}}`。已落地（2026-09-27）：控制端启动时读 `-manifest`（默认 `/opt/watchman/manifest.json`，由 install.sh 部署），作为 agent 升级的版本/sha256 基准，替代 server 自身构建版本，解除"最新 = 控制端自己"的闭环；无 manifest 时（dev）回退到 `CurrentAgentVersion`。单台/批量升级的版本判定、sha256（优先 manifest，其次 `install.AgentBinarySha256`）、签名、下载 URL 全部走这套基准。`GET /api/v1/version`（authed）返回 server 构建信息 + `agent_target_version` + `agent_manifest` + `upgrade_pubkey` + `public_url`。
- 控制端单二进制按需自动拉取 Agent 机制：若控制端脱离 `/opt/watchman` 作为独立二进制运行（本地缺少 agent 二进制），在被纳管主机请求下载 Agent 时，控制端自适应依据当前 Release 版本自动从 GitHub Releases 官方下载对应 Agent 并缓存至 `data/bin/`，确保跨机器一键安装命令平滑打通。
- 版本通道：`stable`（默认，GitHub `/releases/latest`，自动排除 pre-release）、`beta`（releases API 列表第一条，含 pre-release）、`--version` 精确锁定（优先级最高）。
- 手动触发：workflow_dispatch 只构建打包、上传 artifacts，不创建 Release（正式打 tag 前验证管线用）。

### B.6.6 提交规范（Conventional Commits）与变更日志生成

Release 正文的**唯一数据源是本仓库的提交历史**：打版本 tag 时用 `scripts/changelog.sh` 把「上一个 tag..HEAD」之间的全部提交按 type 聚合成分段清单，写入附注 tag 正文，CI 原样输出到 GitHub Release。因此提交信息的规范性直接决定 Release notes 的完整性——**写坏的提交不会被人工补回，直接表现为该版本变更缺失**。

- **格式**：`<type>(<scope>): <subject>`，后接可选 body 与 footer。
- **type 白名单与去向映射**（其余一律视为不合规）：

| type | 含义 | changelog 段 | 版本位 |
| --- | --- | --- | --- |
| `feat` | 新功能、新页面、新接口 | Added | MINOR |
| `fix` | 缺陷修复 | Fixed | PATCH |
| `security` | 安全修复（越权、注入、验签、敏感信息泄露） | Security | PATCH（严重升 MINOR） |
| `perf` | 性能优化 | Changed | PATCH |
| `style` | **UI/视觉调整**（用户可见） | Changed | PATCH |
| `refactor` | 重构，对外行为不变 | Changed | PATCH |
| `revert` | 回滚 | Fixed | 视原提交 |
| `chore(format)` | 纯格式化、重命名 | 不进 | 无 |
| `docs` / `test` / `build` / `ci` / `chore` | 文档、测试、构建、流水线、杂务 | 不进 | 无 |

- **scope 固定词表**（不自创，避免 changelog 分组碎片化）：`agent` `server` `proto` `web` `apps` `hosts` `metrics` `cert` `terminal` `files` `docker` `ai` `upgrade` `settings` `auth` `audit` `tunnel` `registry` `deps` `ci`；同一功能跨层用 `+` 连接，如 `feat(apps+web)`。
- **subject**：中文陈述句、≤50 字、不加结尾句号、写「做了什么」而非「改了哪个文件」。
- **破坏性变更**：`feat(apps)!: ...` 或 footer 写 `BREAKING CHANGE: <影响面>`；changelog 中单独成段并置顶，同时决定版本位走 MAJOR（0.x 阶段走 MINOR）。
- **body**：只写动机与影响面，不复述 diff；footer 可写 `Refs:` / `Closes #123`。
- **禁止**：`update` / `修改` / `修复一下` / `wip` / 无 type 的裸描述进 master；一个提交混多个不相关模块；纯格式化夹带在功能提交里（必须单独 `chore(format)`）。

**工具链（三件均已落地）**：

1. `make hooks`：`git config core.hooksPath .githooks`，启用 `.githooks/commit-msg` 校验 type/scope/格式/subject 长度与句号。钩子随仓库版本化，换机器执行一次即可；**禁止 `--no-verify` 绕过**。
2. `make changelog SINCE=v0.1.0-beta.24 VERSION=v0.2.0-beta.1`：输出带 `release:` 首行的五段式（Breaking Changes / Added / Changed / Fixed / Security）markdown，空段自动省略；`docs/test/build/ci/chore` 自动过滤，不合规提交原样落入 Changed 段（能看到，但应在源头修掉）。
3. `make tag-check TAG=v0.2.0-beta.1`：发版门禁，见 §B.6.5。

**标准发版流程**：

```bash
# 1. 生成 changelog 并人工过一遍（删掉不该给用户看的内部重构、补一句总述）
TAG=v0.2.0-beta.1
make changelog SINCE=$(git describe --tags --abbrev=0) VERSION=$TAG > "notes-$TAG.md"
#    无 make 的机器等价写法：
#    VERSION=$TAG bash scripts/changelog.sh "$(git describe --tags --abbrev=0)" > "notes-$TAG.md"

# 2. 打附注 tag（-F 读文件，首行即 subject）
git tag -a "$TAG" -F "notes-$TAG.md"

# 3. 本地校验：命名 / 附注 tag / 附注完整性
make tag-check TAG=$TAG          # 等价：bash scripts/verify-tag.sh "$TAG"

# 4. 推送触发 CI（推送前校验失败就不要推）
git push origin "$TAG"
```

`notes-*.md` 已加入 `.gitignore`，避免发版临时文件误入提交。Linux/macOS 首次克隆后若钩子提示无执行权限，执行 `chmod +x scripts/*.sh .githooks/commit-msg`。

**红线**：已发布 Release 的 tag 不可删除、不可移动、不可重打；CI 失败时修复后打**下一个编号**，不复用同号 tag。历史遗留的轻量 tag 与本规范上线前的单句附注 tag 保留原样，仅记录为已知异常，不做追溯修改。

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
10. **部署打包**：go:embed 前端 + 单二进制自托管（install.sh / quick_start.sh）+ 跨平台动态一键安装端点。

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
             image,
             mesh_forward_enabled, mesh_forward_host_id, mesh_forward_entry_port, mesh_forward_target_port, mesh_forward_target, # 4层 TCP 组网端口中继
             env_vars json,
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
GET    /api/v1/me/settings                 # 当前用户设置（data + schema + stored_keys），作用域 user
PUT    /api/v1/me/settings                 # {data: {key: value}} 部分更新，原子校验；落盘失败不污染内存
GET    /api/v1/settings                    # 全局设置（admin），作用域 system
PUT    /api/v1/settings                    # {data: {key: value}} 部分更新（admin）
# 键命名空间（点分）：appearance.theme_mode, appearance.show_tips,
# terminal.theme, terminal.default_shell, terminal.font_family, terminal.font_size,
# terminal.cursor_blink, terminal.scrollback, navigation.default_host_tab, files.default_path
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
POST   /api/v1/apps/inspect-image          # 镜像元数据免拉取探测 {image} → Registry V2/OCI 真实端口/环境变量/卷/启动命令（只读）
POST   /api/v1/apps/inspect-repo           # GitHub 仓库探测 {repo_url} → 默认分支/Dockerfile/Compose/.env.example 解析（只读）
POST   /api/v1/apps                        # 创建应用 {source_type, host_id, template_id/template_params | image | compose_content | repo_url/branch, mesh_forward_enabled, mesh_forward_host_id, mesh_forward_entry_port, mesh_forward_target_port, ports[{host,container,bind_scope}], ...}
GET    /api/v1/apps/:id                    # 应用详情
PUT    /api/v1/apps/:id                    # 部分更新（PATCH 语义）：未提交的键保持原值，显式 false/""/{} 才清空；可改 name/image(仅 image 来源)/healthcheck_url/env_vars/ports/volumes/compose_content/mesh_forward_*/auto_deploy/auth_vault_id 等；host_id/source_type/container_name 不可改
DELETE /api/v1/apps/:id                    # 删除
POST   /api/v1/apps/:id/deploy|rollback    # 部署 / 回滚
POST   /api/v1/apps/:id/migrate            # 跨主机迁移 {target_host_id, migrate_volumes?, clean_source?}；同步校验后 202 返回 Deployment（trigger=migrate），失败自动回滚源主机（见 §3.7「服务迁移」）
POST   /api/v1/apps/:id/migrate/precheck   # 只读预检 {target_host_id} → {agent_online, docker_running, s3_ready, s3_bucket, volume_count, port_conflicts, warnings}；读组、免审计，问题只以 warnings 上报
POST   /api/v1/apps/:id/stop|start|restart
GET    /api/v1/apps/:id/deployments        # 部署历史（分页）
POST   /api/v1/apps/:id/deployments/:depID/diagnose   # AI 排障
PUT    /api/v1/apps/:id/proxy              # 绑定域名 {domain, mode: local|gateway, gateway_host_id, cert_id?, upstream?}；无证书时用默认 ACME 账户自动签发；绑定前自动保障系统托管反代容器（ensureProxyNginx，见 §3.7）：Windows 主机 400、Docker 不可用/80-443 被占/镜像拉取失败 → 409 分类提示、其余保障失败 500+日志摘要
DELETE /api/v1/apps/:id/proxy              # 解绑（同时清理应用主机与网关主机配置）
POST   /api/v1/apps/webhook/:token         # Git push 自动部署（token 鉴权）
POST   /api/v1/github-app/webhook          # GitHub App 级 push 事件（公开、HMAC-SHA256 验签、delivery 去重、未配置 503；按 full_name 匹配应用，trigger=github-app）
GET    /api/v1/github-app/status           # App 配置与安装概览（configured/app_id/slug/webhook_url/installations）
GET    /api/v1/github-app/installations/repos  # 全部安装的仓库清单（App 令牌取数）
GET    /api/v1/github-app/repos/:owner/:repo/branches  # 仓库分支（App 令牌取数；不在安装范围 404）
POST   /api/v1/github-app/manifest|manifest/exchange|config|installations/sync  # Manifest 生成/换 code/手动配置/同步安装（admin + 审计）
DELETE /api/v1/github-app                 # 断开 GitHub App（admin + 审计）
POST   /api/v1/apps/:id/github-app/switch  # 应用切换到 GitHub App 触发（admin + 审计 app_github_app_switch；删旧逐仓 hook 失败 502）

# 按需运维隧道（§3.17，控制面；数据面走 -opgrpc 端口的 OperatorService）
GET    /api/v1/tunnels/client-info          # CLI 引导信息 {enabled, endpoint, cli{各平台 Release 直链}}；authed 读、免审计；enabled 反映数据面监听器真实状态（启动失败 fail-closed 置假），endpoint 推导链：server.public_url 设置 → -public-url 参数 → 请求 Host（IPv6 去重围括号）
GET    /api/v1/tunnels/mine                 # 当前用户活动隧道腿（conn_id/目标/建立时间/空闲秒）；authed 读、免审计
DELETE /api/v1/tunnels/mine/:id             # 强制关闭本人的隧道腿（服务层按用户名过滤，只能关自己的）；写组 + 审计 op_tunnel_close

# 组网（P2）
POST   /api/v1/networks/:id/join          # 选中主机加入
GET    /api/v1/networks/topology
DELETE /api/v1/networks/:id/hosts/:hid
GET    /api/v1/network/nodes/:id/mesh-ip  # 只读实时探测主机组网 IP（live 优先、缓存兜底，返回 source: live|cache|""）；
                                          # 挂 authed 读分组、免审计（创建页选主机即调，见 §8.7 第 8 条）；实际路由前缀为 /network

# 备份/迁移
POST   /api/v1/system/backup              # 导出 SQLite/配置/录像打包
POST   /api/v1/system/restore             # 从备份恢复（离线运维操作）
GET    /api/v1/system/health              # 自检：DB/AI/Agent 连接数等

# 证书中心（admin 写操作；读操作所有登录角色可见）
GET    /api/v1/certs                       # 已签发证书列表（响应每行带服务端派生字段 source=acme|manual_dns01|manual_upload 与 account_name，及管理员备注 remark）
GET    /api/v1/certs/cover?domain=<d>      # 覆盖探测（只读）：域名是否已有证书覆盖 {covered, cert?{id,domains,source,account_name,not_after}}（复用 FindByDomain 同源匹配：精确 SAN 或通配符覆盖子域名；PEM 剥离；登录即可、免操作审计；创建页输入防抖预检 + 签发轮询终态判定用；非法/空/通配符域名 400）
GET    /api/v1/certs/:id/detail            # 证书详情（只读）：现场解析证书链 leaf（序列号/SHA-256 指纹/密钥与签名算法/链张数）、用途反查（面板绑定/兜底、应用域名匹配）、chain_pem 仅公钥链
GET    /api/v1/certs/:id/download          # 下载公钥链附件 <主域名>.crt（仅公钥链、不打包私钥；登录即可、只读免操作审计，浏览器导航下载走 ?token= 查询参数惯例）
POST   /api/v1/certs/:id/key               # 查看私钥明文 {data:{key_pem}}（详情弹窗与证书链并列展示用；adminOnly + 审计 cert_key_reveal/high，与 AI Key 明文查看同惯例；常规响应一律剥离私钥，本端点是唯一显式出口）
POST   /api/v1/certs/issue                 # ACME 签发 {domains[], account_id?, challenge?}（异步，202 受理；challenge: ""自动 | "http-01" | "dns-01" | "dns-01-manual"，显式指定后不再自动回退）
POST   /api/v1/certs/import                # 上传已有证书 {cert_pem, key_pem}（服务端校验 PEM 与私钥匹配，入库后进入已签发列表，不自动续期）
POST   /api/v1/certs/:id/renew             # 续期（ACME 重新签发）
PUT    /api/v1/certs/:id/remark            # 编辑管理员备注 {remark}（≤200 字，仅元信息；adminOnly + 审计）
DELETE /api/v1/certs/:id                   # 删除证书
GET    /api/v1/certs/config                # dns-mng 集成配置（自动 DNS-01 验证后端）
PUT    /api/v1/certs/config                # 更新 dns-mng 集成配置
# 手动 DNS-01（两阶段，管理员自行添加 TXT 解析，不依赖 dns-mng）：
POST   /api/v1/certs/manual/:id/confirm    # challenge="dns-01-manual" 时 /certs/issue 同步创建 ACME 订单并返回待添加的 TXT 记录 {id, identifiers[], records[{domain,host,type,value}], status, expires_at}（200）；管理员在 DNS 服务商添加解析后调本接口通知 CA 开始验证（异步 202，轮询下条查询进度）
GET    /api/v1/certs/manual/:id            # 手动订单状态与 TXT 记录 {status: awaiting_dns|verifying|done|error, cert_id?, error?}
DELETE /api/v1/certs/manual/:id            # 取消待处理的手动订单（verifying 中不可取消；CA 侧订单自然过期）
# 挑战验证兼容策略：普通域名优先 HTTP-01（签发时临时监听 80 端口，无需 dns-mng；面板不常驻占用 80），
# 80 端口不可用或 HTTP-01 失败时回退 DNS-01（需配置并启用 dns-mng 自动添加解析）；通配符域名仅支持 DNS-01；
# IP 标识（RFC 8738）仅支持 HTTP-01（DNS-01 对 IP 无效，手动 DNS-01 拒绝 IP）。
# DNS-01 有两种形态：① 自动（dns-mng）：服务端调用 dns-mng 自动添加/清理 TXT 解析，全程无人值守；
# ② 手动（dns-01-manual）：服务端只计算并展示 TXT 记录，由管理员手动到 DNS 服务商添加解析后再确认签发。
# 手动订单状态机：awaiting_dns → verifying → done | error；本地先做 15s DNS TXT 预检查（未传播不通知 CA，避免浪费验证次数）；
# error 分两种：预检查/网络/超时失败可直接重新确认（terminal=false，TXT 记录不变）；CA 终态判定（authorization invalid/expired）则 terminal=true，
# 同一订单不可重试、必须新建订单。订单落盘 2 小时 TTL；verifying 中服务重启会转为可重试的 error（不会卡死），面板内存任务不持久化、重启后需重新发起。
# 手动签发的证书不自动续期（AutoRenew=false；续期走自动验证通道，与管理员选择手动的原因相悖），到期前经告警中心 builtin-cert-expiry 提醒，管理员重新走两阶段签发。
# 签发弹窗与面板一键签发均提供「验证方式」显式选择（自动 / HTTP-01 / DNS-01 / DNS-01 手动解析），显式指定后不再自动回退。

# 面板证书（宝塔式一键签发，admin）
GET    /api/v1/system/panel-cert          # 面板证书状态（ssl_enabled/panel_domain/strict_domain/active/source/subject/issuer/dns_names/days_left）
POST   /api/v1/system/panel-cert/issue    # 启动异步签发 {mode: domain|ip, ip?, challenge?}；domain 走 HTTP-01 优先（临时监听 80）、失败或无 80 时回退 DNS-01（需证书中心 dns-mng；通配符仅 DNS-01），ip 走 HTTP-01（临时监听 80）；challenge 显式指定验证方式（含 "dns-01-manual" 手动两阶段：202 返回 awaiting_dns job 并携带 dns_records）；202 返回 job
GET    /api/v1/system/panel-cert/issue/:job_id  # 签发任务进度 {status: running|awaiting_dns|done|error, cert_id?, error?, pending_id?, dns_records?}
POST   /api/v1/system/panel-cert/issue/:job_id/confirm  # 手动 DNS-01 任务：管理员添加 TXT 解析后通知 CA 开始验证（异步 202；成功自动绑定面板并启用 HTTPS）
DELETE /api/v1/system/panel-cert/issue/:job_id  # 取消手动 DNS-01 签发任务（同时取消证书中心待处理订单）
GET    /api/v1/system/panel-cert/public-ip      # 检测服务器公网出口 IP
```

### B.8.3 协议补充

- **DockerOp/DockerEvent（P2）**：`op` 含 `ps/images/start/stop/restart/rm/logs/inspect/remove_image/prune_images/pull/run`；Agent 侧 shelling out to the docker CLI 执行（`agent/internal/docker/docker.go`）。`run` 的参数走 `DockerOp.args_json`：`{"name":"...","ports":["hostPort:containerPort"],"volumes":["/host:/container"],"env":["KEY=VALUE"],"restart_policy":"no|on-failure|always|unless-stopped","command":["arg1","arg2"]}`，空值条目跳过；**每个字段先过字符白名单正则（容器名/端口/卷/环境变量键/重启策略/镜像引用，拒绝以 `-` 开头等伪装 docker flag 的载荷）再进 argv，exec 不走 shell**；镜像缺失由 docker CLI 自动拉取，故 `pull/run` 的服务端下发与 agent 执行均取 5 分钟超时。
- **安全扫描（P3）**：新增 `ScanRequest(type, rule_set)` / `ScanResult(progress, findings json, report_ref)`，走 exec + 结果回传，不引入新通道。
- **Agent 自更新**：新增 `UpgradeRequest(version, url, sha256)` / `UpgradeProgress`，控制端可下发升级，Agent 校验签名后热更新（解耦发布）。升级签名密钥由控制端启动时从 `vault-pass` 经域分隔 SHA-256 派生 Ed25519（重启稳定、无需新密钥文件，`api.InitSigner`），公钥 hex 经 `GET /api/v1/version` 的 `upgrade_pubkey` 公开；agent 安装脚本自动带上 `-upgrade-pubkey`，agent 验签（未配置则跳过，只校验 sha256）。下载 URL 优先 `-public-url`，未配置时回退到请求 Host（解决反代/NAT 下 Host 不可信）。
- **Agent 卸载**：`UninstallRequest(remove_data)`（proto field 15）。三条路径：① 控制端解绑：`DELETE /api/v1/hosts/:id?uninstall_agent=true`（adminOnly），在线则下发卸载指令并等 agent 下线（15s 超时），离线直接 409（记录不删，提示先上线或去掉勾选）；超时未下线仍删记录但返回 `warning`。② agent 自卸载：`agent/internal/uninstall`，Linux 先 `signal.Ignore(SIGINT/SIGTERM)` 保命（**绝不能 `systemctl stop` 自己**：stop 的 SIGTERM 会杀死当前进程使清理中断；忽略信号后 stop 又会卡到 TimeoutStopSec 才被 SIGKILL），只做 disable + 删 unit + daemon-reload（unit 删掉后 systemd 即便 Restart=always 也无法重启已不存在的 unit）→ 删自身二进制 → remove_data=true 才删 data dir（路径须含 watchman 且非系统根目录）；sysvinit 同理只删脚本不 stop。Windows 删 Scheduled Task `WatchmanAgent`（安装脚本注册的是计划任务**不是** Windows service，`sc.exe` 找不到对象；`schtasks /Delete` 只删任务定义不杀当前运行实例）+ 延迟 cmd 自删（运行中 exe 删不掉，remove_data=false 时不拼 rmdir）。③ 安装脚本：`bash -s -- uninstall` / 落盘后 `& $f -Uninstall`（管道给 iex 传不了 switch 参数），停服务、删二进制与数据目录。
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

4. **版本权威性与唯一来源准则 (Release-Driven Authority)**：
   - **版本完全由官方 Release 提供，严禁任何自行决定或拼接行为**：系统所有组件（控制端 Server、被管端 Agent、升级清单 Manifest 等）的版本号唯一来源必须是 Git tag 触发的官方 GitHub Release（形如 `v1.2.3` 或 `v1.2.3-beta.1`）。
   - **绝对禁止私自捏造/拼接版本**：禁止在开发、部署或测试脚本中自行生成伪版本号（例如取本地 git commit hash 拼凑 `0.1.0-<hash>` 或任意自定义后缀）；未经 tag 发布的代码版本一律为开发环境占位（`0.1.0-dev`），线上与测试环境的升级验证必须且只能来源于官方 Release 产物与由 CI 生成的 `manifest.json`，确保版本链路 100% 可追溯。
   - **严禁裸 tag，必须完整编写变更说明 (Changelog)**：创建 Git tag 时严禁使用无任何说明的裸 tag（如仅 `git tag v1.2.3`）。每次打 tag 发版必须使用附注 tag（`git tag -a vX.Y.Z -m "..."`），并在 tag message 与 GitHub Release notes 中详细枚举 Changed（行为变更/优化）、Fixed（缺陷修复）、Added（新增功能）、Security（安全修复）等内容，确保版本演进清晰透明、便于追踪定位。
   - **必须区分附注 tag 与轻量 tag**：`git tag -a` 创建的是附注 tag（对象类型 `tag`），`git tag` 不带 `-a` 创建的是轻量 tag（对象类型 `commit`）。后者在 `%(contents)` 下会退化为指向 commit 的 message，导致 Release 正文显示「最近一条提交」而非版本汇总（2026-10-28 已确认事故）。发版前后必须执行 `make tag-check TAG=<版本>`，详见 §B.6.5 与 §B.6.6。
   - **提交信息即变更日志的数据源**：Release 正文由 `scripts/changelog.sh` 从提交历史聚合生成，提交信息必须符合 Conventional Commits（type/scope/subject 规范见 §B.6.6），执行 `make hooks` 启用本地校验；不合规提交会直接表现为该版本 Release notes 缺少对应条目。

5. **操作审计全覆盖 (Audit Coverage)**：
   - **所有改变系统/主机状态的写操作必须记审计**：凡是 `POST/PUT/PATCH/DELETE` 且会产生状态变更的 API（创建/更新/删除配置、启停/部署/回滚应用、签发/续期证书、文件增删改、执行命令、绑定解绑域名、授权变更、删除会话录像等），必须有一条审计记录（谁、何时、做了什么、目标对象、结果、风险等级）。
   - **实现方式**：优先走 `server/internal/api/router.go` 的 `auditMutation()` 中间件——在 `deriveMutationAction` 里为新路由添加规则（action 名、targetType、targetParam、risk、desc）；中间件覆盖不到的单个 handler（如异步任务启动、证据删除）用 `h.recordAudit(...)` 显式记录。
   - **新增写路由必须同步加规则**：`deriveMutationAction` 的规则表是审计覆盖的唯一事实来源；新增任何写操作路由时，必须同时在规则表里加一条，并在 `server/internal/api/audit_coverage_test.go` 的 `TestDeriveMutationActionCoverage` 里加一条用例锁住它。Review 时发现写路由无规则 = 缺陷。
   - **审计文案必须全中文、面向操作而非接口**：审计列表「操作」列与「详情」列对用户必须是中文自然语言——规则的 `desc` 就是详情列展示内容，必须是「部署应用」「续期证书」这类操作描述，**严禁写 `POST /api/v1/apps/:id/deploy` 这类方法+路由原文**（中间件仅在 desc 缺失时兜底）；显式埋点的 detail 同样逐条写中文描述。测试 `TestDeriveMutationActionCoverage` 对全规则表断言 desc 非空，新增规则漏写 desc 会直接测试失败。
   - **操作列中文标签的唯一事实来源**是 `web/src/api/audit.ts` 的 `actionLabels`（action → 中文）、`actionTypeLabels`（`update`/`install`/`join`/`leave` 等短码按 `action:target_type` 精确匹配，如 `update:system` = 上传控制端二进制、`join:network_node` = 加入异地组网）、`targetTypeLabels`（目标类型）、`riskLabels`/`resultLabels`（风险/结果）；`AuditList.vue` 只消费这些映射，**新增 action 码必须同步补标签**，未登记的动作回落展示原始码（Review 时看到英文 action = 漏登记）。
   - **豁免**：纯读（GET）、诊断/分析类（AI diagnose、部署诊断）、机器对机器回调（Agent enroll、应用 webhook 回调）不记审计；失败请求（HTTP >= 400）由中间件自动跳过，显式埋点自行决定。
   - **风险分级**：删除/恢复/签发/授权变更/流量切换类记 `high`；常规增改记 `medium`；登出、连通性测试、告警确认等记 `low`。
   - **CSV 导出口径**：`GET /api/v1/audit/export` 导出原始 action 码与字段值（机器消费用），不做中文翻译；中文展示只发生在面板 UI 层。

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
- **页签内的滚动容器必须显式 `height: 100%`（并配 `min-height: 0`）**：`overflow-y: auto` 单独不成立。NTabPane 经 `:deep(.n-tab-pane) { height: 100% }` 拿到确定高度，但子容器高度默认 `auto` 会按内容长高、越过页签底边，而外层（如 `AppDetail` 的 `.detail-body`）是 `overflow: hidden` —— 结果既没有滚动条，超出部分又被裁掉，用户看不到也滚不到（2026-10-22 「数据卷备份」页归档表被裁掉底部即此因，`.overview-scroll` 已补 `height: 100%`）。验收口径：容器 `scrollHeight > clientHeight` 时必须能取到大于 0 的 `scrollTop` 最大值。

### 8.3.1 NSelect 空值约定（2026-10-19）

- **绑定 NSelect `v-model:value` 的字段一律用 `null` 做空值**（声明 `null as string | null`），**严禁 `''`**：naive-ui 对未命中选项的值会走默认 `fallbackOption` 合成一个 `label` 为空串的真实选项，select 自认为「已选中」，占位符永不渲染（幽灵选中态）。`null` 才被识别为空，占位符正常显示。
- 例外：选项列表里**真实存在 `value: ''` 条目**时才允许用 `''`（如 AppDetail 证书选择的「按域名自动匹配」）。
- 连带约束：清空该值的代码（watcher、重置）必须写 `null`；提交载荷用 `|| undefined` 归一化；`Map.get(id)` 等强 string 入参处先判 truthy。AppCreate/AppDetail 已全部按此改造。

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

5. **版本号必须是构建产物，不能硬编码，且完全由 Release 提供**：
   - Agent 上报的 `RegisterRequest.agent_version` 与控制端的 `CurrentAgentVersion` 一律取自 `internal/version`（`version.Get()`），发布时用 `make VERSION=v1.2.3 build-all` 注入（`-ldflags -X watchman/internal/version.Version=...`）。
   - **版本号完全由官方 Release 提供，绝对禁止自行决定/拼接**：严禁在任何安装、部署、升级或测试脚本中自行生成或捏造版本（例如拼接 commit hash `0.1.0-<hash>`）；所有组件版本与升级清单 `manifest.json` 必须且只能来源于官方 GitHub Releases，确保版本权威性与可复现性。
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

4. **下发到远端的命令里所有变量必须按目标 shell 转义**：
   - `joinNode` 曾把 `--hostname`/`--advertise-routes`/`--login-server`/`--authkey` 直接拼进 shell 命令字符串（`pingNode` 用了 `shellQuoteArg` 而这里漏了），operator 可注入任意命令，且审计只记"加入组网"、看不到真实命令。已收敛为 `buildJoinArgs` 统一经 `shellQuoteArg(shell, …)` 转义。
   - `shellQuoteArg` 按目标 shell 选择转义：PowerShell 单引号内用 `''` 转义单引号，POSIX 用 `'\''`；无特殊字符的值原样返回。

5. **密钥永不经 GET 接口回显**：
   - `GET /network/config` 曾明文返回 Tailscale Auth Key，而该接口挂在 `authed` 分组（所有登录用户，含只读 viewer）——viewer 拿 key 就能把自己的设备加进 tailnet，直达所有 100.x 节点。现只返回 `auth_key_set` 布尔值；PUT 空值表示"保持原密钥"（无清除语义，轮换请写入新值）；前端密钥输入框只写不读，保存后清空。

6. **部署用 Mesh IP 必须实时探测，缓存只做兜底**：
   - `bind_scope=mesh` 部署时（`apps/deploy.go:resolveMeshIP`）先对目标主机实时执行 `tailscale ip -4`（1 次 exec，无参数、与 shell 无关、无需转义），命中则直接采用并回写 `network.Store` 缓存（网关反代与 UI 同步受益）。
   - 实时探测失败时才用缓存 IP，并写 `WARN: 异地组网 IP 实时探测失败，使用缓存值 …` 明示；两者皆无才降级公网绑定。**禁止**在探测失败时静默降级公网——用户选了 mesh-only，静默公网暴露是安全回退。
   - 背景：`--reset` 重加入会让 Headscale 重新分配 100.x，纯缓存方案（`checkNode`/`joinNode` 才刷新）会拿过期 IP 去 `docker -p` 绑定，导致部署失败。网关反代侧（`proxy.go`）保持读缓存（per-request exec 不可行），由部署/探测/加入时的刷新覆盖。

7. **Alpine 一键安装对照官方 installer.sh 补齐预检**：
   - `installNode` 的 `sh` 分支（`buildInstallCommand`）先检查 `/etc/apk/repositories` 是否启用 community（`grep -Eq '^http.*/community$'`），缺失时 `setup-apkrepos -c -1` 自动启用；两者皆无则明确报错退出，不再是泛化的"安装脚本执行失败"。
   - 服务启动用 `rc-service tailscale start || rc-service tailscale restart`（与官方一致走 `start` 优先）：`restart` 在从未启动过的服务上退出码不可靠，曾导致"装好了但 UI 报失败"的误报。

8. **只读实时探测端点 `GET /network/nodes/:id/mesh-ip`（2026-10-16）**：
   - 创建应用页选中主机即调用：实时 `tailscale ip -4`（1 次 exec）优先，命中则采用并**回写 Store 缓存**（网关反代与网络列表同步受益）；失败回落缓存 `NodeStatus.IP`，两者皆无返回空。响应 `{ip, source}`，`source` ∈ `live`/`cache`/`""`（未加入组网或探测失败），前端据此标注「实时探测/缓存」。
   - **必须挂在 authed 读分组、免审计**：`POST /network/nodes/:id/check` 虽能刷新缓存，但在 writeRG（审计），每次选主机都调会灌爆操作审计流；诊断类只读探测按 §7 豁免（与 `ai/nl2command` 同例）。
   - 部署侧（`resolveMeshIP`）保持自己的实时探测逻辑不读此端点——部署在 agent 侧执行，与此 API 互为独立实现，均遵循"live 优先、cache 兜底、禁止静默降级公网"。

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

### 8.3 功能提示语的可开关约定

- 各页面**说明性**的帮助段落（`<p class="muted">` 这类功能说明文字）在 `class` 上追加 `tip-hint`，受用户设置 `appearance.show_tips` 控制：App.vue 按设置给 `documentElement` 切换 `hide-tips` 类，全局样式隐藏 `.tip-hint`。
- **警告/报错类 NAlert、数据展示用的 muted 文本不加** `tip-hint`，开关只隐藏说明文字，不隐藏业务信息。






