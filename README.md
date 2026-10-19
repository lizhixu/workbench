# Watchman · 自研云堡垒机 / 主机管理助手

自托管的主机一体化运维平台：一个 Web 控制台 + 装在被管主机上的 Agent，Agent 主动回连控制端，被管主机无需公网 IP、不开放端口。覆盖运维、监控、安全三类能力，面向个人开发者与小型团队自部署。

> 背景：对标长亭百川云「牧云·主机管理助手 / 云堡垒机」。原 SaaS 说停就停，故自研一套可自部署的等价系统，核心目标是**自己掌控、永久可用、不依赖第三方 SaaS**。
>
> 📖 需求与设计以 [`AGENTS.md`](AGENTS.md) 为唯一事实来源（每次开工前必读）；前端设计见 [`DESIGN.md`](DESIGN.md)。

## 架构

| 组件 | 技术 | 职责 |
| --- | --- | --- |
| 控制端 `watchman-server` | Go | Web 控制台（前端 go:embed 进二进制）+ API 网关 + Agent 接入（gRPC）+ 数据存储 + 告警引擎 |
| 被管端 `watchman-agent` | Go | 采集主机信息、执行下发指令、终端/文件/Docker 通道；Linux/Windows，amd64/arm64 |
| 前端 `web/` | Vue 3 + TypeScript + Vite + Naive UI | 控制台 UI，构建产物经 `go:embed` 打进 server 单二进制 |

```
web/  ──vite build──▶  dist/  ──go:embed──▶  watchman-server ── :18789（页面+API 同源）
                                                            ▲
agent ── gRPC over TLS ────────────────────────────────── :9090
```

单二进制直跑，无需 nginx（对标 1Panel/宝塔面板模式）。

## 核心功能

- **主机管理**：一键绑定（安装命令含一次性 token）、分组/搜索/筛选、在线状态、解绑并卸载 Agent
- **远程终端**：Web 终端（xterm），支持会话审计与录像回放
- **文件管理**：在线浏览/编辑（Monaco）、上传下载
- **监控告警**：CPU/内存/网络/磁盘实时与历史曲线（默认 7 天）、自定义阈值告警
- **证书与域名**：ACME 证书签发/续期（DNS-01）、域名绑定到应用（本机/Nginx 网关模式）
- **安全入口**：开启后只能通过秘密入口地址访问面板，直接访问首页/登录接口一律 404（防扫描器发现）
- **系统设置**：统一设置中心（system/user 两级作用域），主题、终端偏好、导航、AI 等
- **AI 助手**：可选接入 OpenAI 兼容模型（Ollama/vLLM），智能诊断与命令生成；不配置时其余功能照常可用
- **内网穿透 / 隧道**：打通不同内网的主机

## 快速开始

### 一键部署（推荐）

```bash
bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)"
# 带参数（原样透传给 install.sh）：
bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)" -- --port 18789
bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)" -- --help
```

或手动下载执行（等价）：

```bash
curl -fsSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/install.sh -o install.sh
sudo bash install.sh            # 默认跟踪正式版，面板监听 18789
sudo bash install.sh --help    # 查看全部选项（--version、--port、--mirror 等）
```

安装目录 `/opt/watchman`，数据目录 `/opt/watchman/data`。

> 版本通道：默认只跟踪正式版。想尝鲜预发布版时，在面板「系统设置 → 系统升级」中打开「加入测试计划」，之后重跑安装脚本升级即可（不再用 `--channel` 这类命令参数区分）。

### 独立单二进制直接运行（免解压）

