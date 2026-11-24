# 控制端 REST API

> 接口契约的唯一事实源。行为、流程与实现约束见 `AGENTS.md` 对应章节；前端布局与交互见 `DESIGN.md`。
>
> 基础路径 `/api/v1`（另有说明的除外）。表格只列契约（方法、路径、权限、关键入参与返回），不写流程细节。

## 通用约定

- **鉴权**：除 `POST /auth/login`、`POST /hosts/enroll` 与标注「公开」的端点外，均需 JWT。
- **分组权限**：按分组校验 `host_id` 的访问范围。
- **审计**：写操作记审计（规则见 `AGENTS.md` §7）。标注「免审计」的只读/诊断端点不记。
- **角色**：`admin` / `operator` / `viewer`。标注 `adminOnly` 的仅管理员；未标注的写操作一般为 admin + operator。
- **分页**：列表接口用 `?offset=&page_size=`（应用部署历史等个别接口用 `?page=`，见各表备注）。
- **错误**：非 2xx 返回 `{error}`；需要区分原因的接口另带结构化字段（如 `missing_domains`、`warnings`），见各表备注。

## 认证

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/auth/login` | `{username, password}` → `{token}` |
| POST | `/auth/logout` | 登出 |
| GET | `/auth/me` | 当前登录用户 |

## 主机

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/hosts?group=&status=&q=` | 主机列表 |
| POST | `/hosts/enroll` | 生成 `enroll_token` + 安装命令（公开）。`install` = Linux 一键脚本，`install_win` = Windows 一键脚本 |
| GET | `/hosts/:id` | 主机详情 |
| DELETE | `/hosts/:id` | 删除主机（admin） |
| PUT | `/hosts/:id/group` | 改分组（admin；多分组传 `groups` 数组） |
| PUT | `/hosts/:id/tags` | 改标签（admin） |
| PUT | `/hosts/:id/billing` | 改计费信息（admin） |
| POST | `/hosts/:id/unbind` | 解绑主机 |
| POST | `/hosts/:id/upgrade` | 升级该台 Agent |
| POST | `/hosts/:id/traffic/correct` | 月流量手动校正（admin）。Agent 须在线，离线 503；旧版 Agent 静默忽略 |

## Docker

`POST /hosts/:id/docker/:op` 的 `op` 透传给 Agent，清单见 `AGENTS.md` §B.7.3。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/hosts/:id/docker/ps` | 容器列表 |
| GET | `/hosts/:id/docker/images` | 镜像列表 |
| GET | `/hosts/:id/docker/all` | 容器 + 镜像一次往返 |
| GET | `/hosts/:id/docker/system-df` | 深度清理预览（`docker system df`） |
| GET | `/hosts/:id/docker/mirrors` | 取 Docker daemon `registry-mirrors` |
| PUT | `/hosts/:id/docker/mirrors` | 更新 `registry-mirrors` 并 reload daemon |
| POST | `/hosts/:id/docker/:op` | Body `{container, image, args_json?}`。写操作记审计，`rm` / `rmi` / `prune` 为 high risk；`pull` / `run` 下发超时 5 分钟 |
| POST | `/hosts/:id/docker/install-script` | 一键安装 Docker（服务端内置官方脚本，前端 330 秒超时） |
| POST | `/hosts/:id/system-cleanup` | 系统清理执行。hostWrite 角色，高危审计，5 分钟超时 |

## 系统状态

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/hosts/:id/sysinfo/:kind` | 系统状态。`kind` = `processes` / `ports` / `users` / `logins` / `cleanup`（清理预览）/ `firewall`（防火墙后端、状态与规则，Linux v1） |
| POST | `/hosts/:id/processes/:pid/kill` | 结束进程 |
| POST | `/hosts/:id/firewall/allow` | 放行防火墙端口。Body `{port, protocol?}`（`protocol` = `tcp`/`udp`，缺省 `tcp`）；9090 硬拒；hostWrite 角色，高危审计；重复放行幂等成功 |
| POST | `/hosts/:id/firewall/deny` | 拒绝防火墙端口。Body 同上；9090 硬拒；hostWrite 角色，高危审计；重复拒绝幂等成功 |
| POST | `/hosts/:id/firewall/delete` | 删除防火墙规则。Body `{port, protocol?}`，按端口/协议定位规则删除；hostWrite 角色，高危审计 |

