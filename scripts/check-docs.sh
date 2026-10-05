#!/usr/bin/env bash
#
# 文档链接检查。
#
# 设计文档里大量互相引用。链接一旦失效，读者就会以为那份文档不存在，
# 而不是去别处找 —— 文档体系的可信度就是这么一点点崩掉的。
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$ROOT" <<'PY'
import os, re, sys

root = sys.argv[1]

# 要检查的 markdown 文件
targets = []
for dirpath, dirnames, filenames in os.walk(root):
    # 跳过参考项目与依赖目录
    dirnames[:] = [d for d in dirnames
                   if d not in ('.git', 'node_modules', '参考项目', 'dist', 'data')]
    for fn in filenames:
        if fn.endswith('.md'):
            targets.append(os.path.join(dirpath, fn))

# 匹配 [文本](链接)，跳过外链与锚点
link_re = re.compile(r'\[([^\]]*)\]\(([^)]+)\)')

def strip_code(text: str) -> str:
    r"""剥离围栏代码块与行内代码，避免把代码里的括号误判成链接。

    用等长替换（保留换行数）而不是直接删除，这样报告的行号仍然准确。
    例如正则 (\d{1,2}) 和 JSON 示例都会落在代码块里，不该被当成链接检查。
    """
    # 围栏代码块：整块替换成等量换行
    text = re.sub(r'```.*?```', lambda m: '\n' * m.group(0).count('\n'), text, flags=re.DOTALL)
    # 行内代码：替换成等长空格
    text = re.sub(r'`[^`\n]*`', lambda m: ' ' * len(m.group(0)), text)
    return text

broken = []
checked_files = 0
checked_links = 0

for path in sorted(targets):
    checked_files += 1
    with open(path, encoding='utf-8', errors='ignore') as f:
        raw_content = f.read()

    content = strip_code(raw_content)
    raw_by_line = raw_content.split('\n')

    for m in link_re.finditer(content):
        raw = m.group(2).strip()

        # 跳过外链、纯锚点、协议链接
        if raw.startswith(('http://', 'https://', 'mailto:', '#')):
            continue
        # 去掉锚点部分
        target = raw.split('#', 1)[0]
        if not target:
            continue
        # 去掉可能的 title 部分  "path" "title"
        target = target.split(' ')[0].strip('"').strip("'")
        if not target:
            continue

        checked_links += 1

        # 相对当前文件解析
        base = os.path.dirname(path)
        resolved = os.path.normpath(os.path.join(base, target))

        # URL 解码（中文路径在 markdown 里可能是 %XX 形式）
        try:
            from urllib.parse import unquote
            resolved = unquote(resolved)
        except Exception:
            pass

        if not os.path.exists(resolved):
            line_no = content[:m.start()].count('\n') + 1
            broken.append((os.path.relpath(path, root), line_no, raw, target))

print(f"检查了 {checked_files} 个文档，{checked_links} 条内部链接")

if not broken:
    print("PASS — 所有内部链接有效")
    sys.exit(0)

print()
print(f"FAIL — 发现 {len(broken)} 条失效链接：")
print()
for path, line_no, raw, target in broken:
    print(f"  {path}:{line_no}")
    print(f"    链接指向「{target}」，实际不存在")
print()
print("修一下引用的路径，或者把目标文件补上。")
sys.exit(1)
PY
