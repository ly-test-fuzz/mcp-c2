#!/usr/bin/env bash
# scripts/build.sh — 按“三端”分类构建 debugMcp，产物整合到 dist/。
#
#   hub   端 (操作者守护进程, 本机 linux/amd64): debugmcp-hub + debugmcp-probe + debugmcp-cli
#   shim  端 (Claude Code 拉起的 MCP 接入, 本机): debugmcp-shim
#   client端 (目标机回连 agent, 交叉编译):       debugmcp-agent
#
# 用法:
#   ./scripts/build.sh                 # 默认全量构建
#   ./scripts/build.sh client          # 只构建 client 端
#   ./scripts/build.sh hub shim        # 只构建指定端
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DIST="${DIST:-dist}"
LDFLAGS="-s -w"            # 去掉符号表和调试信息, 减小体积
GOFLAGS="-trimpath"        # 去除编译机绝对路径
export CGO_ENABLED=0       # 纯 Go, 保证交叉编译与静态链接

# 目标机平台矩阵 (client 端交叉编译)
CLIENT_TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
  "windows/arm64"
)

# 本机平台 (hub / shim 端), 默认宿主平台
HOST_OS="${GOOS:-$(go env GOOS)}"
HOST_ARCH="${GOARCH:-$(go env GOARCH)}"

build_one() { # <goos> <goarch> <pkg> <outfile>
  local goos="$1" goarch="$2" pkg="$3" out="$4"
  echo "  -> $out  ($goos/$goarch)"
  GOOS="$goos" GOARCH="$goarch" \
    go build $GOFLAGS -ldflags "$LDFLAGS" -o "$out" "./$pkg"
}

build_hub() {
  echo "[hub] 操作者守护进程端 ($HOST_OS/$HOST_ARCH)"
  local outdir="$DIST/hub"
  mkdir -p "$outdir"
  build_one "$HOST_OS" "$HOST_ARCH" cmd/hub   "$outdir/debugmcp-hub"
  build_one "$HOST_OS" "$HOST_ARCH" cmd/probe "$outdir/debugmcp-probe"
  build_one "$HOST_OS" "$HOST_ARCH" cmd/cli   "$outdir/debugmcp-cli"
}

build_shim() {
  echo "[shim] Claude Code MCP 接入端 ($HOST_OS/$HOST_ARCH)"
  local outdir="$DIST/shim"
  mkdir -p "$outdir"
  build_one "$HOST_OS" "$HOST_ARCH" cmd/shim "$outdir/debugmcp-shim"
}

build_client() {
  echo "[client] 目标机 agent 端 (交叉编译)"
  local t goos goarch outdir ext
  for t in "${CLIENT_TARGETS[@]}"; do
    goos="${t%%/*}"
    goarch="${t##*/}"
    outdir="$DIST/client/${goos}-${goarch}"
    mkdir -p "$outdir"
    ext=""
    [[ "$goos" == "windows" ]] && ext=".exe"
    build_one "$goos" "$goarch" cmd/agent "$outdir/debugmcp-agent${ext}"
  done
}

usage() {
  cat <<EOF
用法: $0 [hub|shim|client]...
  不带参数 = 构建全部三端
EOF
}

main() {
  local targets=("$@")
  [[ ${#targets[@]} -eq 0 ]] && targets=(hub shim client)

  # 校验
  for t in "${targets[@]}"; do
    case "$t" in
      hub|shim|client) ;;
      *) echo "未知端: $t" >&2; usage; exit 2 ;;
    esac
  done

  rm -rf "$DIST"
  mkdir -p "$DIST"

  for t in "${targets[@]}"; do
    case "$t" in
      hub)    build_hub ;;
      shim)   build_shim ;;
      client) build_client ;;
    esac
  done

  echo
  echo "构建完成, 产物树:"
  (cd "$DIST" && find . -type f | sort | sed 's|^\./|  dist/|')
}

main "$@"
