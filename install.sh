#!/usr/bin/env bash
# addipv6 管理脚本：装、更新、卸载、日常维护都在这儿
#
#   交互菜单： bash <(curl -fsSL https://raw.githubusercontent.com/byJoey/addipv6/main/install.sh)
#   直接安装： bash <(curl -fsSL https://raw.githubusercontent.com/byJoey/addipv6/main/install.sh) install
#   直接卸载： bash <(curl -fsSL https://raw.githubusercontent.com/byJoey/addipv6/main/install.sh) uninstall
set -uo pipefail

REPO="byJoey/addipv6"
BIN=/usr/local/bin/addipv6
UNIT=/etc/systemd/system/addipv6.service
RESTORE_UNIT=/etc/systemd/system/addipv6-restore.service
CONF_DIR=/etc/addipv6
STATE_DIR=/var/lib/addipv6
DEFAULT_PORT=8688

R='\033[31m'; G='\033[32m'; Y='\033[33m'; B='\033[36m'; D='\033[2m'; N='\033[0m'

# 下载用的临时目录。用全局变量配脚本级 trap，别用函数级 trap RETURN,
# 那个会在 local 变量销毁之后才触发，set -u 下直接报未绑定。
TMPDIR_DL=""
cleanup() { [ -n "${TMPDIR_DL:-}" ] && rm -rf "$TMPDIR_DL"; TMPDIR_DL=""; return 0; }
trap cleanup EXIT
ok()   { printf "${G}✓${N} %s\n" "$*"; }
warn() { printf "${Y}!${N} %s\n" "$*"; }
err()  { printf "${R}✗${N} %s\n" "$*"; }
info() { printf "  %s\n" "$*"; }
hr()   { printf "${D}%s${N}\n" "────────────────────────────────────────"; }
# 脚本经常是 bash <(curl ...) 跑起来的，这时候 $0 是 /dev/fd/63，
# 给用户看没意义，所以提示里一律用能直接复制的完整命令。
SELF="bash <(curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh)"

need_root() {
  [ "$(id -u)" -eq 0 ] || { err "要用 root 跑，前面加个 sudo"; exit 1; }
}
need_linux() {
  [ "$(uname -s)" = "Linux" ] || { err "这工具只能跑在 Linux 上"; exit 1; }
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo amd64 ;;
    aarch64|arm64)  echo arm64 ;;
    armv7l|armv6l)  echo arm ;;
    i386|i686)      echo 386 ;;
    riscv64)        echo riscv64 ;;
    *) return 1 ;;
  esac
}

installed() { [ -x "$BIN" ]; }

current_port() {
  [ -f "$UNIT" ] && grep -oE '\-listen [^ ]+' "$UNIT" | awk '{print $2}' | awk -F: '{print $NF}' || echo "$DEFAULT_PORT"
}

public_ip() {
  curl -fsS --max-time 6 https://api.ipify.org 2>/dev/null \
    || hostname -I 2>/dev/null | awk '{print $1}' \
    || echo "服务器IP"
}