## 监控与告警

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/hosts/:id/metrics?metric=&from=&to=&live=` | 监控指标 |
| GET | `/hosts/:id/metrics/history` | 监控历史 |
| GET/POST | `/alerts/rules` | 告警规则列表 / 创建 |
| PUT/DELETE | `/alerts/rules/:id` | 告警规则修改 / 删除 |
| GET | `/alerts/events` | 告警事件列表 |
| POST | `/alerts/events/:id/ack` | 确认单条告警事件 |
| POST | `/alerts/events/ack-all` | 全部确认 |
| DELETE | `/alerts/events/:id` | 删除告警事件 |
| GET/PUT | `/alerts/webhook` | 告警 Webhook 配置 |
| POST | `/alerts/webhook/test` | 测试告警 Webhook |

## 安全扫描

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/hosts/:id/scans` | 发起扫描 |
| GET | `/hosts/:id/scans/latest` | 最新扫描结果 |
| GET | `/scans` | 扫描报告列表 |
| GET | `/scans/:id` | 单条扫描报告 |

## 终端

数据走 WS，见文末 WebSocket 一节。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/hosts/:id/terminals` | 创建终端会话 → `{session_id, ws_url}` |
| POST | `/hosts/:id/terminals/:sid/share` | 终端会话分享 |
| GET | `/terminals/share/:token` | 分享信息（公开） |
| DELETE | `/terminals/:sid` | 关闭终端会话 |
| GET | `/sessions?host_id=&from=&to=` | 会话历史 + 录像 |
| GET | `/sessions/:id` | 单条会话 |
| GET | `/sessions/:id/recording` | 会话录像 |
| DELETE | `/sessions/:id` | 删除会话 |

## 文件

上传 / 下载走 WS（大文件分片 + 断点续传）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/hosts/:id/files?path=` | 列目录 |
| GET | `/hosts/:id/files/stat?path=` | 单文件 stat |
| GET | `/hosts/:id/files/download?path=` | 下载文件 |
| GET | `/hosts/:id/files/content?path=&offset=&length=` | 读文件内容（默认前 8KB，上限 1MB，只读） |
| POST | `/hosts/:id/files/mkdir` | 建目录 |
| POST | `/hosts/:id/files/move` | 移动 |
| POST | `/hosts/:id/files/copy` | 复制 |
| POST | `/hosts/:id/files/upload` | 上传文件 |
| DELETE | `/hosts/:id/files` | 删除文件 / 目录 |
| POST | `/hosts/:id/files/chmod` | `{path, mode}`，mode 0–07777 |
| POST | `/hosts/:id/files/compress` | `{path}` → `{archive, size}`，tar.gz，服务端等 120 秒 |

## 推送命令

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/hosts/:id/exec` | 单台执行命令 |
| POST | `/hosts/batch-exec` | 批量执行命令 |
| GET/PUT | `/policy/command` | 命令执行策略 |
| GET | `/policy/command/audit` | 命令策略审计 |

## 分组 / 用户 / 常用命令

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/groups` | 分组列表 |
| GET | `/groups/:id` | 单个分组 |
| POST | `/groups` | 创建分组（admin） |
| PATCH | `/groups/:id` | 修改分组（admin） |
| DELETE | `/groups/:id` | 删除分组（admin） |
| GET | `/groups/:id/users` | 分组已授权用户 |
| POST | `/groups/:id/users` | 授权用户（admin） |
| DELETE | `/groups/:id/users/:username` | 取消用户授权（admin） |
| GET | `/me/groups` | 当前用户所属分组 |
| POST | `/users` | 创建用户 |
| PUT | `/users/:username/password` | 改密码（admin） |
| PUT | `/users/:username/role` | 改角色（admin） |
| POST | `/users/:username/reset-password` | 重置密码（admin） |
| DELETE | `/users/:username` | 删除用户（admin） |
| GET/POST/PATCH/DELETE | `/commands` | 常用命令库 |
| POST | `/commands/:id/use` | 使用常用命令 |

