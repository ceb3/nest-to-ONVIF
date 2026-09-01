# Shared helpers for nest-to-ONVIF host setup scripts.
# shellcheck shell=bash
setup_common_self="${BASH_SOURCE[0]}"

setup_die() {
  echo "error: $*" >&2
  exit 1
}

setup_info() {
  echo "==> $*"
}

setup_warn() {
  echo "warning: $*" >&2
}

setup_require_linux() {
  case "$(uname -s)" in
    Linux) ;;
    *) setup_die "this script requires Linux (got $(uname -s))" ;;
  esac
}

setup_bin_dir() {
  if [[ -n "${NEST_SETUP_BIN_DIR:-}" ]]; then
    printf '%s\n' "$NEST_SETUP_BIN_DIR"
    return
  fi
  local libdir
  libdir="$(cd "$(dirname "${setup_common_self}")" && pwd)"
  dirname "$libdir"
}

setup_repo_root() {
  if [[ -n "${NEST_BRIDGE_ROOT:-}" ]]; then
    printf '%s\n' "$NEST_BRIDGE_ROOT"
    return
  fi
  local root
  root="$(cd "$(setup_bin_dir)/.." && pwd)"
  if [[ ! -f "$root/deploy/docker-compose.yml" ]]; then
    setup_die "could not find repository root (no deploy/docker-compose.yml); set NEST_BRIDGE_ROOT"
  fi
  printf '%s\n' "$root"
}

setup_deploy_dir() {
  printf '%s/deploy\n' "$(setup_repo_root)"
}

setup_config_path() {
  if [[ -n "${NEST_BRIDGE_CONFIG:-}" ]]; then
    printf '%s\n' "$NEST_BRIDGE_CONFIG"
    return
  fi
  printf '%s/config.yaml\n' "$(setup_repo_root)"
}

setup_bridge_bin() {
  local root bin
  root="$(setup_repo_root)"
  for bin in "$root/bin/nest-bridge" "$root/nest-bridge"; do
    if [[ -x "$bin" ]]; then
      printf '%s\n' "$bin"
      return
    fi
  done
  if command -v nest-bridge >/dev/null 2>&1; then
    command -v nest-bridge
    return
  fi
  setup_die "nest-bridge binary not found; run: bin/build-bridge"
}

setup_has_command() {
  command -v "$1" >/dev/null 2>&1
}

setup_check_docker_compose() {
  if ! setup_has_command docker; then
    return 1
  fi
  docker compose version >/dev/null 2>&1
}

setup_detect_parent_iface() {
  local iface
  iface="$(ip route show default 2>/dev/null | awk '/default/ { for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit } }')"
  if [[ -n "$iface" ]]; then
    printf '%s\n' "$iface"
    return
  fi
  printf '%s\n' "eth0"
}

setup_detect_host_ip() {
  local iface ip
  iface="$(setup_detect_parent_iface)"
  ip="$(ip -4 -brief addr show dev "$iface" 2>/dev/null | awk '{print $3}' | cut -d/ -f1 | head -1)"
  if [[ -n "$ip" ]]; then
    printf '%s\n' "$ip"
    return
  fi
  ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  if [[ -n "$ip" ]]; then
    printf '%s\n' "$ip"
    return
  fi
  setup_die "could not detect a LAN IPv4 address"
}

setup_load_deploy_env() {
  local deploy_dir="${1:-$(setup_deploy_dir)}"
  local env_file="$deploy_dir/deploy.env"
  HOST_IP=""
  PARENT_IFACE=""
  INSTALL_ROOT=""
  if [[ ! -f "$env_file" ]]; then
    return 0
  fi
  # shellcheck disable=SC1090
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%%#*}"
    line="$(echo "$line" | xargs)"
    [[ -z "$line" || "$line" != *=* ]] && continue
    local key="${line%%=*}"
    local value="${line#*=}"
    value="${value%\"}"
    value="${value#\"}"
    value="${value%\'}"
    value="${value#\'}"
    case "$key" in
      HOST_IP) HOST_IP="$value" ;;
      PARENT_IFACE) PARENT_IFACE="$value" ;;
      INSTALL_ROOT) INSTALL_ROOT="$value" ;;
    esac
  done < "$env_file"
}

setup_save_deploy_env() {
  local deploy_dir host_ip parent_iface install_root
  deploy_dir="$(setup_deploy_dir)"
  host_ip="${1:-$(setup_detect_host_ip)}"
  parent_iface="${2:-$(setup_detect_parent_iface)}"
  install_root="${3:-$(setup_repo_root)}"
  cat >"$deploy_dir/deploy.env" <<EOF
# Written by nest-to-ONVIF setup scripts — safe to edit between runs.
HOST_IP="$host_ip"
PARENT_IFACE="$parent_iface"
INSTALL_ROOT="$install_root"
EOF
  setup_info "wrote $deploy_dir/deploy.env"
}

setup_run_privileged() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
    return
  fi
  if setup_has_command sudo; then
    sudo "$@"
    return
  fi
  setup_die "root or sudo required for: $*"
}

setup_skip_interface() {
  local name="$1"
  [[ "$name" == "lo" ]] && return 0
  case "$name" in
    docker*|veth*|br-*|onvif-*|tun*|tap*|wg*|utun*) return 0 ;;
  esac
  return 1
}

setup_list_lan_interfaces() {
  local iface
  while read -r iface; do
    setup_skip_interface "$iface" && continue
    ip link show "$iface" 2>/dev/null | grep -q 'state UP' || continue
    ip -4 -brief addr show dev "$iface" 2>/dev/null | awk '{print $3}' | while read -r cidr; do
      local ip="${cidr%/*}"
      [[ -z "$ip" || "$ip" == 127.* || "$ip" == 169.254.* ]] && continue
      printf '%s %s\n' "$iface" "$ip"
    done
  done < <(ls /sys/class/net)
}

setup_patch_compose_host_ip() {
  local deploy_dir host_ip compose
  deploy_dir="$(setup_deploy_dir)"
  host_ip="${1:-$(setup_detect_host_ip)}"
  compose="$deploy_dir/docker-compose.yml"
  [[ -f "$compose" ]] || setup_die "missing $compose"
  python3 - "$compose" "$host_ip" <<'PY'
import re, sys
path, host_ip = sys.argv[1], sys.argv[2]
placeholder = "203.0.113.1"
text = open(path).read()
if placeholder not in text and f'"{host_ip}:8554' not in text:
    raise SystemExit("no host IP bindings found in docker-compose.yml")
text = text.replace(f'"{placeholder}:', f'"{host_ip}:')
required = [
    ("8554:8554", '      - "127.0.0.1:8554:8554"'),
    ("8888:8888", '      - "127.0.0.1:8888:8888"'),
    ("8080:80", '      - "127.0.0.1:8080:80"'),
]
for host_port, loopback_line in required:
    loopback = f'"127.0.0.1:{host_port}"'
    if loopback in text:
        continue
    host_binding = f'"{host_ip}:{host_port}"'
    if text.count(host_binding) >= 2:
        first = text.find(host_binding)
        second = text.find(host_binding, first + len(host_binding))
        text = text[:second] + loopback + text[second + len(host_binding):]
        continue
    if host_binding in text:
        text = text.replace(host_binding + "\n", host_binding + "\n" + loopback_line + "\n", 1)
for _, loopback_line in required:
    loopback = loopback_line.split('"')[1]
    if loopback not in text:
        raise SystemExit(f"missing required loopback binding {loopback}")
open(path, "w").write(text)
PY
  setup_info "docker-compose.yml host bindings -> $host_ip"
}
