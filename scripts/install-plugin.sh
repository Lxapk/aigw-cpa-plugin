#!/usr/bin/env bash
# 在 Debian 12 (x86_64) 腾讯云服务器上编译并安装 aigw-reverse-proxy 插件。
#
# 前提：CPA 以 Docker 方式运行，且 config.yaml 挂在宿主机的某个目录。
# 本脚本会自动探测 CPA 容器的挂载点，把编译产物放到正确位置。
#
# 用法：
#   bash install-plugin.sh                # 自动探测
#   bash install-plugin.sh --host-dir /root/cpa   # 手动指定宿主目录

set -euo pipefail

PLUGIN_ID="aigw-reverse-proxy"
GO_VERSION="1.27.1"
GO_TARBALL="go${GO_VERSION}.linux-amd64.tar.gz"
GO_URL="https://go.dev/dl/${GO_TARBALL}"

HOST_DIR=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --host-dir) HOST_DIR="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

log()  { printf '\033[32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[!]\033[0m %s\n' "$*"; }
die()  { printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- 1. 架构
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) die "不支持的架构: $ARCH" ;;
esac
log "系统架构 $ARCH -> GOARCH=$GOARCH"

# ---------------------------------------------------------------- 2. 依赖
log "安装编译依赖（gcc / make / wget）"
export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq gcc make wget ca-certificates
else
  die "本脚本针对 Debian/Ubuntu，未找到 apt-get"
fi

# ---------------------------------------------------------------- 3. Go
need_go=1
if command -v go >/dev/null 2>&1; then
  cur="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  log "已安装 Go: $cur"
  # 需要 >= 1.26
  if [[ "$(printf '%s\n1.26\n' "$cur" | sort -V | head -1)" == "1.26" ]]; then
    need_go=0
  else
    warn "Go 版本过旧（需要 >= 1.26），将升级"
  fi
fi

if [[ "$need_go" == "1" ]]; then
  log "下载并安装 Go ${GO_VERSION}"
  pushd /tmp >/dev/null
  wget -q --show-progress -O "$GO_TARBALL" "$GO_URL"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$GO_TARBALL"
  rm -f "$GO_TARBALL"
  popd >/dev/null
fi
export PATH="/usr/local/go/bin:$PATH"
command -v go >/dev/null 2>&1 || die "Go 安装失败"
log "使用 $(go version)"

# ---------------------------------------------------------------- 4. 源码
SRC_DIR=""
for candidate in "./aigw-cpa-plugin" "." ; do
  if [[ -f "$candidate/go.mod" ]]; then
    SRC_DIR="$candidate"; break
  fi
done
[[ -n "$SRC_DIR" ]] || die "未找到 go.mod。请先解包源码：tar xzf aigw-cpa-plugin-src.tar.gz"
cd "$SRC_DIR"
log "源码目录: $(pwd)"

log "下载 Go 依赖"
go mod download

# ---------------------------------------------------------------- 5. 编译
log "编译插件（cgo / c-shared）"
CGO_ENABLED=1 GOOS=linux GOARCH="$GOARCH" \
  go build -buildmode=c-shared -trimpath -o "${PLUGIN_ID}.so" .

[[ -f "${PLUGIN_ID}.so" ]] || die "编译产物缺失"

# 架构自检
if command -v readelf >/dev/null 2>&1; then
  actual="$(readelf -h "${PLUGIN_ID}.so" | awk -F: '/Machine/{print $2}' | xargs)"
  log "产物架构: $actual"
  case "$GOARCH" in
    amd64) [[ "$actual" == *"X86-64"* ]] || die "架构不符：期望 x86-64，实际 $actual" ;;
    arm64) [[ "$actual" == *"AArch64"* ]] || die "架构不符：期望 AArch64，实际 $actual" ;;
  esac
fi

# ABI 符号自检
if command -v nm >/dev/null 2>&1; then
  nm -D --defined-only "${PLUGIN_ID}.so" | grep -q cliproxy_plugin_init \
    || die "缺少 cliproxy_plugin_init 导出符号"
  log "ABI 符号检查通过"