## 设置

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/me/settings` | 当前用户设置（`data` + `schema` + `stored_keys`），作用域 user |
| PUT | `/me/settings` | `{data: {key: value}}` 部分更新，原子校验；落盘失败不污染内存 |
| GET | `/settings` | 全局设置（admin），作用域 system |
| PUT | `/settings` | `{data: {key: value}}` 部分更新（admin） |

键命名空间（点分）：`appearance.theme_mode`、`appearance.show_tips`、`terminal.theme`、`terminal.default_shell`、`terminal.font_family`、`terminal.font_size`、`terminal.cursor_blink`、`terminal.scrollback`、`navigation.default_host_tab`、`files.default_path`。

秘密（AI key / Git token 等）不进统一设置，留在各自专用存储。

## 审计

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/audit?user=&action=&from=&to=` | 审计日志查询 |
| GET | `/audit/export` | 导出审计日志 |

## AI

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/PUT | `/ai/config` | AI provider / 模型 / 开关 / 限速。GET 中 `api_key` 恒为掩码哨兵 `********`；PUT 传哨兵 = 保留原密钥 |
| POST | `/ai/config/key` | 明文查看已保存的 AI API Key（adminOnly，审计 `ai_config_key_reveal` / high） |
| POST | `/ai/models` | 探测端点可用模型列表（OpenAI 兼容 `/models` / Ollama `/api/tags`；只读诊断、免审计；表单当前值可直接探测，未保存也可测；空值与密钥哨兵回落已保存配置；模型字段下拉选择或手动输入） |
| GET/POST | `/ai/chat` | 自然语言问答（tool calling；响应含 trace 调用步骤与 usage token 量） |
| POST | `/ai/nl2command` | 自然语言转命令（带 `host_id` 上下文） |
| POST | `/ai/app-draft` | 需求描述 → 创建应用表单草稿（服务端消毒，不创建；响应含事实补齐的 env / volumes 与 sources 参考来源、trace 调用步骤与 usage token 量） |
| POST | `/ai/app-chat` | 创建页对话辅助（携带表单快照，可返回 `form_patch` 草稿，patch 同样经事实补齐并带 sources、trace 调用步骤与 usage token 量） |
| POST | `/ai/analyze-exec` | 执行结果分析 |
| POST | `/ai/diagnose` | 诊断 |
| POST | `/ai/plan` | 运维计划 |
| POST | `/ai/scan-report` | 扫描报告解读 |
| POST | `/ai/scan-report/followup` | 扫描报告追问 |
| POST | `/ai/test` | 模型连通性测试 |
| POST | `/ai/ops-report` | 生成运维报告 |
| GET | `/ai/ops-reports` | 运维报告列表 |
| GET | `/ai/ops-reports/:id` | 单条运维报告 |

## 凭据保险库

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/POST | `/vault/credentials` | 凭据列表 / 创建 |
| GET/PUT/DELETE | `/vault/credentials/:id` | 单条凭据读写 / 删除 |