write_unit() {
  local port="$1"
  cat > "$UNIT" <<EOF
[Unit]
Description=addipv6 web
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${BIN} -listen 0.0.0.0:${port}
Restart=on-failure
RestartSec=3s
# 改网卡地址要这个能力，其它权限一概不给。
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
NoNewPrivileges=yes
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

# 先从 Releases 拿预编译的；拿不到就退回本机用 Go 编译。
fetch_binary() {
  local arch url tmp
  arch=$(detect_arch) || { err "认不出这个 CPU 架构：$(uname -m)"; return 1; }
  cleanup
  TMPDIR_DL=$(mktemp -d)
  tmp="$TMPDIR_DL"
  url="https://github.com/${REPO}/releases/latest/download/addipv6-linux-${arch}"

  info "架构 ${arch}，从 Releases 下载"
  if curl -fsSL --retry 3 --max-time 180 -o "$tmp/addipv6" "$url" && [ -s "$tmp/addipv6" ]; then
    install -m 755 "$tmp/addipv6" "$BIN"
    return 0
  fi

  warn "Releases 下不动，改用源码编译"
  if ! command -v go >/dev/null 2>&1; then
    err "没装 Go，编译不了"
    info "要么等 Releases 恢复，要么先装 Go："
    info "  apt install -y golang   # 或者去 https://go.dev/dl 拿新版"
    return 1
  fi
  if ! command -v git >/dev/null 2>&1; then
    err "没装 git，拉不了源码。先 apt install -y git"
    return 1
  fi
  git clone --depth 1 "https://github.com/${REPO}.git" "$tmp/src" >/dev/null 2>&1 || {
    err "源码拉取失败，检查下网络"; return 1
  }
  ( cd "$tmp/src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/addipv6" . ) || {
    err "编译失败"; return 1
  }
  install -m 755 "$tmp/addipv6" "$BIN"
}

do_install() {
  need_root; need_linux
  local port="${1:-$DEFAULT_PORT}"

  if installed; then
    warn "已经装过了（$($BIN version 2>/dev/null)），这次按更新处理"
  fi

  fetch_binary || exit 1
  mkdir -p "$CONF_DIR"; chmod 700 "$CONF_DIR"

  write_unit "$port"
  systemctl enable --now addipv6 >/dev/null 2>&1
  sleep 2

  if ! systemctl is-active --quiet addipv6; then
    err "服务没起来"
    info "看日志：journalctl -u addipv6 -n 30 --no-pager"
    exit 1
  fi

  hr
  ok "装好了：$($BIN version)"
  show_access "$port"
  hr
  info "有防火墙记得放行 ${port}"
  info "再次打开菜单： ${SELF}"
}

show_access() {
  local port="${1:-$(current_port)}"
  local pw
  pw=$(cat "$CONF_DIR/password" 2>/dev/null || echo "(看 journalctl -u addipv6)")
  printf "  打开  ${B}http://%s:%s${N}\n" "$(public_ip)" "$port"
  printf "  密码  ${B}%s${N}\n" "$pw"
}

do_update() {
  need_root; need_linux
  installed || { err "还没装过，先选安装"; return 1; }
  local old latest
  old=$($BIN version 2>/dev/null | awk '{print $2}')
  latest=$(curl -fsS --max-time 10 "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
           | grep -oE '"tag_name": *"[^"]+"' | cut -d'"' -f4)
  if [ -n "$latest" ] && [ "$latest" = "$old" ]; then
    ok "已经是最新的 $old，不用更新"
    return 0
  fi
  [ -n "$latest" ] && info "$old  ->  $latest"
  fetch_binary || return 1
  systemctl restart addipv6 2>/dev/null
  sleep 1
  ok "更新完成：$($BIN version 2>/dev/null)"
  info "地址记录和 Cloudflare 配置都没动"
}

do_uninstall() {
  need_root
  local purge="${1:-}"
  systemctl disable --now addipv6 >/dev/null 2>&1
  systemctl disable --now addipv6-restore >/dev/null 2>&1
  rm -f "$UNIT" "$RESTORE_UNIT" "$BIN"
  systemctl daemon-reload >/dev/null 2>&1
  ok "程序和服务都删了"

  if [ "$purge" = "purge" ]; then
    rm -rf "$CONF_DIR" "$STATE_DIR"
    ok "配置和地址记录也清了"
    warn "已经加到网卡上的 IPv6 还在，重启后会自己消失"
  else
    info "配置还留着：$CONF_DIR 和 $STATE_DIR"
    info "要一起清掉就跑：${SELF} uninstall purge"
  fi
}

do_status() {
  hr
  if installed; then
    ok "已安装 $($BIN version 2>/dev/null)"
  else
    warn "没装"
    hr; return
  fi

  local act; act=$(systemctl is-active addipv6 2>/dev/null)
  if [ "$act" = "active" ]; then ok "服务运行中"; else err "服务没在跑（$act）"; fi

  if systemctl is-enabled --quiet addipv6-restore 2>/dev/null; then
    ok "开机恢复已打开"
  else
    info "开机恢复没开（在网页上点「开机保持」）"
  fi

  local n
  n=$(grep -c '"prefix"' "$STATE_DIR/state.json" 2>/dev/null || echo 0)
  info "本工具记录在案的地址：${n} 个"

  if [ -f "$CONF_DIR/config.json" ] && grep -qE '"(token|globalKey)": *"[^"]' "$CONF_DIR/config.json" 2>/dev/null; then
    ok "Cloudflare 已连接"
  else
    info "Cloudflare 还没连"
  fi

  hr
  [ "$act" = "active" ] && show_access
  hr
}

do_password() {
  need_root
  installed || { err "还没装过"; return 1; }
  printf "新密码（直接回车＝随机生成）："; read -r pw
  if [ -z "$pw" ]; then
    rm -f "$CONF_DIR/password"
    systemctl restart addipv6
    sleep 2
    ok "已重新随机生成"
  else
    mkdir -p "$CONF_DIR"; chmod 700 "$CONF_DIR"
    printf '%s\n' "$pw" > "$CONF_DIR/password"
    chmod 600 "$CONF_DIR/password"
    systemctl restart addipv6
    sleep 1
    ok "改好了"
  fi
  show_access
}

do_port() {
  need_root
  installed || { err "还没装过"; return 1; }
  printf "新端口（当前 %s）：" "$(current_port)"; read -r port
  case "$port" in
    ''|*[!0-9]*) err "端口要是数字"; return 1 ;;
  esac
  [ "$port" -ge 1 ] && [ "$port" -le 65535 ] || { err "端口范围不对"; return 1; }
  write_unit "$port"
  systemctl restart addipv6
  sleep 1
  ok "换到 $port 了"
  warn "防火墙记得放行新端口"
  show_access "$port"
}

