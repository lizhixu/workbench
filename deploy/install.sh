#!/usr/bin/env bash
#
# Watchman 控制端一键安装脚本（单二进制 + systemd，无需 nginx）
#
# 控制端是单个 Go 二进制：Web 控制台经 go:embed 打进二进制，
# 直接监听端口同时提供静态页面与 API（对标 1Panel/宝塔面板模式）。
#
# 一键安装（默认跟踪正式版）：
#   curl -fsSL https://raw.githubusercontent.com/lizhixu/workbench/main/deploy/install.sh | bash
#
# 常用姿势：
#   bash install.sh --version v1.2.3        # 精确锁定版本
#   bash install.sh --mirror https://ghproxy.example.com   # 走 GitHub 镜像
#   bash install.sh --uninstall             # 卸载（保留数据）
#   bash install.sh --uninstall --purge     # 卸载并删除所有数据
#
# 版本策略（不再用命令行区分测试版）：
#   - 首次安装：跟踪最新正式版（GitHub /releases/latest，自动排除 pre-release）。
#   - 想尝鲜预发布版：在面板「系统设置 → 系统升级」中打开「加入测试计划」，
#     之后重跑本脚本升级时会自动跟踪含 pre-release 的最新版本。
#   - --version 可精确锁定任意版本（优先级最高）。
#
# 语义：
#   - 首次运行 = 安装；重跑 = 升级（保留 /opt/watchman/data 与密钥）。
#   - 同版本重跑默认跳过，加 --force 强制重装。
#   - 需要 root；仅支持 Linux（amd64/arm64）。
#
set -euo pipefail

# ---------------- 默认值（可用环境变量覆盖） ----------------
REPO="${WATCHMAN_REPO:-lizhixu/workbench}"   # --repo 可覆盖
VERSION_TAG=""                               # --version，精确锁定版本
MIRROR="${GITHUB_MIRROR:-}"                  # --mirror，GitHub 镜像前缀
BASE_URL=""                                  # --base-url，完全自定义下载源
HTTP_PORT=18789                             # --port，面板监听端口（冷门端口，避免占用业务常用的 80/8080）
INSTALL_DIR="/opt/watchman"
DATA_DIR="/opt/watchman/data"
ASSUME_YES=0
FORCE=0
UNINSTALL=0
PURGE=0

TAG=""                                       # 解析出的最终版本（v1.2.3）
TMP=""

# ---------------- 输出 ----------------
log_info() { echo "[INFO] $*"; }
log_ok()   { echo "[ OK ] $*"; }
log_warn() { echo "[WARN] $*" >&2; }
die()      { echo "[ERROR] $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
用法: install.sh [选项]

选项:
  --version v1.2.3     精确锁定版本（优先级最高）
  --mirror URL         GitHub 镜像前缀，如 https://ghproxy.example.com
                       （或 GITHUB_MIRROR 环境变量）
  --base-url URL       完全自定义下载源，布局需与 GitHub Release 一致：
                       {base-url}/{tag}/watchman-dist-{tag}-linux-{arch}.tar.gz
  --repo owner/name    仓库（默认 lizhixu/workbench）
  --port PORT          面板监听端口（默认 18789）
  --force              已是目标版本时仍强制重装
  --uninstall          卸载（保留数据与密钥）
  --purge              配合 --uninstall，删除 /opt/watchman 全部内容
  -y                   全自动，不做交互确认
  -h, --help           显示本帮助
EOF
}

# ---------------- 参数解析 ----------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)   VERSION_TAG="$2"; shift 2 ;;
    --mirror)    MIRROR="$2"; shift 2 ;;
    --base-url)  BASE_URL="$2"; shift 2 ;;
    --repo)      REPO="$2"; shift 2 ;;
    --port)      HTTP_PORT="$2"; shift 2 ;;
    --force)     FORCE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge)     PURGE=1; shift ;;
    -y)          ASSUME_YES=1; shift ;;
    -h|--help)   usage; exit 0 ;;
    *)           die "未知参数: $1（用 --help 查看用法）" ;;
  esac
