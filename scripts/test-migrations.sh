#!/usr/bin/env bash
#
# 迁移验证。
#
# 竞品最容易踩的坑：迁移脚本只在空库上测过，上线后遇到有数据的库就失败
#（典型场景是加一个 NOT NULL 列但没给默认值）。
#
# 这个脚本跑 assemble 包里的迁移测试，覆盖三种场景：
#   1. 空库
#   2. 重复执行（幂等）
#   3. 有数据的库（增量）
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO="${GO:-go}"
if ! command -v "$GO" >/dev/null 2>&1; then
    for candidate in "$HOME/sdk/go/bin/go" /usr/local/go/bin/go /opt/homebrew/bin/go; do
        if [[ -x "$candidate" ]]; then GO="$candidate"; break; fi
    done
fi

if ! command -v "$GO" >/dev/null 2>&1 && [[ ! -x "$GO" ]]; then
    echo "找不到 go 命令。请安装 Go 1.25+，或用 GO=/path/to/go 指定。"
    exit 1
fi

echo "== 迁移测试 =="
"$GO" test ./pkg/kernel/assemble/... -run 'TestKernelMigrations|TestInstanceLock|TestStore_' -v

echo
echo "PASS — 迁移在空库 / 重复执行 / 有数据的库三种场景下均通过"