## 应用中心

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/apps` | 应用列表（全部来源统一管理） |
| POST | `/apps` | 创建应用（创建即自动入队首轮部署） |
| GET | `/apps/catalog` | 内置模板目录（声明式 Catalog） |
| GET | `/apps/:id` | 应用详情 |
| PUT | `/apps/:id` | 部分更新（PATCH 语义）：未提交的键保持原值，显式 `false` / `""` / `{}` 才清空。`host_id` / `source_type` / `container_name` 不可改；镜像地址仅 `image` 来源可改 |
| DELETE | `/apps/:id?purge=` | 删除应用（可选清除容器与镜像） |
| POST | `/apps/:id/deploy?force=` | 触发部署（`force=true` 支持终止在途任务并重新部署） |
| POST | `/apps/:id/upgrade` | 升级应用：先拉取最新镜像再重新部署（单条部署记录，`trigger=upgrade`；见 `AGENTS.md` §3.7「应用升级」） |
| POST | `/apps/:id/deployments/cancel` | 终止当前正在进行的部署任务 |
| POST | `/apps/:id/rollback` | 版本回滚 |
| POST | `/apps/:id/stop` | 停止 |
| POST | `/apps/:id/start` | 启动 |
| POST | `/apps/:id/restart` | 重启 |
| POST | `/apps/:id/migrate` | 跨主机迁移 `{target_host_id, migrate_volumes?, clean_source?}`；同步校验后 202 返回 Deployment（`trigger=migrate`），失败自动回滚源主机 |
| POST | `/apps/:id/migrate/precheck` | 只读预检 `{target_host_id}` → `{agent_online, docker_running, s3_ready, s3_bucket, volume_count, port_conflicts, warnings}`；读组、免审计，问题只以 `warnings` 上报 |
| GET | `/apps/:id/deployments` | 部署历史（分页；列表不含 `build_log`，正文走单条接口） |
| GET | `/apps/:id/deployments/:depID` | 单条部署记录。运行中时 `build_log` 叠加引擎内存缓冲（实时日志） |
| POST | `/apps/:id/deployments/:depID/diagnose` | AI 排障 |
| GET | `/apps/:id/status` | 应用状态 |
| PUT | `/apps/:id/proxy` | 绑定域名（全量替换） |
| DELETE | `/apps/:id/proxy` | 解绑（同时清理应用主机与网关主机配置） |
| POST | `/apps/:id/webhook/sync` | 同步 webhook |
| GET/POST | `/apps/webhook/:token` | Git push 自动部署（token 鉴权） |
| POST | `/apps/inspect-image` | 镜像元数据免拉取只读探测 `{image}` → Registry V2 / OCI 真实端口 / 环境变量 / 卷 / 启动命令 |
| POST | `/apps/inspect-repo` | GitHub 仓库探测 `{repo_url}` → 默认分支 / Dockerfile / Compose / `.env.example` 解析（只读） |
| POST | `/apps/:id/github-app/switch` | 切换到 GitHub App 触发（非 github.com 仓库 400；有旧逐仓 hook 时先删，失败 502 不改库） |

### 应用中心：域名绑定（`PUT /apps/:id/proxy`）

Body `{domains: [{domain, cert_id?, force_https?}], mode: local|gateway, gateway_host_id?, upstream?, websocket?}`（兼容旧单值 `domain` / `cert_id`）。

- `cert_id` 省略 / 空 = 服务端按域名自动匹配当前可用证书并在保存时物化具体 ID（`auto=true`）。
- `force_https` 省略 / 关 = 80 直接明文访问；开 = 80 跳转 https。
- 任一域名无可用证书时整批 400 + `{error, missing_domains}`，此时未写任何主机。
- 显式 `cert_id` 仍严格校验存在性 / SAN 覆盖 / 材料。
- 绑定前自动保障系统托管反代容器（`ensureProxyNginx`，见 `AGENTS.md` §3.7）：Windows 主机 400；Docker 不可用 / 80–443 被占 / 镜像拉取失败 → 409 分类提示；其余保障失败 500 + 日志摘要。

### 应用中心：挂载卷备份

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/apps/:id/volumes` | 应用当前挂载卷清单。单容器取声明卷并补挂载点，Compose 取 Agent 在线动态发现；动态发现失败且静态解析无结果时返回错误而非空列表；只读 |
| GET | `/apps/:id/volume-backups?volume=&offset=&page_size=` | 挂载卷备份归档列表（登录即可；含 `last_restore_at` / `status` / `error` 与 `s3:{ready, target_id, target_name, bucket}` 去向块） |
| POST | `/apps/:id/volume-backups` | 备份挂载卷。202 异步；见下 |
| GET | `/apps/:id/volume-backups/:archiveID/download` | 下载归档（只读、免审计；仅 success，302 到 15 分钟预签名 S3 GET，目标已删除 410） |
| POST | `/apps/:id/volume-backups/:archiveID/restore` | 恢复归档（仅 success；下载 S3 归档并校验 SHA256 后停机 → 安全副本 → 解压 →（重）启动） |
| DELETE | `/apps/:id/volume-backups/:archiveID` | 删除归档（含 S3 对象与宿主暂存 / 安全副本；running 归档 409） |
| POST | `/apps/:id/volume-backups/upload-scan` | 上传并识别备份文件（multipart 多 file，≤10 份，单份 2GB 上限；流式落盘时服务端算 SHA256，扫 tar 内容自动识别每份文件的目标挂载卷，返回识别 / 暂存结果供确认） |
| POST | `/apps/:id/volume-backups/upload-commit` | 确认配对并批量恢复。Body `{pairs: [{archive_id, specs[]}]}`，≤10 对，202 异步 |
| GET | `/volume-uploads/:token` | Agent 一次性下载上传文件（公开、无 JWT，token 即凭证；安全入口豁免） |

`POST /apps/:id/volume-backups` 的 Body：`{volumes?: string[], note?≤200 字, target_id?, consistency_mode?, retention?}`。

