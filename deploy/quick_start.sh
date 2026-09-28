#!/usr/bin/env bash
# Watchman 一键安装引导（对标 1Panel quick_start.sh）。
#
# 用法：
#   bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)"
#   # 带参数（参数原样透传给 install.sh）：
#   bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)" -- --channel beta
#   bash -c "$(curl -sSL https://raw.githubusercontent.com/lizhixu/workbench/master/deploy/quick_start.sh)" -- --port 18789 --uninstall
#
# 只做两件事：
#   1. 下载 master 分支最新的 deploy/install.sh（安装逻辑以它为准，
#      版本解析 --channel/--version 由 install.sh 在运行时完成）；
#   2. 把全部参数透传给 install.sh 并执行。
#
# 环境变量（调试用）：WATCHMAN_INSTALL_URL 可覆盖下载地址。
set -euo pipefail

REPO="lizhixu/workbench"
BRANCH="master"
URL="${WATCHMAN_INSTALL_URL:-https://raw.githubusercontent.com/${REPO}/${BRANCH}/deploy/install.sh}"
TMP="/tmp/watchman-install.sh"

[[ "$(id -u)" -eq 0 ]] || { echo "请用 root 运行一键安装" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "需要 curl，请先安装（apt install curl / yum install curl）" >&2; exit 1; }

echo "下载安装脚本..."
curl -fsSL "$URL" -o "$TMP" || { echo "下载失败：$URL（检查网络后重试）" >&2; exit 1; }
[[ -s "$TMP" ]] || { echo "下载到的安装脚本为空，已中止" >&2; exit 1; }
grep -q "watchman" "$TMP" || { echo "下载到的文件不是 watchman 安装脚本，已中止" >&2; exit 1; }

exec bash "$TMP" "$@"
