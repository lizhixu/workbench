#!/usr/bin/env bash
# 校验发版 tag 的合法性（AGENTS.md B.6.5 / B.6.6）。
# 本地打 tag 后、以及 CI 建 Release 前都必须通过本校验。
#
# 核心防线（历史事故根因）：
#   轻量 tag（git tag v1.2.3 不带 -a）在 `git tag -l --format='%(contents)'`
#   下会静默退化为「指向 commit 的 message」，CI 无法感知，于是 GitHub Release
#   的正文变成最近一条提交的描述，该版本其余变更全部丢失。
#
# 用法：bash scripts/verify-tag.sh [tag]（缺省取最近一个可达 tag）

set -euo pipefail

TAG="${1:-$(git describe --tags --abbrev=0 2>/dev/null || true)}"

fail() {
  printf '✗ %s\n' "$*" >&2
  # ::error:: 会被 GitHub Actions 渲染成 Annotation，失败原因直接显示在 PR/Run 页面
  printf '::error::%s\n' "$(printf '%s' "$*" | head -1)" >&2
  exit 1
}

[ -n "$TAG" ] || fail "未指定 tag，且当前仓库没有任何可达 tag"

# 1. 命名必须严格符合 semver + v 前缀（CI 的 tags 过滤是 v[0-9]*，过于宽松）
if ! printf '%s' "$TAG" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.[1-9][0-9]*)?$'; then
  fail "tag 命名非法：$TAG
  合法形态：v1.2.3 / v1.2.3-beta.1 / v1.2.3-rc.1
  禁止：v1.2、v1.2.3.4、v1.2.3+build、latest、日期版本、任何 commit hash 拼接"
fi

# 2. 必须有 refs/tags/<tag>：actions/checkout 的 fetch-tags 默认 false，
#    只 checkout sha 时本地没有 tag ref，cat-file 与 tag -l 都会失效。
if ! git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null 2>&1; then
  git fetch --force --no-tags origin "refs/tags/${TAG}:refs/tags/${TAG}" >/dev/null 2>&1 || true
fi
if ! git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null 2>&1; then
  fail "本地不存在 refs/tags/${TAG}：CI 需在 checkout 配置 fetch-tags: true；本地需先 git fetch --tags"
fi

# 3. 必须是附注 tag（annotated tag 的对象类型为 tag；轻量 tag 为 commit）
OBJ_TYPE="$(git cat-file -t "refs/tags/${TAG}" 2>/dev/null || true)"
[ "$OBJ_TYPE" = "tag" ] || fail "$TAG 是轻量 tag（对象类型 ${OBJ_TYPE:-unknown}），不是附注 tag。
  %(contents) 会退化为指向 commit 的 message，Release 正文将只显示最近一条提交。
  处理：git tag -d $TAG && git push origin :refs/tags/$TAG，然后 git tag -a $TAG -F <notes> 重打"

MSG="$(git tag -l --format='%(contents)' "$TAG")"

# 4. 附注不能为空
if [ -z "$(printf '%s' "$MSG" | tr -d '[:space:]')" ]; then
  fail "$TAG 附注为空，Release 正文会是空白"
fi

# 5. 附注必须是版本级清单，而不是一句话或一条提交的复制
LINE_COUNT="$(printf '%s\n' "$MSG" | wc -l | tr -d ' ')"
if [ "$LINE_COUNT" -lt 3 ]; then
  fail "$TAG 附注仅 ${LINE_COUNT} 行，不足以描述整个版本的变更。
  请用：make changelog SINCE=<上一个tag> VERSION=$TAG > notes.md && git tag -a $TAG -F notes.md"
fi

COMMIT_SUBJECT="$(git log -1 --format='%s' "${TAG}^{commit}" 2>/dev/null || true)"
FIRST_LINE="$(printf '%s\n' "$MSG" | head -1)"
if [ -n "$COMMIT_SUBJECT" ] && [ "$FIRST_LINE" = "$COMMIT_SUBJECT" ]; then
  fail "$TAG 附注首行与指向 commit 的 subject 完全相同，疑似直接把提交信息当作版本说明"
fi

# 6. 至少命中一个 changelog 分段或条目，防止随意堆砌几行文字
if ! printf '%s' "$MSG" | grep -qE '^(### |- |\* )'; then
  fail "$TAG 附注没有可识别的变更条目（缺少 '### Added' 之类的分段或 '- ' 列表）"
fi

printf '✓ %s 校验通过（附注 %s 行）\n' "$TAG" "$LINE_COUNT"
