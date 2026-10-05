#!/usr/bin/env bash
#
# 拉取设计阶段参考过的 15 个开源项目到 参考项目/。
#
# 这些项目各自有独立的 git 历史与许可证，不作为本仓库的一部分提交。
# 分析报告见 docs/ 与 分析/ 目录。
#
# 用法：
#   ./scripts/fetch-references.sh          # 只拉缺失的
#   ./scripts/fetch-references.sh --update # 全部更新
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="$ROOT/参考项目"
UPDATE=0

[[ "${1:-}" == "--update" ]] && UPDATE=1

# 格式：仓库地址|本地目录名
REPOS=(
  "https://github.com/jxxghp/MoviePilot.git|moviepilot"
  "https://github.com/konghanghang/ember.git|ember"
  "https://github.com/Prejudice-Studio/Twilight.git|twilight"
  "https://github.com/snnabb/Meridian.git|meridian"
  "https://github.com/zeyu8023/emby-pulse.git|emby-pulse"
  "https://github.com/binglww/bubble-emby-admin.git|bubble-emby-admin"
  "https://github.com/berry8838/Sakura_embyboss.git|sakura-embyboss"
  "https://github.com/nayeshiluo/lemon-emby-bot.git|lemon-emby-bot"
  "https://github.com/emby-keeper/emby-keeper.git|emby-keeper"
  "https://github.com/iovejieba/EmbyForge.git|embyforge"
  "https://github.com/DFANNN/emby-tools.git|emby-tools"
  "https://github.com/NSJLUCAS/emby-request-ui.git|emby-request-ui"
  "https://github.com/jialin4567/emby-register-enhanced.git|emby-register-enhanced"
  "https://github.com/Gaodong2510/emby-manager.git|emby-manager-gaodong"
  "https://github.com/Xieburouzzz/usersvr.git|usersvr"
)

mkdir -p "$DEST"

ok=0
skip=0
fail=0

for entry in "${REPOS[@]}"; do
  url="${entry%%|*}"
  name="${entry##*|}"

  if [[ -d "$DEST/$name/.git" ]]; then
    if [[ $UPDATE -eq 1 ]]; then
      if git -C "$DEST/$name" pull --depth 1 --quiet 2>/dev/null; then
        echo "更新  $name"
        ok=$((ok + 1))
      else
        echo "失败  $name（更新）"
        fail=$((fail + 1))
      fi
    else
      echo "跳过  $name（已存在，用 --update 更新）"
      skip=$((skip + 1))
    fi
    continue
  fi

  if git clone --depth 1 --quiet "$url" "$DEST/$name" 2>/dev/null; then
    echo "克隆  $name"
    ok=$((ok + 1))
  else
    echo "失败  $name  <- $url"
    fail=$((fail + 1))
  fi
done

echo
echo "完成：成功 $ok，跳过 $skip，失败 $fail"
echo "位置：$DEST"
echo
echo "注意：这些项目各自适用其原始许可证，请遵守。本项目仅借鉴设计思路，未复制代码。"
