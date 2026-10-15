#!/usr/bin/env bash
# 一键发版：打 tag 并推送，GitHub Actions 会自动构建二进制与 Docker 镜像。
#
# 用法：
#   ./tools/release.sh 1.1.0            # 发布 v1.1.0
#   ./tools/release.sh 1.1.0 "修复签到" # 带说明（可选）
#
# 前置条件：
#   - 工作区干净（无未提交改动）
#   - 本地已关联 origin 且具备推送权限
set -euo pipefail

VERSION="${1:-}"
NOTE="${2:-}"

if [[ -z "$VERSION" ]]; then
  echo "用法: $0 <版本号> [说明]   例: $0 1.1.0 \"修复签到\""
  exit 1
fi

# 去掉可能带的 v 前缀，统一成 vX.Y.Z
VERSION="${VERSION#v}"
TAG="v${VERSION}"

if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "❌ 版本号需为 X.Y.Z 形式（如 1.1.0），当前: $VERSION"
  exit 1
fi

if ! git diff-index --quiet HEAD --; then
  echo "❌ 工作区有未提交改动，请先提交或 stash"
  git status --short
  exit 1
fi

if git rev-parse "$TAG" >/dev/null 2>&1; then
  echo "❌ 标签 $TAG 已存在"
  exit 1
fi

echo "→ 当前分支: $(git rev-parse --abbrev-ref HEAD)"
echo "→ 将创建标签: $TAG"
read -r -p "确认发版? [y/N] " ans
[[ "$ans" =~ ^[Yy]$ ]] || { echo "已取消"; exit 0; }

git tag -a "$TAG" -m "${NOTE:-Release $TAG}"
git push origin "$TAG"

echo ""
echo "✅ 已推送 $TAG"
echo "   GitHub Actions 将自动："
echo "   · 构建 6 平台二进制并附加到 Release"
echo "   · 构建并推送镜像 ghcr.io/<owner>/<repo>:${VERSION} 与 :latest"
echo ""
echo "   查看进度: https://github.com/$(git remote get-url origin | sed -E 's#.*github.com[:/](.*)\.git#\1#')/actions"
