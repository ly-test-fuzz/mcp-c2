#!/usr/bin/env bash
# scripts/build.sh — debugMcp 构建 / 安装 / 打包 一体脚本。
#
# 子命令:
#   ./scripts/build.sh [hub|shim|client]...   # 构建指定端到 dist/（默认全量）
#   ./scripts/build.sh install [bindir]       # 构建本机端 + 装到 bindir（默认 /usr/local/bin，需 sudo）
#   ./scripts/build.sh assets [plugin_dir]    # 全平台构建 + 整合到 plugin 的 assets/bin/
#   ./scripts/build.sh version                # 打印当前版本串
#
# 版本来源（优先级）: $VERSION > git describe --tags > git 短 sha > dev
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DIST="${DIST:-dist}"
LDFLAGS="-s -w"            # 去掉符号表和调试信息, 减小体积
GOFLAGS="-trimpath"        # 去除编译机绝对路径
export CGO_ENABLED=0       # 纯 Go, 保证交叉编译与静态链接

# ---- 版本 ----
compute_version() {
  if [[ -n "${VERSION:-}" ]]; then echo "$VERSION"; return; fi
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if git describe --tags --always --dirty 2>/dev/null; then return; fi
    git rev-parse --short HEAD 2>/dev/null && return
  fi
  echo "dev"
}
VERSION_STR="$(compute_version)"
# 注入 internal/version.Version，供各 main 的 -version 输出
VERSION_LDFLAGS="-X debugmcp/internal/version.Version=${VERSION_STR}"

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
  echo "  -> $out  ($goos/$goarch, v${VERSION_STR})"
  GOOS="$goos" GOARCH="$goarch" \
    go build $GOFLAGS -ldflags "$LDFLAGS $VERSION_LDFLAGS" -o "$out" "./$pkg"
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

build_all() {
  rm -rf "$DIST"
  mkdir -p "$DIST"
  build_hub
  build_shim
  build_client
  echo
  echo "构建完成 (v${VERSION_STR}), 产物树:"
  (cd "$DIST" && find . -type f | sort | sed 's|^\./|  dist/|')
}

# ---- install: 装本机端到 PATH ----
do_install() {
  local bindir="${1:-/usr/local/bin}"
  echo "[install] 构建本机端 (hub/shim/cli/probe) 并装到 $bindir (v${VERSION_STR})"
  rm -rf "$DIST"; mkdir -p "$DIST"
  build_hub
  build_shim

  # sudo 仅在目标目录不可写时启用
  local sudo=""
  if [[ ! -w "$bindir" ]]; then
    sudo="sudo"
    echo "  (目标 $bindir 不可写, 启用 sudo)"
  fi
  $sudo mkdir -p "$bindir"
  $sudo install -m 0755 \
    "$DIST/hub/debugmcp-hub" \
    "$DIST/hub/debugmcp-cli" \
    "$DIST/hub/debugmcp-probe" \
    "$DIST/shim/debugmcp-shim" \
    "$bindir/"

  echo
  echo "已安装:"
  for b in debugmcp-hub debugmcp-cli debugmcp-probe debugmcp-shim; do
    local p
    p="$bindir/$b"
    if [[ -x "$p" ]]; then
      printf "  %-22s %s\n" "$b" "$($p -version 2>/dev/null || echo "$p")"
    fi
  done
}

# ---- assets: 全平台构建并整合进 plugin 包 ----
do_assets() {
  local plugin_dir="${1:-plugins/debugmcp}"
  local assets_bin="$plugin_dir/assets/bin"
  echo "[assets] 全平台构建 + 整合到 $assets_bin (v${VERSION_STR})"
  build_all

  rm -rf "$assets_bin"
  mkdir -p "$assets_bin/hub" "$assets_bin/shim" "$assets_bin/client"

  # 本机端 (hub/shim/cli/probe): linux/amd64
  cp "$DIST/hub/debugmcp-hub"   "$assets_bin/hub/"
  cp "$DIST/hub/debugmcp-cli"   "$assets_bin/hub/"
  cp "$DIST/hub/debugmcp-probe" "$assets_bin/hub/"
  cp "$DIST/shim/debugmcp-shim" "$assets_bin/shim/"

  # client: 全平台矩阵
  local t goos goarch src dst ext
  for t in "${CLIENT_TARGETS[@]}"; do
    goos="${t%%/*}"; goarch="${t##*/}"
    ext=""
    [[ "$goos" == "windows" ]] && ext=".exe"
    src="$DIST/client/${goos}-${goarch}/debugmcp-agent${ext}"
    dst="$assets_bin/client/${goos}-${goarch}"
    mkdir -p "$dst"
    cp "$src" "$dst/"
  done

  echo
  echo "plugin assets 就绪:"
  (cd "$assets_bin" && find . -type f | sort | sed 's|^\./|  |')
  echo
  echo "总体积: $(du -sh "$assets_bin" | cut -f1)"
}

usage() {
  cat <<EOF
用法: $0 <子命令>
  (无) / hub / shim / client    构建到 dist/
  install [bindir]              构建本机端 + 装到 bindir（默认 /usr/local/bin）
  assets [plugin_dir]           全平台构建 + 整合到 plugin assets/bin/
  version                       打印版本串

版本来源优先级: \$VERSION > git describe > git 短 sha > dev
EOF
}

main() {
  [[ $# -eq 0 ]] && { build_all; exit 0; }
  case "$1" in
    install) shift; do_install "$@" ;;
    assets)  shift; do_assets "$@" ;;
    version) echo "$VERSION_STR" ;;
    -h|--help|help) usage ;;
    hub|shim|client)
      # 校验剩余都是合法端
      for t in "$@"; do
        case "$t" in hub|shim|client) ;; *) echo "未知端: $t" >&2; exit 2 ;; esac
      done
      rm -rf "$DIST"; mkdir -p "$DIST"
      for t in "$@"; do
        case "$t" in hub) build_hub ;; shim) build_shim ;; client) build_client ;; esac
      done
      echo "构建完成 (v${VERSION_STR})."
      ;;
    *) echo "未知子命令: $1" >&2; usage; exit 2 ;;
  esac
}

main "$@"
