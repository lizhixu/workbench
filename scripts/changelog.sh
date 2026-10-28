#!/usr/bin/env bash
# 从 git log 聚合版本级 changelog（AGENTS.md B.6.6）。
#
# 背景：Release notes 由 CI 读取「附注 tag 的正文」生成。历史上曾出现轻量 tag，
# 其 %(contents) 会静默退化为指向 commit 的 message，导致 GitHub 上的 tag 描述
# 只显示最近一条提交。本脚本的作用是把「上一个 tag 到 HEAD」之间的全部提交
# 按 Conventional Commits 的 type 聚合成分段清单，直接作为附注 tag 的正文。
#
# 用法：
#   bash scripts/changelog.sh                       # 上一个 tag..HEAD
#   bash scripts/changelog.sh v0.1.0-beta.24        # 指定基线
#   VERSION=v0.2.0-beta.1 bash scripts/changelog.sh # 首行加 release: <版本>
#   RANGE=A..B bash scripts/changelog.sh            # 显式区间
#
# 输出：Added / Changed / Fixed / Security / Breaking Changes 五段 markdown，
# 空段自动省略。docs / test / build / ci / chore 不进入清单。

set -euo pipefail

if [ -n "${RANGE:-}" ]; then
  LOG_RANGE="$RANGE"
elif [ -n "${1:-}" ]; then
  LOG_RANGE="$1..HEAD"
else
  BASE="$(git describe --tags --abbrev=0 2>/dev/null || true)"
  if [ -n "$BASE" ]; then
    LOG_RANGE="$BASE..HEAD"
  else
    LOG_RANGE="HEAD"
  fi
fi

declare -a ADDED=() CHANGED=() FIXED=() SECURITY=() BREAKING=()

# 记录条数，用于发现「区间内全是 docs/chore」时的空清单
COUNT=0

while IFS=$'\x1f' read -r -d $'\x1e' _hash subject body; do
  subject="${subject#"${subject%%[![:space:]]*}"}"
  subject="${subject%"${subject##*[![:space:]]}"}"
  [ -z "$subject" ] && continue

  # Merge 已由 --no-merges 过滤；兜底跳过残留的合并提交
  case "$subject" in
    "Merge "*) continue ;;
  esac

  COUNT=$((COUNT + 1))

  type="other"; scope=""; bang=""; text="$subject"
  if [[ "$subject" =~ ^([a-zA-Z]+)(\(([^\)]*)\))?(!)?:[[:space:]]*(.+)$ ]]; then
    type="${BASH_REMATCH[1],,}"
    scope="${BASH_REMATCH[3]}"
    bang="${BASH_REMATCH[4]}"
    text="${BASH_REMATCH[5]}"
  fi

  line="$text"
  if [ -n "$scope" ]; then
    line="**${scope}** ${text}"
  fi

  # BREAKING CHANGE 由 ! 标记或 body 中的声明判定，单独成段且不再重复归入 type 段
  is_breaking=0
  if [ -n "$bang" ] || printf '%s' "$body" | grep -qE '^BREAKING[ -]CHANGE:'; then
    is_breaking=1
    BREAKING+=("$line")
  fi

  if [ "$is_breaking" -eq 1 ]; then
    continue
  fi

  case "$type" in
    feat) ADDED+=("$line") ;;
    fix) FIXED+=("$line") ;;
    security) SECURITY+=("$line") ;;
    perf|refactor|style) CHANGED+=("$line") ;;
    revert) FIXED+=("$line") ;;
    docs|test|build|ci|chore) ;;
    *) CHANGED+=("$line") ;;
  esac
done < <(git log "$LOG_RANGE" --no-merges --reverse --format='%H%x1f%s%x1f%b%x1e')

if [ -n "${VERSION:-}" ]; then
  printf 'release: %s\n\n' "$VERSION"
fi

emit() {
  local title="$1"
  shift
  [ "$#" -eq 0 ] && return 0
  printf '### %s\n' "$title"
  local item
  for item in "$@"; do
    printf -- '- %s\n' "$item"
  done
  printf '\n'
}

emit "Breaking Changes" "${BREAKING[@]+"${BREAKING[@]}"}"
emit "Added" "${ADDED[@]+"${ADDED[@]}"}"
emit "Changed" "${CHANGED[@]+"${CHANGED[@]}"}"
emit "Fixed" "${FIXED[@]+"${FIXED[@]}"}"
emit "Security" "${SECURITY[@]+"${SECURITY[@]}"}"

if [ "$COUNT" -eq 0 ]; then
  echo "（${LOG_RANGE} 区间内没有可发布的提交：docs/chore/ci 等不进入 changelog）" >&2
fi
