#!/usr/bin/env bash
#
# 内核纯度检查。
#
# 内核（pkg/kernel/）不允许出现任何具体外部系统的名字。
# 这是三条设计公理里第二条的自动化保证 —— 靠人自觉是守不住的。
#
# 判断方法：剥离 Go 源码里的注释与字符串字面量，再搜关键词。
# 剥离是必要的，否则 "内核里不出现 Emby" 这句注释本身就会被误报。
#
# 豁免：在那一行加注释 // purity:allow 即可，但要说明理由。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TARGET="$ROOT/pkg/kernel"

if [[ ! -d "$TARGET" ]]; then
  echo "找不到内核目录：$TARGET"
  exit 1
fi

python3 - "$TARGET" <<'PY'
import os, re, sys

root = sys.argv[1]

# 具体外部系统的名字 —— 内核绝对不该知道它们的存在
FORBIDDEN = [
    # 媒体服务器
    "emby", "jellyfin", "plex",
    # 下载器
    "qbittorrent", "transmission", "rtorrent", "aria2", "deluge",
    # 元数据源
    "tmdb", "themoviedb", "thetvdb", "douban", "bangumi", "anilist",
    # 资源自动化
    "moviepilot", "nastool", "jackett", "prowlarr", "alist",
    # 通知渠道
    "telegram", "wechat", "feishu", "dingtalk", "discord", "slack",
    # 网盘与支付
    "stripe", "turnstile", "p115", "115pan",
]

# 业务领域词 —— 内核可以有"认证主体"这类基础抽象，但不该有"积分""订单"
BUSINESS = [
    "points", "redeem", "subscribe", "playback", "ticket", "coupon", "invoice",
]

def strip_go(src: str) -> str:
    """剥离注释与字符串字面量，只留下会被编译的标识符与关键字。"""
    out, i, n = [], 0, len(src)
    while i < n:
        c = src[i]
        # 行注释
        if c == '/' and i + 1 < n and src[i+1] == '/':
            i = src.find('\n', i)
            if i == -1: break
            continue
        # 块注释
        if c == '/' and i + 1 < n and src[i+1] == '*':
            j = src.find('*/', i + 2)
            i = n if j == -1 else j + 2
            continue
        # 解释型字符串
        if c == '`':
            j = src.find('`', i + 1)
            i = n if j == -1 else j + 1
            continue
        # 双引号字符串
        if c == '"':
            i += 1
            while i < n:
                if src[i] == '\\': i += 2; continue
                if src[i] == '"': i += 1; break
                i += 1
            continue
        # 单引号 rune
        if c == "'":
            i += 1
            while i < n:
                if src[i] == '\\': i += 2; continue
                if src[i] == "'": i += 1; break
                i += 1
            continue
        out.append(c)
        i += 1
    return ''.join(out)

violations = []
checked = 0

for dirpath, _, filenames in os.walk(root):
    for fn in filenames:
        if not fn.endswith('.go'):
            continue
        path = os.path.join(dirpath, fn)
        checked += 1

        with open(path, encoding='utf-8', errors='ignore') as f:
            raw = f.read()

        # 逐行判断豁免标记（豁免要写在原始行上）
        allow_lines = {
            i for i, line in enumerate(raw.split('\n'), 1)
            if 'purity:allow' in line
        }

        stripped = strip_go(raw)
        lowered = stripped.lower()

        # 用剥离后的内容定位，再映射回原始行号
        for word in FORBIDDEN + BUSINESS:
            for m in re.finditer(r'\b' + re.escape(word), lowered):
                # 在原始文本里找到大致位置以报告行号
                pos = m.start()
                line_no = stripped[:pos].count('\n') + 1
                if line_no in allow_lines:
                    continue
                text = stripped.split('\n')[line_no - 1].strip()[:90]
                violations.append((os.path.relpath(path, root), line_no, word, text))

print(f"检查了 {checked} 个 Go 文件")

if not violations:
    print("PASS — 内核中没有出现业务或外部系统的名字")
    sys.exit(0)

print()
print(f"FAIL — 发现 {len(violations)} 处违规：")
print()
for path, line_no, word, text in sorted(violations):
    print(f"  {path}:{line_no}")
    print(f"    命中「{word}」：{text}")
    print()

print("内核只应认识三样东西：契约、注册表、事件。")
print("具体实现请放到 internal/provider/ 或 internal/plugin/。")
print("确实需要豁免时，在该行加注释 // purity:allow 并说明理由。")
sys.exit(1)
PY
