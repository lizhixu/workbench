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

### 源码构建

```bash
make build-all        # 构建 server + agent
make test vet fmt    # 测试 / vet / 格式化
```

前端：

```bash
cd web && npm install
npm run dev           # 开发（代理 /api 到本地 server）
npx vue-tsc -b        # 类型检查
npm run build         # 生产构建
```

常用 server 参数：`-grpc :9090`（Agent 接入）、`-http :18080`、`-data ./data`、`-jwt-key`、`-vault-pass`、`-ai-url`/`-ai-model`/`-ai-key`。

锁死恢复（忘记安全入口路径时）：`watchman-server -reset-secure-entry -data /opt/watchman/data`。

### 绑定一台主机

在控制台「主机」页生成安装命令，复制到被管主机执行即可完成注册；Windows 可下载安装包。

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