done

confirm() {
  # confirm "提示语"：-y 直接通过，否则交互确认
  [[ "$ASSUME_YES" -eq 1 ]] && return 0
  read -r -p "$1 [y/N] " ans
  [[ "$ans" == "y" || "$ans" == "Y" ]]
}

cleanup_tmp() { [[ -n "$TMP" && -d "$TMP" ]] && rm -rf "$TMP"; }
# 保留原始退出码：trap 里 cleanup 的失败不能污染 $?（set -e 下 trap 内失败会直接退出）
trap 'rc=$?; cleanup_tmp || true; exit "$rc"' EXIT

# ================= 卸载 =================
do_uninstall() {
  log_info "卸载 Watchman 控制端..."
  systemctl stop watchman-server watchman-agent 2>/dev/null || true
  systemctl disable watchman-server watchman-agent 2>/dev/null || true
  rm -f /etc/systemd/system/watchman-server.service /etc/systemd/system/watchman-agent.service
  systemctl daemon-reload 2>/dev/null || true

  # 旧版残留的 nginx 站点（2026-09-28 起不再需要 nginx）
  if [[ -f /etc/nginx/sites-enabled/watchman || -f /etc/nginx/sites-available/watchman ]]; then
    rm -f /etc/nginx/sites-enabled/watchman /etc/nginx/sites-available/watchman
    if systemctl is-active nginx >/dev/null 2>&1; then
      (nginx -t && systemctl reload nginx) || log_warn "nginx reload 失败，请手动检查"
    fi
    log_ok "已移除旧版 nginx 站点配置"
  fi

  if [[ "$PURGE" -eq 1 ]]; then
    confirm "将删除 $INSTALL_DIR 全部内容（含数据与密钥），确认吗？" || die "已取消"
    rm -rf "$INSTALL_DIR"
    log_ok "已删除 $INSTALL_DIR（--purge）"
  else
    rm -rf "$INSTALL_DIR/bin" \
           "$INSTALL_DIR/manifest.json" "$INSTALL_DIR/install.sh"
    log_ok "已卸载程序文件，数据保留在 $DATA_DIR（彻底删除请加 --purge）"
  fi
}

# ================= 安装步骤 =================
check_root() {
  [[ "$(id -u)" -eq 0 ]] || die "请用 root 运行（一键安装需要写 /opt 与 systemd）"
}

detect_platform() {
  [[ "$(uname -s)" == "Linux" ]] || die "仅支持 Linux，当前: $(uname -s)"
  case "$(uname -m)" in
    x86_64|amd64)   ARCH=amd64 ;;
    aarch64|arm64)   ARCH=arm64 ;;
    *)              die "不支持的架构: $(uname -m)（仅支持 amd64/arm64）" ;;
  esac
  log_info "平台: linux/$ARCH"
}

