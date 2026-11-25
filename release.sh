#!/bin/sh
# 打 Release：创建 GitHub Release 并上传模块包。
#
# 用法：./release.sh [tag]
#   tag 省略时从 module/module.prop 读版本号（v1.50.0-go → v1.50.0）
#
# 为什么要它：面板「检查更新」读的是 GitHub Release，
# 只推代码不打 Release，面板就永远显示旧版本号，
# 用户会以为「版本对不上」。每次发版都必须跑一遍。
set -e
cd "$(dirname "$0")"

TOKEN=$(grep -o 'https://[^:]*:[^@]*@github.com' ~/.git-credentials 2>/dev/null | head -1 | sed 's|https://||;s|@github.com||' | cut -d: -f2)
if [ -z "$TOKEN" ]; then
  echo "找不到 GitHub token（~/.git-credentials）" >&2
  exit 1
fi

# 版本号：module.prop 里是 v1.50.0-go，tag 用 v1.50.0
VER=$(grep '^version=' module/module.prop | cut -d= -f2 | tr -d ' ')
TAG=${1:-$(echo "$VER" | sed 's/-go$//')}
ZIP="AllIn-Go.zip"
REPO="Bxdj99/allin-go"

if [ ! -f "$ZIP" ]; then
  echo "缺少 $ZIP，先跑 ./build.sh" >&2
  exit 1
fi

echo "==> 版本 $VER  tag $TAG"

# tag 必须指向当前 HEAD 并已推送
git tag -f -a "$TAG" -m "All In (Go) $TAG" >/dev/null 2>&1 || true
git push -f origin "$TAG" >/dev/null 2>&1 || echo "警告：tag 推送失败，继续尝试建 Release"

# 正文：取最近 5 条提交作为更新说明
BODY=$(git log -5 --pretty='- %s' | sed 's/"/\\"/g')

cat > /tmp/rel.json <<EOF
{
  "tag_name": "$TAG",
  "name": "All In (Go) $TAG",
  "body": "$BODY",
  "draft": false,
  "prerelease": false
}
EOF

echo "==> 创建 / 复用 Release"
# 先查是否已存在：重跑脚本时不该报 already_exists 直接退出，
# 那样资产永远传不上去（踩过：超时那次的 Release 建成了，重跑却失败）。
EXIST=$(curl -s -H "Authorization: token $TOKEN" \
  "https://api.github.com/repos/$REPO/releases/tags/$TAG")
RID=$(echo "$EXIST" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('id') or '')" 2>/dev/null || echo "")

if [ -n "$RID" ]; then
  echo "    Release 已存在，复用 id=$RID"
  # 已存在的同名资产要先删，否则上传会 422
  for AID in $(echo "$EXIST" | python3 -c "
import sys,json
d=json.load(sys.stdin)
for a in (d.get('assets') or []):
    if a.get('name')=='$ZIP': print(a.get('id'))
" 2>/dev/null); do
    echo "    删除旧资产 $AID"
    curl -s -X DELETE -H "Authorization: token $TOKEN" \
      "https://api.github.com/repos/$REPO/releases/assets/$AID" >/dev/null
  done
else
  RESP=$(curl -s -X POST \
    -H "Authorization: token $TOKEN" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/$REPO/releases" \
    -d @/tmp/rel.json)
  RID=$(echo "$RESP" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('id') or '')" 2>/dev/null || echo "")
  if [ -z "$RID" ]; then
    echo "创建失败：$(echo "$RESP" | head -c 300)" >&2
    exit 1
  fi
  echo "    新建 release id=$RID"
fi

echo "==> 上传 $ZIP（可能较慢，失败会自动重试）"
for i in 1 2 3 4 5; do
  # --limit-rate 300k + --max-time 600：
  # 不限速时 3.4MB 包在移动网络下经常传到一半断掉（实测多次 HTTP 000）。
  # 限速牺牲一点速度，换来稳定完成。
  CODE=$(curl -s -o /tmp/up.json -w '%{http_code}' --max-time 600 --limit-rate 300k \
    -X POST \
    -H "Authorization: token $TOKEN" \
    -H "Content-Type: application/zip" \
    --data-binary @"$ZIP" \
    "https://uploads.github.com/repos/$REPO/releases/$RID/assets?name=$(basename "$ZIP")")
  echo "    第 $i 次：HTTP $CODE"
  [ "$CODE" = "201" ] && break
  sleep 8
done

if [ "$CODE" != "201" ]; then
  echo "上传失败：$(head -c 200 /tmp/up.json)" >&2
  exit 1
fi

echo "==> 完成：https://github.com/$REPO/releases/tag/$TAG"