menu() {
  while :; do
    printf "\n"
    hr
    printf "  ${B}addipv6${N}  批量管理 IPv6 + 解析到 Cloudflare\n"
    printf "  ${D}github.com/%s${N}\n" "$REPO"
    hr
    if installed; then
      printf "  状态：${G}已安装${N} %s  服务 %s\n" \
        "$($BIN version 2>/dev/null | awk '{print $2}')" \
        "$(systemctl is-active addipv6 2>/dev/null)"
    else
      printf "  状态：${Y}未安装${N}\n"
    fi
    hr
    printf "   1) 安装\n"
    printf "   2) 更新到最新版\n"
    printf "   3) 卸载\n"
    printf "  ${D}────${N}\n"
    printf "   4) 启动    5) 停止    6) 重启\n"
    printf "   7) 查看状态和访问地址\n"
    printf "   8) 看日志\n"
    printf "  ${D}────${N}\n"
    printf "   9) 改密码\n"
    printf "  10) 改端口\n"
    printf "   0) 退出\n"
    hr
    printf "选择: "; read -r c
    case "$c" in
      1) do_install ;;
      2) do_update ;;
      3) printf "配置和地址记录也一起删吗？(y/N): "; read -r y
         [[ "$y" =~ ^[Yy]$ ]] && do_uninstall purge || do_uninstall ;;
      4) systemctl start addipv6 && ok "起来了" ;;
      5) systemctl stop addipv6 && ok "停了" ;;
      6) systemctl restart addipv6 && ok "重启了" ;;
      7) do_status ;;
      8) journalctl -u addipv6 -n 50 --no-pager ;;
      9) do_password ;;
      10) do_port ;;
      0) exit 0 ;;
      *) err "没这个选项" ;;
    esac
  done
}

case "${1:-}" in
  install)   do_install "${2:-$DEFAULT_PORT}" ;;
  update)    do_update ;;
  uninstall) do_uninstall "${2:-}" ;;
  status)    do_status ;;
  "")
    # curl | bash 这种情况下 stdin 被占着，没法交互，直接按安装处理。
    if [ -t 0 ]; then need_root; need_linux; menu; else do_install; fi
    ;;
  *) err "不认识的参数：$1"
     info "用法：${SELF} [install|update|uninstall [purge]|status]"
     exit 1 ;;
esac