除了一键脚本自动托管 systemd 外，也可以从 [GitHub Releases](https://github.com/lizhixu/workbench/releases) 直接下载单二进制独立运行：

- **控制端（内置 Web 控制台，开箱即用）**：
  ```bash
  # Linux x86_64
  curl -LO https://github.com/lizhixu/workbench/releases/download/<TAG>/watchman-server-linux-amd64
  chmod +x watchman-server-linux-amd64
  ./watchman-server-linux-amd64 -http :18789 -grpc :9090 -data ./data
  ```
  控制端启动后直接在浏览器打开 `http://<IP>:18789`，默认管理员账号密码均为 `admin`。若本地未预置各系统 agent 二进制，首次生成纳管命令并下载时控制端会自动按需从官方 Release 缓存。

- **被管端 Agent（独立运行）**：
  - **Linux**：
    ```bash
    curl -LO https://github.com/lizhixu/workbench/releases/download/<TAG>/watchman-agent-linux-amd64
    chmod +x watchman-agent-linux-amd64
    ./watchman-agent-linux-amd64 -server <SERVER_IP>:9090 -token <ENROLL_TOKEN>
    ```
  - **Windows**：直接下载 `watchman-agent-windows-amd64.exe`，在 PowerShell 或命令提示符中执行：
    ```powershell
    .\watchman-agent-windows-amd64.exe -server <SERVER_IP>:9090 -token <ENROLL_TOKEN>
    ```

### 源码构建与本地调试

#### 1. 编译构建

```bash
make build-all        # 快速编译 server 与 agent 二进制到 bin/ 目录
make test vet fmt     # 执行测试 / go vet 语法校验 / 代码格式化
```

前端校验与生产构建：
```bash
cd web
npx vue-tsc -b        # TypeScript 类型检查
npm run build         # 前端生产打包（输出至 web/dist/）
```

#### 2. 本地启动调试（前后端分离开发）

- **启动控制端 Server**（默认监听 HTTP `:18080`、gRPC `:9090`）：
  ```bash
  # 方式 1：直接通过 go run 启动
  go run ./server/cmd/watchman -http :18080 -grpc :9090 -data ./data

  # 方式 2：使用固定 JWT 密钥与 Vault 密码启动（避免重启后需重新登录）
  go run ./server/cmd/watchman -http :18080 -grpc :9090 -data ./data \
    -jwt-key 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
    -vault-pass devpass123
  ```

- **启动 Web 前端**（支持热重载，自动反向代理 `/api` 到本地 Server）：
  ```bash
  cd web
  npm install
  npm run dev           # 开发服务器启动于 http://localhost:5173
  ```
  在浏览器中打开 `http://localhost:5173` 即可进入控制台，初始管理员账号密码均为 `admin`。

- **启动被管端 Agent**（本地回连调试）：
  1. 在控制台「主机」页面点击「添加主机」，生成一次性 `enroll_token`；
  2. 本地启动 Agent 进行反向回连：
  ```bash
  # Linux / macOS
  go run ./agent/cmd/watchman-agent -server localhost:9090 -enroll <ENROLL_TOKEN> -state ./data/agent-state.json

  # Windows (PowerShell)
  go run .\agent\cmd\watchman-agent -server localhost:9090 -enroll <ENROLL_TOKEN> -state .\data\agent-state.json
  ```
  *说明：首次凭 token 注册成功后，Agent 会将长期凭证存入 `-state` 状态文件；后续只需运行 `go run ./agent/cmd/watchman-agent -server localhost:9090 -state ./data/agent-state.json` 即可直接连回。*

#### 3. 常用调试参数与应急恢复

- **对接本地 AI 大模型调试（如 Ollama）**：
  ```bash
  go run ./server/cmd/watchman -data ./data -ai-url http://localhost:11434 -ai-model qwen2.5:14b
  ```
- **构建包含内嵌 Web UI 的单二进制**：
  ```bash
  make build-server-with-ui   # 编译前端并自动将 dist 嵌入 server，产出独立可运行的 bin/watchman
  ```
- **应急恢复命令（调试时若误开启安全入口或证书导致无法访问面板）**：
  ```bash
  go run ./server/cmd/watchman -reset-secure-entry -data ./data   # 关闭安全入口（恢复通过 / 正常访问）
  go run ./server/cmd/watchman -reset-panel-ssl -data ./data      # 关闭面板 SSL 与强制 HTTPS 重定向
  go run ./server/cmd/watchman -reset-panel-domain -data ./data   # 解绑面板域名（恢复 IP 访问）
  ```

### 绑定一台主机

在控制台「主机」页生成安装命令，复制到被管主机执行即可完成注册；Windows 可下载安装包。

## 开发注意事项

在进行二次开发或提交代码前，请务必仔细阅读并严格遵守 [`AGENTS.md`](AGENTS.md) 中的工程规范：

1. **唯一事实来源与文档同步**
   - [`AGENTS.md`](AGENTS.md) 是系统需求、协议字段、REST/WS 接口与架构设计的唯一事实来源，每次开工前必读。
   - 所有功能的改动或新增，在实现代码的同时**必须同步更新 AGENTS.md** 对应章节，严禁只改代码不更新文档。

2. **零错误防线与质量校验**
   - **前端 (Vue 3 / TypeScript)**：任何代码修改必须通过 `npx vue-tsc -b` 类型检查与 `npm run build` 构建，确保组件导入正确、模板语法完整无误、严格去除废弃 Emoji 符号，且符合 TypeScript 类型约束。
   - **后端 (Go)**：修改后必须运行 `make test vet fmt` 或 `go vet` / `go build`，确保导出函数、结构体字段与 Protobuf IDL 接口声明及实现 100% 对齐。

3. **版本唯一真相源与发版纪律**
   - 系统各组件（Server、Agent、升级清单 Manifest 等）的**版本号完全由官方 Git Tag 触发的 GitHub Release 提供**（形如 `v1.2.3` 或 `v1.2.3-beta.1`）。
   - **严禁私自捏造或拼接版本号**（例如取 commit hash 拼接 `0.1.0-<hash>`）；开发态一律为 `0.1.0-dev` 占位，所有线上升级验证基准必须来源于官方 Release 产物与 CI 生成的 `manifest.json`。
   - **严禁使用裸 tag**：打 tag 必须为附注 tag（`git tag -a vX.Y.Z -m "..."`），并在说明中详细列出 Added / Changed / Fixed / Security 清单。

4. **操作审计全覆盖**
   - 凡是会改变系统或主机状态的写操作（`POST`/`PUT`/`PATCH`/`DELETE`），必须记录操作审计日志。
   - 新增写操作路由时，必须在 `server/internal/api/router.go` 的 `deriveMutationAction` 中登记规则，并同步在 `audit_coverage_test.go` 中补充覆盖用例。
   - 审计文案必须为面向操作的**全中文描述**（如「部署应用」「续期证书」），严禁直接展示原始 HTTP 方法与路径；新增 action 码必须在 `web/src/api/audit.ts` 中注册中文标签。

5. **前端 UI 与布局设计规范**
   - **组件选用**：优先直接使用 Naive UI 原生组件（`NCard`、`NDataTable`、`NTabs`、`NButton` 等），杜绝无意义的低效套壳封装；独立页面主标题统一为外层单文字 `<h2 class="page-title">`，无中英文混排。
   - **页面留白统一**：页面外层内边距由 `AppShell` 统一施加，页面组件根节点**严禁再定义外层 padding**。
   - **大数据表格布局**：承载不定长数据的表格一律使用「整页填满视口 + 表头与分页固定 + 仅数据区滚动 + **必须分页**」规范（`NDataTable flex-height` + 弹性容器 `min-height: 0`），禁止使用原生无分页的 `<NTable>` 承载不定长数据。
   - **多页签工作区 (KeepAlive)**：路由组件必须使用 `defineOptions({ name: 'Xxx' })` 声明稳定组件名，并与路由配置中的 `meta.viewName` 保持一致，确保切页签时保留表单与终端等运行状态。

6. **Agent 通信与并发安全底线**
   - **零入站端口**：守住被管机不开放入站端口的原则，所有操作均由 Agent 主动出站长连接（gRPC over TLS）承载。
   - **命令下发安全**：下发到 Agent 的命令必须根据目标主机操作系统动态选择 Shell（Windows 为 PowerShell、Alpine 为 sh、其他 Linux 为 bash），所有外部参数必须进行 Shell 转义，严禁直接字符串拼接以防命令注入。
   - **RPC Hub 并发与锁纪律**：持锁期间严禁调用外部回调（防止自死锁），关闭共享 Channel 必须带 `closed` 状态守卫（防止并发向已关闭通道发送导致 panic）。
   - **双向维护握手**：控制端重启与 Agent 升级必须通过双向维护信令（`MaintenanceNotice` 与 `reconnect_reason`）进行状态对齐，杜绝因重启和重连引发虚假离线/上线告警轰炸。

## 发版

推送带附注的 `v*` tag 触发 GitHub Actions 官方 Release 流水线：
- 编译 Server（Linux amd64/arm64）与 Agent（Linux amd64/arm64、Windows amd64），通过 ldflags 注入版本号、Commit 与构建时间；
- 发布独立单二进制：`watchman-server-linux-amd64`、`watchman-server-linux-arm64`、`watchman-agent-linux-amd64`、`watchman-agent-linux-arm64`、`watchman-agent-windows-amd64.exe`；
- 发布完整套件包：`watchman-dist-<tag>-linux-<arch>.tar.gz`（内含完整二进制、manifest.json 与 install.sh，前端已 embed 进 server）；
- 发布官方校验与清单：`manifest.json` 与 `CHECKSUMS.txt`。

严禁使用裸 tag，必须编写包含详细变更的附注 tag：
```bash
git tag -a v0.1.0-beta.9 -m "release: v0.1.0-beta.9

### Added
- Release 页面直出免解压独立单二进制
"
git push origin v0.1.0-beta.9
```

## 目录结构

```
server/     控制端（cmd/watchman 入口，internal/ 按域拆分）
agent/      被管端
web/        前端控制台
proto/      gRPC 协议（buf 管理）
deploy/     一键安装脚本 install.sh（systemd 模板）
```

## 许可证

私有项目，保留所有权利。