fi

# ---------------------------------------------------------------- 6. 冒烟测试
if [[ -d smoke ]]; then
  log "运行 dlopen 冒烟测试"
  CGO_ENABLED=1 go build -o smoke-bin ./smoke
  if ./smoke-bin "${PLUGIN_ID}.so" | tail -3; then
    log "冒烟测试通过"
  else
    warn "冒烟测试未通过，请检查上面的输出"
  fi
fi

# ---------------------------------------------------------------- 7. 找 CPA 挂载点
find_cpa_host_dir() {
  command -v docker >/dev/null 2>&1 || return 1
  local cid
  for pat in cliproxy cpa cli-proxy-api newapi; do
    cid="$(docker ps --format '{{.ID}} {{.Image}} {{.Names}}' | grep -i "$pat" | head -1 | awk '{print $1}')"
    [[ -n "$cid" ]] && break
  done
  [[ -n "$cid" ]] || return 1
  echo "$cid"
}

if [[ -z "$HOST_DIR" ]]; then
  cid="$(find_cpa_host_dir || true)"
  if [[ -n "$cid" ]]; then
    log "发现 CPA 容器: $cid"
    echo "--- 容器挂载 ---"
    docker inspect "$cid" --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'
    # 尝试找一个像是 CPA 工作目录的挂载（含 config.yaml）
    while IFS=' -> ' read -r src dst; do
      [[ -d "$src" ]] || continue
      if [[ -f "$src/config.yaml" || -f "$src/config.yml" ]]; then
        HOST_DIR="$src"; break
      fi
    done < <(docker inspect "$cid" --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}')
    if [[ -z "$HOST_DIR" ]]; then
      warn "容器挂载里没找到含 config.yaml 的目录"
      echo "请手动指定：bash $0 --host-dir <宿主上含 config.yaml 的目录>"
      exit 2
    fi
    log "自动探测到 CPA 宿主目录: $HOST_DIR"
  else
    warn "未发现运行中的 CPA 容器"
    echo "请手动指定：bash $0 --host-dir <宿主上含 config.yaml 的目录>"
    exit 2
  fi
fi

[[ -d "$HOST_DIR" ]] || die "目录不存在: $HOST_DIR"

# ---------------------------------------------------------------- 8. 安装
PLUGIN_SUBDIR="plugins/linux/${GOARCH}"
log "创建插件目录: ${HOST_DIR}/${PLUGIN_SUBDIR}"
mkdir -p "${HOST_DIR}/${PLUGIN_SUBDIR}"
install -m 755 "${PLUGIN_ID}.so" "${HOST_DIR}/${PLUGIN_SUBDIR}/${PLUGIN_ID}.so"
log "已安装到 ${HOST_DIR}/${PLUGIN_SUBDIR}/${PLUGIN_ID}.so"

# ---------------------------------------------------------------- 9. 配置提示
CFG=""
for f in "${HOST_DIR}/config.yaml" "${HOST_DIR}/config.yml"; do
  [[ -f "$f" ]] && CFG="$f" && break
done

echo
log "完成。接下来需要手动确认 config.yaml："
echo
if [[ -n "$CFG" ]]; then
  echo "  配置文件: $CFG"
  grep -n "^plugins:" "$CFG" >/dev/null 2>&1 \
    && warn "已存在 plugins 段，请确认包含以下内容" \
    || warn "尚未配置 plugins 段，请添加："
else
  warn "未找到 config.yaml，请在 CPA 工作目录创建/修改："
fi
cat <<'YAML'

  plugins:
    enabled: true
    dir: "plugins"
    configs:
      aigw-reverse-proxy:
        enabled: true
        priority: 10
        api_key: "sk-改成你自己的密钥"
        allow_no_key: false
        default_provider: "trae"

YAML
echo "  然后重启 CPA 容器："
echo "    docker restart <容器名>"
echo
echo "  验证（日志应出现 plugin loaded）："
echo "    docker logs <容器名> 2>&1 | grep -i plugin"
echo