- 归档直传页内所选 S3 目标；`target_id` 省略时回落默认目标，两者皆无 400；未知 `target_id` 400。
- 仅已声明卷。`retention` 缺省 / 0 = 手动备份不裁剪，显式 1~100 才按窗口裁剪。
- 卷忙 / 部署中 409、Agent 离线 503。

## GitHub App

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/github-app/status` | App 配置与安装概览（`configured` / `app_id` / `slug` / `webhook_url` / `installations`） |
| GET | `/github-app/installations/repos` | 全部安装的仓库清单（App 令牌取数） |
| GET | `/github-app/repos/:owner/:repo/branches` | 仓库分支（App 令牌取数；不在安装范围 404） |
| POST | `/github-app/manifest` | 生成 Manifest（`name` 可空 → 自动生成唯一名；`state` 内存 15 分钟过期） |
| POST | `/github-app/manifest/exchange` | 验 `state` 后换 code 落盘（私钥可用性校验） |
| POST | `/github-app/config` | 手动填写 App ID / 私钥 / Webhook Secret（私钥可用性校验） |
| POST | `/github-app/installations/sync` | 同步安装与仓库清单 |
| DELETE | `/github-app` | 断开（清配置与安装） |
| POST | `/github-app/webhook` | 公开端点（HMAC-SHA256 验签、恒时比较、delivery 去重、body 4MB 上限；未配置 503；ping 200；仅 `refs/heads/` 分支推送触发，tag 跳过；按 `full_name` 匹配应用，`trigger=github-app`） |

## Git 仓库接入（经典 PAT）

与 GitHub App 互补的老接入方式。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/git/providers` | 已接入的 Git 供应商 |
| GET | `/git/github/repos` | 仓库列表 |
| GET | `/git/github/repos/:owner/:repo/branches` | 仓库分支 |
| POST | `/git/github/token` | 保存 GitHub Token |
| DELETE | `/git/github` | 断开 GitHub 接入 |

## 按需运维隧道

控制面。数据面走 `-opgrpc` 端口的 OperatorService。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/hosts/:id/tunnels` | 在目标主机上开启隧道 |
| GET | `/hosts/:id/tunnels` | 该主机的隧道列表 |
| DELETE | `/hosts/:id/tunnels/:tid` | 关闭隧道 |
| GET | `/tunnels/client-info` | CLI 引导信息 `{enabled, endpoint, cli{各平台 Release 直链}}`；authed 读、免审计；`enabled` 反映数据面监听器真实状态（启动失败 fail-closed 置假） |
| GET | `/tunnels/mine` | 当前用户活动隧道腿（`conn_id` / 目标 / 建立时间 / 空闲秒）；authed 读、免审计 |
| DELETE | `/tunnels/mine/:id` | 强制关闭本人的隧道腿（服务层按用户名过滤，只能关自己的）；写组 + 审计 `op_tunnel_close` |

## 动态组网与网络拓扑

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/PUT | `/network/config` | 组网配置 |
| GET | `/network/nodes` | 组网节点列表 |
| GET | `/network/nodes/:id` | 单个节点 |
| POST | `/network/nodes/:id/check` | 节点连通检查 |
| POST | `/network/nodes/:id/install` | 节点安装组网客户端（异步：立即返回 `202 {task_id, status:"running"}`，轮询任务接口取结果） |
| POST | `/network/nodes/:id/join` | 选中主机加入组网（异步：立即返回 `202 {task_id, status:"running"}`，轮询任务接口取结果；成功回 `ip` / `previous_ip` / `ip_changed`） |
| GET | `/network/nodes/:id/tasks/:taskId` | 组网任务状态（`{task_id, host_id, kind, status: running\|success\|failed, message, detail, ip, previous_ip, ip_changed, duration_ms}`；只读免审计；结果保留 10 分钟） |
| POST | `/network/nodes/:id/leave` | 节点离开组网 |
| POST | `/network/nodes/:id/ping` | 节点间 ping |
| GET | `/network/nodes/:id/mesh-ip` | 只读实时探测主机组网 IP（live 优先、缓存兜底，返回 `source: live\|cache\|""`）；挂 authed 读分组、免审计 |

## 快照备份（任务 / S3 目标）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET/POST | `/backups/jobs` | 备份任务列表 / 创建 |
| PUT/DELETE | `/backups/jobs/:id` | 备份任务修改 / 删除 |
| POST | `/backups/jobs/:id/run` | 立即执行备份任务 |
| GET | `/backups/jobs/:id/archives` | 任务归档历史 |
| POST | `/backups/jobs/:id/archives/:archiveID/restore` | 从归档恢复 |
| DELETE | `/backups/jobs/:id/archives/:archiveID` | 删除归档 |
| GET/POST | `/backups/s3-targets` | S3 存储目标列表 / 创建 |
| PUT/DELETE | `/backups/s3-targets/:id` | S3 目标修改 / 删除 |
| POST | `/backups/s3-targets/:id/test` | 测试 S3 目标连通 |

## 系统备份 / 恢复 / 升级

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/version` | 服务端版本（server 构建信息 + `agent_target_version` + `agent_manifest` + `upgrade_pubkey` + `public_url` + `timezone`） |
| POST | `/system/backup` | 导出 SQLite / 配置 / 录像打包 |
| POST | `/system/restore` | 从备份恢复（离线运维操作） |
| GET | `/system/backups` | 控制端备份列表 |
| GET | `/system/backups/:name/download` | 下载备份归档 |
| DELETE | `/system/backups/:name` | 删除控制端备份 |
| POST | `/system/backups/:name/restore` | 从指定备份恢复 |
| GET | `/system/health` | 自检：DB / AI / Agent 连接数等 |
| GET | `/system/check-update?force=&beta=` | 查询官方 Release 最新版本、更新日志与升级包（带通道与缓存） |
| POST | `/system/online-upgrade` | 控制端一键在线自升级（下载官方包、校验 SHA-256、原子替换、维护握手平滑重启） |
| POST | `/system/upgrade` | 离线上传 watchman-server 单二进制升级（multipart `file`，可带 `sha256`） |
| POST | `/system/upgrade-agents` | 批量升级 Agent |
| POST | `/system/restart` | 控制端平滑重启（广播维护预告，保护全网 Agent 零误报） |