check_deps() {
  local missing=()
  for cmd in curl tar sha256sum openssl systemctl grep; do
    command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
  done
  [[ ${#missing[@]} -eq 0 ]] || die "缺少依赖: ${missing[*]}，请先安装"
}

beta_opted_in() {
  # 测试计划开关在面板「系统设置 → 系统升级」中，由管理员开启；
  # 落盘在 $DATA_DIR/settings.json 的 system 域（点分键 system.join_beta_program）。
  # 文件不存在（全新安装）或开关未开 → 走正式版。
  local f="$DATA_DIR/settings.json"
  [[ -f "$f" ]] || return 1
  grep -q '"system\.join_beta_program"[[:space:]]*:[[:space:]]*true' "$f" 2>/dev/null
}

resolve_version() {
  if [[ -n "$VERSION_TAG" ]]; then
    TAG="$VERSION_TAG"
    [[ "$TAG" == v* ]] || TAG="v$TAG"
    log_info "使用指定版本: $TAG"
    return
  fi
  if beta_opted_in; then
    local api="https://api.github.com/repos/${REPO}/releases?per_page=1"
    [[ -n "$MIRROR" ]] && api="${MIRROR}/https://api.github.com/repos/${REPO}/releases?per_page=1"
    log_info "已加入测试计划：解析最新版本（含 pre-release）..."
    TAG=$(curl -fsSL "$api" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4) \
      || die "版本解析失败（$api）"
  else
    local url="https://github.com/${REPO}/releases/latest"
    [[ -n "$MIRROR" ]] && url="${MIRROR}/https://github.com/${REPO}/releases/latest"
    log_info "解析最新正式版..."
    local eff
    eff=$(curl -fsSIL -o /dev/null -w '%{url_effective}' "$url") || die "版本解析失败（$url）"
    # GitHub 的 /releases/latest 只认正式版：仓库暂无正式版时会落到 /releases 列表页
    [[ "$eff" == */tag/v* ]] || die "暂无正式版：可用 --version vX.Y.Z 指定版本，或在面板「系统设置 → 系统升级」中加入测试计划后重试"
    TAG="${eff##*/tag/}"
  fi
  [[ -n "$TAG" && "$TAG" == v* ]] || die "解析到的版本非法: '$TAG'"
  log_ok "目标版本: $TAG"
}

download_tarball() {
  local asset="watchman-dist-${TAG}-linux-${ARCH}.tar.gz"
  local dl_base
  if [[ -n "$BASE_URL" ]]; then
    dl_base="${BASE_URL%/}/${TAG}"
  else
    dl_base="https://github.com/${REPO}/releases/download/${TAG}"
    [[ -n "$MIRROR" ]] && dl_base="${MIRROR}/https://github.com/${REPO}/releases/download/${TAG}"
  fi
  TMP="$(mktemp -d)"
  log_info "下载 $asset ..."
  curl -fsSL --retry 3 -o "$TMP/$asset" "$dl_base/$asset" \
    || die "下载失败：$dl_base/$asset"
  curl -fsSL --retry 3 -o "$TMP/CHECKSUMS.txt" "$dl_base/CHECKSUMS.txt" \
    || die "下载失败：$dl_base/CHECKSUMS.txt"
  log_info "校验 sha256..."
  grep " ${asset}\$" "$TMP/CHECKSUMS.txt" | (cd "$TMP" && sha256sum -c -) \
    || die "sha256 校验失败，安装包可能损坏或被篡改"
  log_ok "校验通过"
  TARBALL="$TMP/$asset"
}

installed_version() {
  local mf="$INSTALL_DIR/manifest.json"
  [[ -f "$mf" ]] || return 0
  grep -o '"version": *"[^"]*"' "$mf" 2>/dev/null | head -1 | cut -d'"' -f4 || true
}

stop_services() {
  if systemctl list-unit-files 2>/dev/null | grep -q '^watchman-server\.service'; then
    log_info "停止旧服务..."
    systemctl stop watchman-server watchman-agent 2>/dev/null || true
  fi
}

remove_legacy_nginx() {
  # 2026-09-28 起控制端不再需要 nginx：旧版 install.sh 写下的站点配置若残留，
  # 会继续反代 127.0.0.1:18080，且其 auth_request 指向已删除的
  # /api/v1/secure-entry/check（新二进制对该路径返回 404，nginx 只捕获 401），
  # 导致所有静态页面 404。只删我们自己的站点文件，不碰其他站点。
  local changed=0
  for f in /etc/nginx/sites-enabled/watchman /etc/nginx/sites-available/watchman; do
    if [[ -f "$f" ]]; then rm -f "$f"; changed=1; fi
  done
  [[ "$changed" -eq 0 ]] && return 0
  log_ok "已移除旧版 nginx 站点配置（新版不再需要 nginx）"
  if systemctl is-active nginx >/dev/null 2>&1; then
    (nginx -t && systemctl reload nginx) || log_warn "nginx reload 失败，请手动检查"
  fi
}

extract_tarball() {
  log_info "解压到 $INSTALL_DIR ..."
  mkdir -p "$INSTALL_DIR"
  tar -xzf "$TARBALL" -C "$INSTALL_DIR" --strip-components=1
  chmod 755 "$INSTALL_DIR/bin/watchman-server" "$INSTALL_DIR/bin"/watchman-agent* 2>/dev/null || true
  # Web 控制台已通过 go:embed 打进 watchman-server，无需单独部署前端
  # 把安装脚本自身留一份，方便以后重跑升级
  cp "$0" "$INSTALL_DIR/install.sh" 2>/dev/null || true
  log_ok "文件已落盘"
}

provision_secrets() {
  # 密钥只生成一次：重装/升级必须复用，否则已签发的 token 与加密的凭据全废
  local env_file="$INSTALL_DIR/watchman.env"
  if [[ ! -s "$env_file" ]]; then
    umask 077
    printf 'WATCHMAN_JWT_KEY=%s\nWATCHMAN_VAULT_PASS=%s\n' \
      "$(openssl rand -hex 32)" "$(openssl rand -hex 32)" > "$env_file"
    log_ok "已生成服务密钥 $env_file"
  else
    log_info "复用已有服务密钥 $env_file"
  fi
}

write_systemd_server() {
  cat > /etc/systemd/system/watchman-server.service <<EOF
[Unit]
Description=Watchman Control Server
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
EnvironmentFile=$INSTALL_DIR/watchman.env
ExecStart=$INSTALL_DIR/bin/watchman-server -grpc :9090 -http :$HTTP_PORT -data $DATA_DIR -jwt-key \${WATCHMAN_JWT_KEY} -vault-pass \${WATCHMAN_VAULT_PASS}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
  log_ok "已写入 watchman-server.service"
}

api_login_token() {
  # 登录默认 admin 账号拿 token，重试 30 秒（等 server 启动）
  local out
  for _ in $(seq 1 10); do
    out=$(curl -sS -X POST http://127.0.0.1:${HTTP_PORT}/api/v1/auth/login \
      -H "Content-Type: application/json" \
      -d '{"username":"admin","password":"admin"}' 2>/dev/null) || true
    local tok
    tok=$(echo "$out" | grep -o '"token": *"[^"]*"' | head -1 | cut -d'"' -f4)
    if [[ -n "$tok" ]]; then
      echo "$tok"
      return 0
    fi
    sleep 3
  done
  return 1
}

write_systemd_agent() {
  # 本机自纳管：复用已有 enroll 状态，避免重复注册
  local enroll_param=""
  if [[ ! -s "$DATA_DIR/agent-state.json" ]]; then
    log_info "申请本机 agent 的 enroll token..."
    local token enroll_json enroll_token
    token=$(api_login_token) || die "server 未就绪或 admin 登录失败，无法为本机 agent 申请 enroll token"
    enroll_json=$(curl -sS -X POST http://127.0.0.1:${HTTP_PORT}/api/v1/hosts/enroll \
      -H "Authorization: Bearer $token")
    enroll_token=$(echo "$enroll_json" | grep -o '"enroll_token": *"[^"]*"' | head -1 | cut -d'"' -f4)
    [[ -z "$enroll_token" ]] && enroll_token=$(echo "$enroll_json" | grep -o '"token": *"[^"]*"' | head -1 | cut -d'"' -f4)
    [[ -n "$enroll_token" ]] || die "enroll token 申请失败：$enroll_json"
    enroll_param="-enroll $enroll_token "
    log_ok "enroll token 已申请"
  else
    log_info "复用已有 agent 注册状态"
  fi
  cat > /etc/systemd/system/watchman-agent.service <<EOF
[Unit]
Description=Watchman Agent
After=network.target watchman-server.service

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/bin/watchman-agent -server 127.0.0.1:9090 ${enroll_param}-state $DATA_DIR/agent-state.json
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  log_ok "已写入 watchman-agent.service"
}

start_services() {
  systemctl daemon-reload
  systemctl enable watchman-server >/dev/null 2>&1 || true
  systemctl restart watchman-server
  sleep 3
  systemctl is-active watchman-server >/dev/null || {
    journalctl -u watchman-server -n 30 --no-pager 2>/dev/null | tail -20 >&2 || true
    die "watchman-server 启动失败"
  }
  log_ok "watchman-server 运行中"
}

remove_legacy_webdir() {
  # 旧版把前端落盘在 $INSTALL_DIR/web（+ 残留的 web-dist.tar.gz）；
  # 新版 Web 控制台已 go:embed 进 watchman-server，这些成了死重，清理掉。
  local removed=0
  if [[ -d "$INSTALL_DIR/web" ]]; then rm -rf "$INSTALL_DIR/web"; removed=1; fi
  if [[ -f "$INSTALL_DIR/web-dist.tar.gz" ]]; then rm -f "$INSTALL_DIR/web-dist.tar.gz"; removed=1; fi
  [[ "$removed" -eq 1 ]] && log_ok "已清理旧版落盘前端（新版已内置进二进制）"
}

secure_entry_enabled() {
  # data 目录在升级时保留，安全入口一旦开启就会一直生效
  [[ -f "$DATA_DIR/settings.json" ]] || return 1
  grep -q '"security\.secure_entry_enabled"[[:space:]]*:[[:space:]]*true' "$DATA_DIR/settings.json" 2>/dev/null
}

health_check() {
  # Web 首页（Go 直接提供）
  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HTTP_PORT}/" 2>/dev/null) || code="000"
  if secure_entry_enabled; then
    # 安全入口开启时，无 cookie 访问首页应被 guard 404（页面隐藏），
    # 这本身就证明服务正常且 guard 生效；此时登录 API 同样被隐藏，跳过登录检查。
    [[ "$code" == "404" ]] || die "健康检查失败：首页返回 $code（安全入口已启用，期望 404）"
    log_ok "Web 服务正常（安全入口隐藏中，首页 404 符合预期）"
    return 0
  fi
  [[ "$code" == "200" ]] || die "健康检查失败：首页返回 $code"
  log_ok "Web 首页 200"
  # API 登录（默认 admin/admin）
  api_login_token >/dev/null || die "健康检查失败：API 登录不通"
  log_ok "API 登录正常"
}

print_summary() {
  local ip
  ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  [[ -z "$ip" ]] && ip="<本机IP>"
  cat <<EOF

========================================
 Watchman 控制端安装完成
========================================
 版本:     $TAG
 访问地址: http://${ip}:${HTTP_PORT}/
 默认账号: admin / admin（请登录后立即修改密码）
 数据目录: $DATA_DIR
 安装目录: $INSTALL_DIR
 版本清单: $INSTALL_DIR/manifest.json

 常用命令:
   systemctl status watchman-server watchman-agent
   journalctl -u watchman-server -f

 升级: 重跑本脚本即可（自动保留数据与密钥）
   bash $INSTALL_DIR/install.sh [--version vX.Y.Z]
========================================
EOF
}

# ================= 主流程 =================
main() {
  if [[ "$UNINSTALL" -eq 1 ]]; then
    check_root
    do_uninstall
    exit 0
  fi

  check_root
  detect_platform
  check_deps
  resolve_version

  local cur
  cur=$(installed_version)
  if [[ -n "$cur" ]]; then
    if [[ "$cur" == "$TAG" && "$FORCE" -eq 0 ]]; then
      log_ok "已安装 $TAG，无需重复安装（强制重装请加 --force）"
      exit 0
    fi
    log_info "检测到已安装版本 $cur，将升级到 $TAG"
  else
    log_info "全新安装 $TAG"
  fi

  download_tarball
  stop_services
  remove_legacy_nginx
  extract_tarball
  remove_legacy_webdir
  provision_secrets
  write_systemd_server
  start_services
  write_systemd_agent
  systemctl enable watchman-agent >/dev/null 2>&1 || true
  systemctl restart watchman-agent
  sleep 2
  systemctl is-active watchman-agent >/dev/null || die "watchman-agent 启动失败"
  log_ok "watchman-agent 运行中"
  health_check
  print_summary
}

main "$@"