控制端备份与恢复另有异步任务模式接口（见 `AGENTS.md` §3.12「控制端备份与恢复工程约定」）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/system/backups/status` | 备份 / 恢复任务阶段与终态（admin） |
| GET/PUT | `/system/backups/policy` | 定时备份策略（admin） |
| DELETE | `/system/restore/pending` | 取消待恢复任务（admin） |

## 证书中心

写操作 admin；读操作所有登录角色可见。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/certs` | 已签发证书列表（每行带服务端派生字段 `source=acme\|manual_dns01\|manual_upload`、`account_name`、管理员备注 `remark`） |
| GET | `/certs/:id` | 单张证书 |
| GET | `/certs/presets` | ACME 机构预设目录地址 |
| GET | `/certs/cover?domain=<d>` | 覆盖探测（只读）：`{covered, usable, reason?, cert?{id, domains, source, account_name, not_after}}`。复用 `FindByDomain` 同源匹配（精确 SAN 或通配符覆盖子域名）；`usable` 仅当前可用证书为真，`reason` 说明过期 / 未生效 / 缺私钥；PEM 剥离；登录即可、免操作审计；非法 / 空 / 通配符域名 400 |
| GET | `/certs/:id/detail` | 证书详情（只读）：现场解析证书链 leaf（序列号 / SHA-256 指纹 / 密钥与签名算法 / 链张数）、用途反查（面板绑定 / 兜底、应用域名匹配）、`chain_pem` 仅公钥链 |
| GET | `/certs/:id/download` | 下载公钥链附件 `<主域名>.crt`（仅公钥链、不打包私钥；登录即可、只读免操作审计；浏览器导航下载走 `?token=` 查询参数惯例） |
| POST | `/certs/:id/key` | 查看私钥明文 `{data:{key_pem}}`（adminOnly + 审计 `cert_key_reveal` / high；常规响应一律剥离私钥，本端点是唯一显式出口） |
| POST | `/certs/accounts/:id/key` | 查看 ACME 账户 EAB HMAC Key 明文 `{data:{eab_hmac_key}}`（adminOnly + 审计 `cert_account_key_reveal` / high；常规响应只给掩码，本端点是唯一显式出口；账户未配置 EAB 返回 404） |
| POST | `/certs/issue` | ACME 签发 `{domains[], account_id?, challenge?}`。异步，202 受理返回 `{ok:true, task_id, cert_id, message}`；`challenge`：`""` 自动 / `"http-01"` / `"dns-01"` / `"dns-01-manual"`，显式指定后不再自动回退 |
| GET | `/certs/:id/log` | 证书执行日志（只读）：`{data:{id, running, log, updated_at}}`。优先读续期中 liveLogs，无活跃任务回退读落盘 `<crt_id>.log`；登录即可、免操作审计 |
| GET | `/certs/tasks` | 签发任务列表（只读）：`{data: IssueTask[]}`（精简载荷，不含大文本 log；登录即可、免操作审计） |
| GET | `/certs/tasks/:taskID` | 签发任务状态与日志（只读）：`{data: IssueTask}`（含全量步骤日志，运行中叠加 live log；登录即可、免操作审计） |
| POST | `/certs/import` | 上传已有证书 `{cert_pem, key_pem}`（服务端校验 PEM 与私钥匹配，入库后进入已签发列表，不自动续期） |
| POST | `/certs/:id/renew` | 续期（ACME 重新签发，按记录存量验证方式；未存则自动） |
| POST | `/certs/:id/reissue` | 按新配置重新签发 `{domains, account_id, challenge}`（异步任务 202；手动 DNS-01 返回 order 走两阶段确认；adminOnly + 审计 `cert_reissue` / high） |
| PUT | `/certs/:id/remark` | 编辑管理员备注 `{remark}`（≤200 字，仅元信息；adminOnly + 审计） |
| DELETE | `/certs/:id` | 删除证书 |
| GET | `/certs/config` | dns-mng 集成配置（自动 DNS-01 验证后端） |
| PUT | `/certs/config` | 更新 dns-mng 集成配置 |
| GET/POST | `/certs/accounts` | ACME 机构账户列表 / 创建 |
| GET/PUT/DELETE | `/certs/accounts/:id` | 单个 ACME 机构账户读写 / 删除 |

### 证书中心：手动 DNS-01（两阶段）

管理员自行添加 TXT 解析，不依赖 dns-mng。挑战验证策略与订单状态机见 `AGENTS.md` §B.7.2。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/certs/manual/:id/confirm` | `challenge="dns-01-manual"` 时 `/certs/issue` 同步创建 ACME 订单并返回待添加的 TXT 记录 `{id, identifiers[], records[{domain, host, type, value}], status, expires_at}`（200）；管理员在 DNS 服务商添加解析后调本接口通知 CA 开始验证（异步 202，轮询下条查询进度） |
| GET | `/certs/manual/:id` | 手动订单状态与 TXT 记录 `{status: awaiting_dns\|verifying\|done\|error, cert_id?, error?}` |
| DELETE | `/certs/manual/:id` | 取消待处理的手动订单（`verifying` 中不可取消；CA 侧订单自然过期） |

## 面板证书

宝塔式一键签发，admin。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/system/panel-cert` | 面板证书状态（`ssl_enabled` / `panel_domain` / `strict_domain` / `active` / `source` / `subject` / `issuer` / `dns_names` / `days_left` / `panel_http_port` / `standard_ports[{port, desired, listening, error?}]`） |
| POST | `/system/panel-cert/issue` | 启动异步签发 `{mode: domain\|ip, ip?, challenge?}`。202 返回 job |
| GET | `/system/panel-cert/issue/:job_id` | 签发任务进度 `{status: running\|awaiting_dns\|done\|error, cert_id?, error?, pending_id?, dns_records?}` |
| POST | `/system/panel-cert/issue/:job_id/confirm` | 手动 DNS-01 任务：管理员添加 TXT 解析后通知 CA 开始验证（异步 202；成功自动绑定面板并启用 HTTPS） |
| DELETE | `/system/panel-cert/issue/:job_id` | 取消手动 DNS-01 签发任务（同时取消证书中心待处理订单） |
| GET | `/system/panel-cert/public-ip` | 检测服务器公网出口 IP |

## 安全入口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/secure-entry/<路径>` | 校验通过后签发 HttpOnly cookie（`wm_secure_entry=<路径>.<HMAC>`）并 302 到 `/` |

## 安装与下载（公开）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/install` | Linux 一键安装脚本 |
| GET | `/install_script` | 安装脚本（别名） |
| GET | `/host/install_script` | 主机安装脚本 |
| GET | `/agent/binary` | Agent 二进制下载 |

## WebSocket

| 端点 | 说明 |
| --- | --- |
| `WS /ws/terminal/:sid` | 终端 IO（浏览器 ↔ 控制端 ↔ Agent） |
