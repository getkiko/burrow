#!/usr/bin/env bash
#
# burrow-signal: installer and management menu for the Burrow signaling server
# on Linux (systemd).
#
# Install:
#   curl -fsSL https://raw.githubusercontent.com/getkiko/burrow/main/signal-deploy/install.sh | sudo bash
#
# With a terminal the script opens a menu; after installation the same menu is
# available as `burrow-signal-menu`. Running install again upgrades: the latest
# release tag is looked up and the download is skipped if it is already
# installed.
#
# Non-interactive install (no prompts, defaults or environment values):
#   curl -fsSL .../install.sh | sudo ASSUME_YES=1 BURROW_SIGNAL_PREFIX=/signal bash
#
# Environment variables (all optional):
#   BURROW_SIGNAL_LISTEN   listen addresses        (default :9960,:9962,:9963,:10002)
#   BURROW_SIGNAL_STAT     metrics endpoint        (default 127.0.0.1:9961, empty disables)
#   BURROW_SIGNAL_PREFIX   entry path prefix       (default empty: root path)
#   BURROW_SIGNAL_SECRET   probe token HMAC secret (default empty: random per start)
#   BURROW_SIGNAL_VERSION  pin a release tag       (default empty: follow latest)
#   ASSUME_YES=1           no prompts
#   FORCE=1                re-download even if the version is already installed
set -euo pipefail

REPO="getkiko/burrow"
ASSET_PREFIX="burrow-signal-linux"

# ------------------------------------------------------------------ defaults
# Overridable by environment variables or interactive input; an existing
# installation keeps its configuration by default.
DEF_LISTEN=":9960,:9962,:9963,:10002"
DEF_STAT="127.0.0.1:9961"
DEF_PREFIX=""
DEF_SECRET=""

BIN_PATH="/usr/local/bin/burrow-signal"
CTL_PATH="/usr/local/bin/burrow-signal-menu"
CONF_DIR="/etc/burrow-signal"
CONF_FILE="${CONF_DIR}/burrow-signal.env"
VERSION_FILE="${CONF_DIR}/version"
SERVICE_NAME="burrow-signal"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
FW_MARK="burrow-signal"
# Service account name. With DynamicUser systemd allocates it on the fly;
# on systemd older than 235 the installer creates it as a system user. An
# account created by the installer carries SERVICE_USER_GECOS, so uninstall
# only ever removes its own account.
SERVICE_USER="burrow-signal"
SERVICE_USER_GECOS="burrow-signal service account (created by install.sh)"

# ------------------------------------------------------------------ output
if [[ -t 1 ]]; then
  C_R=$'\033[31m'; C_G=$'\033[32m'; C_Y=$'\033[33m'; C_B=$'\033[36m'; C_0=$'\033[0m'
else
  C_R=""; C_G=""; C_Y=""; C_B=""; C_0=""
fi
log()  { echo "${C_B}==>${C_0} $*"; }
ok()   { echo "    ${C_G}ok${C_0} $*"; }
warn() { echo "    ${C_Y}!${C_0} $*"; }
info() { echo "    $*"; }

die()  { echo "${C_R}!!!${C_0} $*" >&2; exit 1; }

need_root() {
  [[ "$(id -u)" == "0" ]] || die "root privileges are required; run with sudo"
}

# ask reads one line from the terminal. It must read /dev/tty, not stdin: with
# `curl | bash`, stdin is the script itself and would return EOF immediately.
ask() {
  local prompt="$1" ans=""
  if [[ -e /dev/tty ]]; then
    read -r -p "${prompt}" ans < /dev/tty || true
  fi
  echo "${ans}"
}

# ask_silent waits for Enter and discards the input.
ask_silent() {
  [[ -e /dev/tty ]] || return 0
  read -r -p "$1" _ < /dev/tty || true
}

# ------------------------------------------------------------------ environment
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "unsupported architecture: $(uname -m) (only x86_64 and aarch64 are supported)" ;;
  esac
}

have() { command -v "$1" >/dev/null 2>&1; }

asset_url() {
  echo "https://github.com/${REPO}/releases/download/$1/$2"
}

download() {
  local url="$1" out="$2"
  if have curl; then
    curl -fsSL --connect-timeout 15 --retry 3 -o "${out}" "${url}"
  elif have wget; then
    wget -q --timeout=15 --tries=3 -O "${out}" "${url}"
  else
    die "curl or wget is required"
  fi
}

# fetch prints the body of a URL, or nothing on failure.
fetch() {
  if have curl; then
    curl -fsSL --connect-timeout 15 "$1" 2>/dev/null || true
  elif have wget; then
    wget -qO- --timeout=15 "$1" 2>/dev/null || true
  fi
}

# asset_exists checks with a HEAD request that a tag has the asset for this
# architecture, so a bad version fails before any configuration is asked for.
asset_exists() {
  local tag="$1" arch="$2" url code
  url="$(asset_url "${tag}" "${ASSET_PREFIX}-${arch}")"
  if have curl; then
    code="$(curl -sIL -o /dev/null -w '%{http_code}' --connect-timeout 20 "${url}" 2>/dev/null || echo 000)"
    [[ "${code}" == "200" ]]
  elif have wget; then
    wget -q --spider --timeout=20 "${url}" 2>/dev/null
  else
    # Without a download tool skip the check; download reports the error.
    return 0
  fi
}

# list_tags prints up to 10 release tags, shown when a version is wrong.
list_tags() {
  fetch "https://api.github.com/repos/${REPO}/releases" | grep '"tag_name"' \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' | head -10
}

# latest_tag prints the tag of the latest release. The GitHub API can read a
# public repository without a token; if it fails (for example rate limiting),
# fall back to the /releases/latest redirect.
latest_tag() {
  local tag
  tag="$(fetch "https://api.github.com/repos/${REPO}/releases/latest" | grep -m1 '"tag_name"' \
    | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' || true)"
  if [[ -z "${tag}" ]] && have curl; then
    tag="$(curl -fsSIL -o /dev/null -w '%{url_effective}' --connect-timeout 15 \
      "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
    if [[ "${tag}" == */releases/tag/* ]]; then
      tag="${tag##*/}"
    else
      tag=""
    fi
  fi
  echo "${tag}"
}

sha256_of() {
  if have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# verify_checksum checks a downloaded asset against the release's SHA256SUMS.
# A missing SHA256SUMS only warns; a mismatch is fatal.
verify_checksum() {
  local tag="$1" name="$2" file="$3" sums expected actual
  sums="$(fetch "$(asset_url "${tag}" SHA256SUMS)")"
  if [[ -z "${sums}" ]]; then
    warn "release ${tag} has no SHA256SUMS; checksum not verified"
    return 0
  fi
  expected="$(echo "${sums}" | awk -v n="${name}" '$2 == n || $2 == "*" n {print $1; exit}')"
  if [[ -z "${expected}" ]]; then
    warn "SHA256SUMS has no entry for ${name}; checksum not verified"
    return 0
  fi
  actual="$(sha256_of "${file}")"
  if [[ -z "${actual}" ]]; then
    warn "sha256sum not found; checksum not verified"
    return 0
  fi
  [[ "${actual}" == "${expected}" ]] || return 1
  ok "checksum verified (sha256 ${actual:0:16}...)"
}

# ------------------------------------------------------------------ configuration
load_conf() {
  [[ -f "${CONF_FILE}" ]] || return 0
  # shellcheck disable=SC1090
  set -a; source "${CONF_FILE}"; set +a
}

# save_conf rewrites the whole file every time; appending would stack duplicate
# variables. Values are always quoted because load_conf sources the file, and an
# unquoted value containing spaces would be executed as a command.
save_conf() {
  mkdir -p "${CONF_DIR}"
  cat > "${CONF_FILE}" <<EOF
# burrow-signal configuration (generated by install.sh).
# Change it through the menu ("Change configuration"), which also updates the
# firewall and rewrites the systemd unit. Manual edits only take effect after
# running "Change configuration" or reinstalling.
BURROW_SIGNAL_LISTEN="${BURROW_SIGNAL_LISTEN}"
BURROW_SIGNAL_STAT="${BURROW_SIGNAL_STAT}"
BURROW_SIGNAL_PREFIX="${BURROW_SIGNAL_PREFIX}"
BURROW_SIGNAL_SECRET="${BURROW_SIGNAL_SECRET}"
# Pinned version. When set, install and update use this tag and never move to
# the latest release; empty follows the latest release.
BURROW_SIGNAL_VERSION="${BURROW_SIGNAL_VERSION:-}"
EOF
  chmod 600 "${CONF_FILE}"
}

# listen_ports extracts the port list from a listen string, skipping loopback
# addresses, which need no firewall rule.
listen_ports() {
  local addr port host
  echo "${1}" | tr ',' '\n' | while read -r addr; do
    addr="$(echo "${addr}" | tr -d '[:space:]')"
    [[ -n "${addr}" ]] || continue
    port="${addr##*:}"
    host="${addr%:*}"
    [[ "${port}" =~ ^[0-9]+$ ]] || continue
    case "${host}" in
      127.0.0.1|localhost|"[::1]") continue ;;
    esac
    echo "${port}"
  done | sort -un
}

# ------------------------------------------------------------------ firewall
# Detects firewalld / ufw / nftables / iptables and opens TCP ports. Every rule
# is tagged with FW_MARK so uninstall can remove exactly what was added.

fw_backend() {
  if have firewall-cmd && systemctl is-active --quiet firewalld 2>/dev/null; then
    echo "firewalld"
  elif have ufw && ufw status 2>/dev/null | grep -qi "^Status: active"; then
    echo "ufw"
  elif have nft; then
    echo "nftables"
  elif have iptables; then
    echo "iptables"
  else
    echo "none"
  fi
}

fw_open() {
  local ports="$1" backend port
  backend="$(fw_backend)"
  [[ -n "${ports}" ]] || { warn "no ports to open"; return 0; }

  case "${backend}" in
    firewalld)
      for port in ${ports}; do
        firewall-cmd --permanent --add-port="${port}/tcp" >/dev/null 2>&1 || true
      done
      firewall-cmd --reload >/dev/null 2>&1 || true
      ok "firewalld opened: $(echo "${ports}" | tr '\n' ' ')"
      ;;
    ufw)
      for port in ${ports}; do
        ufw allow "${port}/tcp" comment "${FW_MARK}" >/dev/null 2>&1 || true
      done
      ok "ufw opened: $(echo "${ports}" | tr '\n' ' ')"
      ;;
    nftables)
      # Make sure the inet filter table and input chain exist (they may be
      # missing on a plain nft system).
      nft list table inet filter >/dev/null 2>&1 || nft add table inet filter
      nft list chain inet filter input >/dev/null 2>&1 || \
        nft 'add chain inet filter input { type filter hook input priority 0; policy accept; }'
      for port in ${ports}; do
        # Idempotent: skip ports that already have a tagged rule.
        if nft -a list chain inet filter input 2>/dev/null | grep -q "dport ${port} .*${FW_MARK}"; then
          continue
        fi
        nft add rule inet filter input tcp dport "${port}" accept comment "\"${FW_MARK}\"" 2>/dev/null || true
      done
      fw_persist_nft
      ok "nftables opened: $(echo "${ports}" | tr '\n' ' ')"
      ;;
    iptables)
      for port in ${ports}; do
        # -C checks whether the rule exists to avoid duplicates.
        if ! iptables -C INPUT -p tcp --dport "${port}" -m comment --comment "${FW_MARK}" -j ACCEPT 2>/dev/null; then
          iptables -I INPUT -p tcp --dport "${port}" -m comment --comment "${FW_MARK}" -j ACCEPT 2>/dev/null || \
            iptables -I INPUT -p tcp --dport "${port}" -j ACCEPT 2>/dev/null || true
        fi
      done
      fw_persist_iptables
      ok "iptables opened: $(echo "${ports}" | tr '\n' ' ')"
      ;;
    none)
      warn "no firewall tool found, skipping (open the ports in your cloud security group if you have one)"
      ;;
  esac
}

# fw_persist_nft saves the nftables ruleset to whichever distribution path
# exists.
fw_persist_nft() {
  local f
  for f in /etc/nftables.conf /etc/sysconfig/nftables.conf; do
    if [[ -f "${f}" ]]; then
      nft list ruleset > "${f}" 2>/dev/null && return 0
    fi
  done
  # Do not create a file if neither exists, to avoid fighting the
  # distribution's own mechanism.
  warn "nftables rules not persisted and may be lost on reboot (run: nft list ruleset > /etc/nftables.conf)"
}

fw_persist_iptables() {
  if have netfilter-persistent; then
    netfilter-persistent save >/dev/null 2>&1 && return 0
  fi
  if have iptables-save; then
    if [[ -d /etc/iptables ]]; then
      iptables-save > /etc/iptables/rules.v4 2>/dev/null && return 0
    elif [[ -f /etc/sysconfig/iptables ]]; then
      iptables-save > /etc/sysconfig/iptables 2>/dev/null && return 0
    fi
  fi
  warn "iptables rules not persisted and may be lost on reboot (consider installing iptables-persistent)"
}

fw_close() {
  local ports="$1" backend port
  backend="$(fw_backend)"
  case "${backend}" in
    firewalld)
      for port in ${ports}; do
        firewall-cmd --permanent --remove-port="${port}/tcp" >/dev/null 2>&1 || true
      done
      firewall-cmd --reload >/dev/null 2>&1 || true
      ;;
    ufw)
      for port in ${ports}; do ufw delete allow "${port}/tcp" >/dev/null 2>&1 || true; done
      ;;
    nftables)
      # Delete tagged rules one by one by handle.
      local handle
      nft -a list chain inet filter input 2>/dev/null | grep "${FW_MARK}" | \
        grep -oE 'handle [0-9]+' | awk '{print $2}' | sort -rn | while read -r handle; do
          nft delete rule inet filter input handle "${handle}" 2>/dev/null || true
        done
      fw_persist_nft
      ;;
    iptables)
      for port in ${ports}; do
        while iptables -C INPUT -p tcp --dport "${port}" -m comment --comment "${FW_MARK}" -j ACCEPT 2>/dev/null; do
          iptables -D INPUT -p tcp --dport "${port}" -m comment --comment "${FW_MARK}" -j ACCEPT 2>/dev/null || break
        done
      done
      fw_persist_iptables
      ;;
  esac
  ok "firewall rules removed"
}

# ------------------------------------------------------------------ systemd
# Arguments are written directly into ExecStart instead of using systemd ${VAR}
# expansion: variables from EnvironmentFile are not word-split in ExecStart, so
# "-stat X -prefix Y" would reach the program as a single argument. Changing the
# configuration through the menu rewrites this unit. The secret still comes
# from EnvironmentFile so it never shows up in `systemctl show` or `ps`.
#
# The service never runs as root. With systemd 235 or newer it runs as a
# transient user (DynamicUser). Older systemd silently ignores DynamicUser and
# would start the service as root, so there a dedicated system account is
# created and named with User= instead. Either way it reads no files at
# runtime: the EnvironmentFile is read by systemd itself (as root) before
# privileges are dropped, so it stays root-only (0600), and the binary is
# world-executable. Nothing is written, so no StateDirectory is needed.
#
# Version-dependent settings:
#   - DynamicUser needs 235 (see above).
#   - SystemCallFilter=@system-service needs 239. Older versions skip the
#     unknown group but still apply the allow-list, leaving only @default,
#     and the Go runtime is killed with SIGSYS at start. The line is
#     therefore only written on 239 or newer.
#   - AmbientCapabilities needs 229; without it ports below 1024 cannot be
#     bound, so the installer warns when such a port is configured.
# Other hardening options are merely skipped (with a log warning) by a
# systemd that does not know them, which only weakens the sandbox.

# systemd_version prints the running systemd's major version, or 0.
systemd_version() {
  local v
  v="$(systemctl --version 2>/dev/null | awk 'NR==1 {print $2}')"
  v="${v%%[!0-9]*}"
  echo "${v:-0}"
}

# ensure_service_user makes sure an existing SERVICE_USER account is safe to
# run the service as, and creates one when create is "yes" and none exists.
# An account the installer created (marked by SERVICE_USER_GECOS) is reused.
# Any other account of that name is only accepted if it is a system account
# without a login shell; an ordinary account is refused, because systemd
# would run the service as it (DynamicUser also reuses an existing account
# of the same name).
ensure_service_user() {
  local create="$1" entry uid shell gecos uid_min
  entry="$(getent passwd "${SERVICE_USER}" 2>/dev/null || true)"
  if [[ -z "${entry}" ]]; then
    [[ "${create}" == "yes" ]] || return 0
    local nologin
    nologin="$(command -v nologin 2>/dev/null || echo /sbin/nologin)"
    useradd --system --user-group --no-create-home --home-dir / \
      --shell "${nologin}" --comment "${SERVICE_USER_GECOS}" "${SERVICE_USER}" >&2 \
      || die "failed to create the system user ${SERVICE_USER}; refusing to run the service as root"
    return 0
  fi
  IFS=: read -r _ _ uid _ gecos _ shell <<< "${entry}"
  [[ "${gecos}" == "${SERVICE_USER_GECOS}" ]] && return 0
  uid_min="$(awk '$1 == "UID_MIN" {print $2}' /etc/login.defs 2>/dev/null)"
  uid_min="${uid_min:-1000}"
  if [[ ! "${uid}" =~ ^[0-9]+$ ]] || (( uid == 0 || uid >= uid_min )); then
    die "an account named ${SERVICE_USER} already exists and is not a system account (uid ${uid}); remove or rename it first"
  fi
  case "${shell}" in
    */nologin|*/false) ;;
    *) die "an account named ${SERVICE_USER} already exists with login shell ${shell}; remove it or set its shell to nologin first" ;;
  esac
  getent group "${SERVICE_USER}" >/dev/null 2>&1 \
    || die "the existing account ${SERVICE_USER} has no group of the same name; create it or remove the account first"
  warn "reusing the existing system account ${SERVICE_USER}" >&2
}

# service_user_lines prints the unit lines that select the unprivileged
# account for systemd version $1, creating the fallback system user on old
# systemd.
service_user_lines() {
  local ver="$1"
  if (( ver >= 235 )); then
    ensure_service_user no
    echo "DynamicUser=yes"
    return 0
  fi
  warn "systemd ${ver} is older than 235 and does not support DynamicUser; using the system user ${SERVICE_USER}" >&2
  warn "some hardening options in the unit are not supported by this systemd version and are skipped" >&2
  ensure_service_user yes
  echo "User=${SERVICE_USER}"
  echo "Group=${SERVICE_USER}"
}

# syscall_filter_lines prints the system call filter for systemd version $1;
# see the version notes above for why it is omitted before 239.
syscall_filter_lines() {
  local ver="$1"
  if (( ver >= 239 )); then
    echo "SystemCallFilter=@system-service"
  else
    echo "# SystemCallFilter omitted: @system-service needs systemd 239 or newer."
  fi
}

# warn_privileged_ports warns when systemd version $1 cannot grant
# CAP_NET_BIND_SERVICE (AmbientCapabilities needs 229) and a configured
# address uses a port below 1024.
warn_privileged_ports() {
  local ver="$1" addr port
  (( ver >= 229 )) && return 0
  local listen="${BURROW_SIGNAL_LISTEN:-}"
  for addr in ${listen//,/ } ${BURROW_SIGNAL_STAT:-}; do
    port="${addr##*:}"
    if [[ "${port}" =~ ^[0-9]+$ ]] && (( port < 1024 )); then
      warn "systemd ${ver} does not support AmbientCapabilities (needs 229); the unprivileged service cannot bind port ${port}. Use ports 1024 or above." >&2
    fi
  done
}

write_service() {
  local EXTRA_ARGS USER_LINES SYSCALL_LINES ver
  EXTRA_ARGS="$(build_extra_args)"
  ver="$(systemd_version)"
  USER_LINES="$(service_user_lines "${ver}")" || exit 1
  SYSCALL_LINES="$(syscall_filter_lines "${ver}")"
  warn_privileged_ports "${ver}"
  cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=Burrow signaling server
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target
# Crash-loop guard: stop after 10 restarts within 5 minutes.
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple
EnvironmentFile=${CONF_FILE}
ExecStart=${BIN_PATH} -listen ${BURROW_SIGNAL_LISTEN}${EXTRA_ARGS:+ ${EXTRA_ARGS}}
Restart=always
RestartSec=3
LimitNOFILE=1048576

# Run as an unprivileged user. The only capability kept is the one needed to
# bind ports below 1024, should the configuration use them.
${USER_LINES}
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

# Hardening: the service only needs the network, never a writable filesystem.
NoNewPrivileges=yes
UMask=0077
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
ProtectProc=invisible
ProtectClock=yes
ProtectHostname=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RemoveIPC=yes
SystemCallArchitectures=native
${SYSCALL_LINES}

StandardOutput=journal
StandardError=journal
SyslogIdentifier=burrow-signal

[Install]
WantedBy=multi-user.target
EOF
  chmod 644 "${SERVICE_FILE}"
  systemctl daemon-reload
}

# build_extra_args omits -stat / -prefix when empty; passing an empty string
# would be taken as a real value.
build_extra_args() {
  local args=""
  [[ -n "${BURROW_SIGNAL_STAT}" ]]   && args="${args} -stat ${BURROW_SIGNAL_STAT}"
  [[ -n "${BURROW_SIGNAL_PREFIX}" ]] && args="${args} -prefix ${BURROW_SIGNAL_PREFIX}"
  echo "${args# }"
}

# ------------------------------------------------------------------ install
cmd_install() {
  need_root

  local arch tag cur_ver asset interactive=1
  arch="$(detect_arch)"
  asset="${ASSET_PREFIX}-${arch}"

  # Interactive whenever a terminal exists (including `curl | bash`, because
  # prompts read /dev/tty). Only ASSUME_YES=1 or no terminal (cron/CI) uses
  # defaults throughout.
  if [[ "${ASSUME_YES:-0}" == "1" ]] || ! [[ -e /dev/tty ]]; then
    interactive=0
  fi

  log "Installing burrow-signal"
  info "architecture: linux-${arch}"

  # load_conf sources the file and would overwrite values passed in the
  # environment. Save them first and restore them afterwards so that
  # environment > existing configuration.
  local cli_listen="${BURROW_SIGNAL_LISTEN:-}" cli_stat="${BURROW_SIGNAL_STAT:-}"
  local cli_prefix="${BURROW_SIGNAL_PREFIX:-}" cli_secret="${BURROW_SIGNAL_SECRET:-}"
  local cli_version="${BURROW_SIGNAL_VERSION:-}"

  # An existing installation provides the defaults.
  local is_upgrade=0
  if [[ -f "${CONF_FILE}" ]]; then
    is_upgrade=1
    load_conf
    info "existing installation found; upgrading and keeping its configuration"
  fi

  [[ -n "${cli_listen}" ]]  && BURROW_SIGNAL_LISTEN="${cli_listen}"
  [[ -n "${cli_stat}" ]]    && BURROW_SIGNAL_STAT="${cli_stat}"
  [[ -n "${cli_prefix}" ]]  && BURROW_SIGNAL_PREFIX="${cli_prefix}"
  [[ -n "${cli_secret}" ]]  && BURROW_SIGNAL_SECRET="${cli_secret}"
  [[ -n "${cli_version}" ]] && BURROW_SIGNAL_VERSION="${cli_version}"
  BURROW_SIGNAL_VERSION="${BURROW_SIGNAL_VERSION:-}"

  # Precedence: environment > existing configuration > built-in default.
  BURROW_SIGNAL_LISTEN="${BURROW_SIGNAL_LISTEN:-${DEF_LISTEN}}"
  BURROW_SIGNAL_STAT="${BURROW_SIGNAL_STAT:-${DEF_STAT}}"
  BURROW_SIGNAL_PREFIX="${BURROW_SIGNAL_PREFIX:-${DEF_PREFIX}}"
  BURROW_SIGNAL_SECRET="${BURROW_SIGNAL_SECRET:-${DEF_SECRET}}"

  # Resolve and validate the version before asking for configuration, so a
  # missing version fails immediately instead of after all the prompts.
  if [[ -n "${BURROW_SIGNAL_VERSION}" ]]; then
    tag="${BURROW_SIGNAL_VERSION}"
    # Accept 1.0.0 as well as v1.0.0.
    [[ "${tag}" == v* ]] || tag="v${tag}"
    log "Using pinned version ${tag}"
    if ! asset_exists "${tag}" "${arch}"; then
      echo ""
      info "This version does not exist or has no ${asset} asset. Available versions:"
      list_tags | sed 's/^/      /'
      die "version ${tag} is not available (full list: https://github.com/${REPO}/releases)"
    fi
    ok "version ${tag} is available"
  else
    log "Looking up the latest release"
    tag="$(latest_tag)"
    [[ -n "${tag}" ]] || die "cannot determine the latest release of ${REPO} (check the network)"
    info "latest: ${tag}"
  fi

  if [[ "${interactive}" == "1" ]]; then
    echo ""
    echo "  ${C_B}Configuration${C_0} (press Enter to keep the default)"
    local ans
    # Values given explicitly in the environment are not asked again.
    if [[ -z "${cli_listen}" ]]; then
      ans="$(ask "  Listen addresses [${BURROW_SIGNAL_LISTEN}]: ")"
      [[ -n "${ans}" ]] && BURROW_SIGNAL_LISTEN="${ans}"
    fi
    if [[ -z "${cli_stat}" ]]; then
      ans="$(ask "  Metrics endpoint [${BURROW_SIGNAL_STAT:-<disabled>}]: ")"
      [[ -n "${ans}" ]] && BURROW_SIGNAL_STAT="${ans}"
    fi
    if [[ -z "${cli_prefix}" ]]; then
      ans="$(ask "  Path prefix [${BURROW_SIGNAL_PREFIX:-<root>}]: ")"
      [[ -n "${ans}" ]] && BURROW_SIGNAL_PREFIX="${ans}"
    fi
    if [[ -z "${cli_secret}" ]]; then
      ans="$(ask "  Signal secret [${BURROW_SIGNAL_SECRET:+set}${BURROW_SIGNAL_SECRET:-not set}]: ")"
      [[ -n "${ans}" ]] && BURROW_SIGNAL_SECRET="${ans}"
    fi
    echo ""
  fi

  # ---- download the binary (the version was validated above)
  cur_ver=""
  [[ -f "${VERSION_FILE}" ]] && cur_ver="$(cat "${VERSION_FILE}" 2>/dev/null || true)"

  if [[ "${cur_ver}" == "${tag}" && -x "${BIN_PATH}" && "${FORCE:-0}" != "1" ]]; then
    ok "already at ${tag}, skipping download (FORCE=1 to reinstall)"
  else
    log "Downloading ${asset} (${tag})"
    # The temporary file lives next to the target so the final mv is an
    # atomic rename, not a cross-device copy.
    local tmp
    tmp="$(mktemp "$(dirname "${BIN_PATH}")/.burrow-signal.XXXXXX")" || die "cannot create a temporary file"
    if ! download "$(asset_url "${tag}" "${asset}")" "${tmp}"; then
      rm -f "${tmp}"
      die "download failed (check that ${tag} exists: https://github.com/${REPO}/releases)"
    fi
    # A very loose lower bound that only catches obviously broken downloads
    # such as an error page or an empty file.
    if [[ "$(stat -c %s "${tmp}" 2>/dev/null || echo 0)" -lt 1000000 ]]; then
      rm -f "${tmp}"
      die "the downloaded file is too small; the release asset may be missing"
    fi
    if ! verify_checksum "${tag}" "${asset}" "${tmp}"; then
      rm -f "${tmp}"
      die "checksum mismatch for ${asset} (${tag}); refusing to install"
    fi
    # mktemp creates the file with mode 0600; the service runs as an
    # unprivileged dynamic user, so the binary must be world-executable.
    chmod 755 "${tmp}"
    # A running binary cannot be overwritten in place (ETXTBSY); mv swaps the
    # inode instead. Replacing only after a complete download means a failure
    # never breaks the existing binary.
    mv -f "${tmp}" "${BIN_PATH}" || { rm -f "${tmp}"; die "failed to replace the binary"; }
    mkdir -p "${CONF_DIR}"
    echo "${tag}" > "${VERSION_FILE}"
    ok "installed ${BIN_PATH} ($(du -h "${BIN_PATH}" | cut -f1))"
  fi

  # ---- configuration and service
  log "Writing configuration"
  save_conf
  info "config file: ${CONF_FILE}"
  info "listen:      ${BURROW_SIGNAL_LISTEN}"
  info "metrics:     ${BURROW_SIGNAL_STAT:-<disabled>}"
  info "prefix:      ${BURROW_SIGNAL_PREFIX:-<root>}"

  log "Configuring the systemd service"
  write_service
  ok "${SERVICE_FILE}"

  # ---- firewall
  log "Configuring the firewall ($(fw_backend))"
  local ports
  ports="$(listen_ports "${BURROW_SIGNAL_LISTEN}")"
  fw_open "${ports}"

  # ---- install this script locally so burrow-signal-menu opens the same menu
  if [[ -f "${BASH_SOURCE[0]}" && "${BASH_SOURCE[0]}" != "${CTL_PATH}" ]]; then
    cp -f "${BASH_SOURCE[0]}" "${CTL_PATH}" && chmod +x "${CTL_PATH}"
  else
    # When piped there is no script file; fetch a copy from the repository.
    if download "https://raw.githubusercontent.com/${REPO}/main/signal-deploy/install.sh" "${CTL_PATH}" 2>/dev/null; then
      chmod +x "${CTL_PATH}"
    else
      warn "could not install the menu script; rerun the curl command to get it"
    fi
  fi
  [[ -x "${CTL_PATH}" ]] && ok "run burrow-signal-menu to open this menu later"

  # ---- start
  log "Starting the service"
  systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1
  systemctl restart "${SERVICE_NAME}"
  sleep 2

  if systemctl is-active --quiet "${SERVICE_NAME}"; then
    ok "service is running"
  else
    echo ""
    journalctl -u "${SERVICE_NAME}" -n 20 --no-pager 2>/dev/null | sed 's/^/      /'
    die "the service failed to start; see the log above"
  fi

  # ---- verify
  log "Checking endpoints"
  verify_endpoints "${ports}"

  echo ""
  if [[ "${is_upgrade}" == "1" ]]; then
    log "${C_G}Upgrade complete${C_0}: ${tag}"
  else
    log "${C_G}Installation complete${C_0}: ${tag}"
  fi
  info "manage: run burrow-signal-menu"
  info "entry:  ws://<public-ip>:<port>${BURROW_SIGNAL_PREFIX}?command={signal|echo|nat}"
}

# verify_endpoints probes each port with curl: 204 is the health check and 101
# is a successful WebSocket handshake on the nat endpoint. After a 101 curl
# exits non-zero because the connection is not closed cleanly, but http_code
# is already printed, so "|| echo 000" would yield "101000"; take the last
# three characters instead.
verify_endpoints() {
  local ports="$1" port code prefix="${BURROW_SIGNAL_PREFIX:-}"
  if ! have curl; then warn "curl not found, skipping endpoint checks"; return 0; fi
  local ws=(-H "Connection: Upgrade" -H "Upgrade: websocket"
            -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==")
  local failed=0
  for port in ${ports}; do
    code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 \
      "http://127.0.0.1:${port}${prefix}?command=health" 2>/dev/null | tail -c 3 || true)
    local nat
    nat=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "${ws[@]}" \
      "http://127.0.0.1:${port}${prefix}?command=nat&room=r&session=s" 2>/dev/null | tail -c 3 || true)
    if [[ "${code:-000}" == "204" && "${nat:-000}" == "101" ]]; then
      ok ":${port}  health=204  nat=101"
    else
      warn ":${port}  health=${code:-000}  nat=${nat:-000}"
      failed=1
    fi
  done
  [[ "${failed}" == "0" ]] || warn 'some endpoints failed; check "View logs" in the menu'
}

# ------------------------------------------------------------------ management
cmd_status() {
  load_conf
  echo ""
  echo "  ${C_B}burrow-signal${C_0}"
  echo "  ------------------------------------"
  local ver="unknown"
  [[ -f "${VERSION_FILE}" ]] && ver="$(cat "${VERSION_FILE}")"
  printf "  %-10s %s\n" "version" "${ver}"
  printf "  %-10s %s\n" "binary" "${BIN_PATH}"
  printf "  %-10s %s\n" "listen" "${BURROW_SIGNAL_LISTEN:-<not configured>}"
  printf "  %-10s %s\n" "metrics" "${BURROW_SIGNAL_STAT:-<disabled>}"
  printf "  %-10s %s\n" "prefix" "${BURROW_SIGNAL_PREFIX:-<root>}"
  # Only show whether a secret is set, never the secret itself.
  if [[ -n "${BURROW_SIGNAL_SECRET:-}" ]]; then
    printf "  %-10s %s\n" "secret" "set"
  else
    printf "  %-10s %s\n" "secret" "not set"
  fi
  [[ -n "${BURROW_SIGNAL_VERSION:-}" ]] && printf "  %-10s %s\n" "pinned" "${BURROW_SIGNAL_VERSION}"

  local state
  state="$(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo inactive)"
  if [[ "${state}" == "active" ]]; then
    printf "  %-10s ${C_G}%s${C_0}\n" "state" "running"
  else
    printf "  %-10s ${C_R}%s${C_0}\n" "state" "${state}"
  fi
  printf "  %-10s %s\n" "on boot" "$(systemctl is-enabled "${SERVICE_NAME}" 2>/dev/null || echo disabled)"

  # Uptime and memory.
  local since mem pid
  pid="$(systemctl show -p MainPID --value "${SERVICE_NAME}" 2>/dev/null || echo 0)"
  if [[ "${pid}" != "0" && -n "${pid}" ]]; then
    since="$(systemctl show -p ActiveEnterTimestamp --value "${SERVICE_NAME}" 2>/dev/null || true)"
    mem="$(systemctl show -p MemoryCurrent --value "${SERVICE_NAME}" 2>/dev/null || true)"
    printf "  %-10s %s\n" "pid" "${pid}"
    [[ -n "${since}" ]] && printf "  %-10s %s\n" "since" "${since}"
    if [[ "${mem}" =~ ^[0-9]+$ ]]; then
      printf "  %-10s %s MB\n" "memory" "$((mem / 1024 / 1024))"
    fi
  fi

  echo "  ------------------------------------"
  echo "  ${C_B}Ports${C_0}"
  local ports port
  ports="$(listen_ports "${BURROW_SIGNAL_LISTEN:-}")"
  for port in ${ports}; do
    if have ss && ss -ltn 2>/dev/null | grep -q ":${port} "; then
      printf "  %-10s ${C_G}%s${C_0}\n" ":${port}" "listening"
    elif have netstat && netstat -ltn 2>/dev/null | grep -q ":${port} "; then
      printf "  %-10s ${C_G}%s${C_0}\n" ":${port}" "listening"
    else
      printf "  %-10s ${C_R}%s${C_0}\n" ":${port}" "not listening"
    fi
  done

  echo "  ------------------------------------"
  echo "  ${C_B}Firewall${C_0} ($(fw_backend))"
  fw_show "${ports}"
  echo ""
  # Probe the endpoints when the service is running.
  if [[ "${state}" == "active" ]]; then
    echo "  ${C_B}Health check${C_0}"
    verify_endpoints "${ports}"
    echo ""
  fi
}

fw_show() {
  local ports="$1" port backend
  backend="$(fw_backend)"
  for port in ${ports}; do
    local opened=1
    case "${backend}" in
      firewalld) firewall-cmd --query-port="${port}/tcp" >/dev/null 2>&1 || opened=0 ;;
      ufw)       ufw status 2>/dev/null | grep -q "^${port}/tcp" || opened=0 ;;
      nftables)  nft list chain inet filter input 2>/dev/null | grep -q "dport ${port} " || opened=0 ;;
      iptables)  iptables -C INPUT -p tcp --dport "${port}" -m comment --comment "${FW_MARK}" -j ACCEPT 2>/dev/null || opened=0 ;;
      none)      printf "  %-10s %s\n" ":${port}" "no firewall"; continue ;;
    esac
    if [[ "${opened}" == "1" ]]; then
      printf "  %-10s ${C_G}%s${C_0}\n" ":${port}" "open"
    else
      printf "  %-10s ${C_Y}%s${C_0}\n" ":${port}" 'not open (fix it under "Firewall" in the menu)'
    fi
  done
}

cmd_logs() {
  local follow=0 lines=100
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -f|--follow) follow=1; shift ;;
      -n) lines="$2"; shift 2 ;;
      *) shift ;;
    esac
  done
  if [[ "${follow}" == "1" ]]; then
    journalctl -u "${SERVICE_NAME}" -f --no-pager
  else
    journalctl -u "${SERVICE_NAME}" -n "${lines}" --no-pager
  fi
}

cmd_restart() {
  need_root; systemctl restart "${SERVICE_NAME}"; sleep 1
  if systemctl is-active --quiet "${SERVICE_NAME}"; then ok "restarted"; else die 'restart failed; see "View logs" in the menu'; fi
}
cmd_stop() { need_root; systemctl stop "${SERVICE_NAME}"; ok "stopped"; }
cmd_start() {
  need_root; systemctl start "${SERVICE_NAME}"; sleep 1
  if systemctl is-active --quiet "${SERVICE_NAME}"; then ok "started"; else die 'start failed; see "View logs" in the menu'; fi
}

cmd_config() {
  need_root
  [[ -f "${CONF_FILE}" ]] || die "burrow-signal is not installed"

  # Remember the current ports so rules for ports no longer used can be removed.
  load_conf
  local old_ports
  old_ports="$(listen_ports "${BURROW_SIGNAL_LISTEN:-}")"

  # Same form as install; Enter keeps the current value.
  echo ""
  echo "  ${C_B}Change configuration${C_0} (press Enter to keep the current value)"
  local ans
  ans="$(ask "  Listen addresses [${BURROW_SIGNAL_LISTEN}]: ")"
  [[ -n "${ans}" ]] && BURROW_SIGNAL_LISTEN="${ans}"
  ans="$(ask "  Metrics endpoint [${BURROW_SIGNAL_STAT:-disabled}]: ")"
  [[ -n "${ans}" ]] && BURROW_SIGNAL_STAT="${ans}"
  ans="$(ask "  Path prefix [${BURROW_SIGNAL_PREFIX:-root}]: ")"
  [[ -n "${ans}" ]] && BURROW_SIGNAL_PREFIX="${ans}"
  ans="$(ask "  Signal secret [${BURROW_SIGNAL_SECRET:+set}${BURROW_SIGNAL_SECRET:-not set}]: ")"
  [[ -n "${ans}" ]] && BURROW_SIGNAL_SECRET="${ans}"
  # "latest" removes the pin and follows the latest release again.
  ans="$(ask "  Pinned version [${BURROW_SIGNAL_VERSION:-follow latest}] (enter latest to unpin): ")"
  if [[ -n "${ans}" ]]; then
    if [[ "${ans}" == "latest" || "${ans}" == "-" ]]; then
      BURROW_SIGNAL_VERSION=""
    else
      [[ "${ans}" == v* ]] || ans="v${ans}"
      # Validate now rather than failing at download time. Nothing has been
      # saved yet, so exiting leaves the configuration untouched.
      if ! asset_exists "${ans}" "$(detect_arch)"; then
        echo ""
        info "This version does not exist or has no asset for this architecture. Available versions:"
        list_tags | sed 's/^/      /'
        die "version ${ans} is not available; configuration unchanged"
      fi
      BURROW_SIGNAL_VERSION="${ans}"
    fi
  fi
  echo ""

  save_conf
  # Arguments live in ExecStart, so the unit must be rewritten for -stat and
  # -prefix changes to take effect.
  write_service

  log "Applying configuration"
  local new_ports port
  new_ports="$(listen_ports "${BURROW_SIGNAL_LISTEN}")"
  # Close ports removed from the listen list so no orphan rules remain.
  for port in ${old_ports}; do
    grep -qx "${port}" <<<"${new_ports}" || { fw_close "${port}"; info "closed port :${port}"; }
  done
  fw_open "${new_ports}"

  # A new pinned version needs a new binary, not just a restart.
  local cur_ver
  cur_ver="$(cat "${VERSION_FILE}" 2>/dev/null || echo "")"
  if [[ -n "${BURROW_SIGNAL_VERSION:-}" && "${BURROW_SIGNAL_VERSION}" != "${cur_ver}" ]]; then
    info "switching from ${cur_ver:-<none>} to ${BURROW_SIGNAL_VERSION}"
    # If the install fails, restore the previous pin; otherwise a bad version
    # would stay in the configuration.
    local want="${BURROW_SIGNAL_VERSION}"
    if ! ( ASSUME_YES=1 cmd_install ); then
      BURROW_SIGNAL_VERSION="${cur_ver}"
      save_conf
      die "switching to ${want} failed; restored the previous configuration (${cur_ver:-unpinned})"
    fi
    return 0
  fi

  systemctl restart "${SERVICE_NAME}"
  sleep 1
  if systemctl is-active --quiet "${SERVICE_NAME}"; then
    ok "applied"
  else
    die 'restart failed; see "View logs" in the menu'
  fi
}

cmd_firewall() {
  need_root
  load_conf
  log "Reapplying firewall rules ($(fw_backend))"
  fw_open "$(listen_ports "${BURROW_SIGNAL_LISTEN:-${DEF_LISTEN}}")"
}

cmd_update() {
  need_root
  load_conf
  local tag cur
  cur="$(cat "${VERSION_FILE}" 2>/dev/null || echo "")"

  # With a pinned version, do not look up the latest release; updating would
  # silently override the pin.
  if [[ -n "${BURROW_SIGNAL_VERSION:-}" ]]; then
    warn "version is pinned to ${BURROW_SIGNAL_VERSION}; not upgrading"
    info 'to unpin: "Change configuration" in the menu, enter latest as the pinned version'
    return 0
  fi

  tag="$(latest_tag)"
  [[ -n "${tag}" ]] || die "cannot determine the latest release"
  if [[ "${cur}" == "${tag}" && "${FORCE:-0}" != "1" ]]; then
    ok "already at the latest version ${tag}"
    return 0
  fi
  info "${cur:-<none>} -> ${tag}"
  ASSUME_YES=1 cmd_install
}

cmd_uninstall() {
  need_root
  local ans="y"
  if [[ "${ASSUME_YES:-0}" != "1" ]]; then
    ans="$(ask 'Uninstall burrow-signal? Configuration and firewall rules are removed too [y/N]: ')"
  fi
  [[ "${ans}" =~ ^[Yy]$ ]] || { info "cancelled"; return 0; }

  load_conf
  log "Stopping the service"
  systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
  systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
  rm -f "${SERVICE_FILE}"
  systemctl daemon-reload

  log "Removing firewall rules"
  fw_close "$(listen_ports "${BURROW_SIGNAL_LISTEN:-${DEF_LISTEN}}")"

  log "Removing files"
  rm -f "${BIN_PATH}" "${CTL_PATH}"
  rm -rf "${CONF_DIR}"
  # Only the account the installer created (on systemd older than 235) is
  # removed; it is recognized by its GECOS marker. A pre-existing account of
  # the same name is left alone.
  local gecos
  gecos="$(getent passwd "${SERVICE_USER}" 2>/dev/null | cut -d: -f5)"
  if [[ "${gecos}" == "${SERVICE_USER_GECOS}" ]]; then
    userdel "${SERVICE_USER}" 2>/dev/null || warn "could not remove the system user ${SERVICE_USER}"
    if getent group "${SERVICE_USER}" >/dev/null 2>&1; then
      groupdel "${SERVICE_USER}" 2>/dev/null || true
    fi
  fi
  ok "uninstalled"
}

# ------------------------------------------------------------------ menu
# menu_header shows at a glance whether the server is installed and running.
menu_header() {
  local ver state
  echo ""
  echo "${C_B}--------------- burrow-signal management ---------------${C_0}"
  if [[ -x "${BIN_PATH}" ]]; then
    ver="$(cat "${VERSION_FILE}" 2>/dev/null || echo 'unknown')"
    state="$(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo inactive)"
    if [[ "${state}" == "active" ]]; then
      echo "burrow-signal ${ver}, ${C_G}running${C_0}"
    else
      echo "burrow-signal ${ver}, ${C_R}not running${C_0}"
    fi
  else
    echo "burrow-signal, ${C_Y}not installed${C_0}"
  fi
  echo ""
}

# menu_show offers only "Install" until the server is installed, so no option
# is bound to fail.
menu_show() {
  if [[ -x "${BIN_PATH}" ]]; then
    echo "  ${C_G}1)${C_0} Reinstall / upgrade"
    echo "  ${C_G}2)${C_0} Change configuration"
    echo "  ${C_G}3)${C_0} Show configuration and status"
    echo "  ${C_G}4)${C_0} Start / stop / restart"
    echo "  ${C_G}5)${C_0} View logs"
    echo "  ${C_G}6)${C_0} Update"
    echo "  ${C_G}7)${C_0} Uninstall"
    echo "  ${C_G}8)${C_0} Firewall"
    echo "  ${C_G}0)${C_0} Exit"
  else
    echo "  ${C_G}1)${C_0} Install"
    echo "  ${C_G}0)${C_0} Exit"
  fi
  echo ""
}

menu_run() {
  echo ""
  echo "  ${C_G}1)${C_0} Start"
  echo "  ${C_G}2)${C_0} Stop"
  echo "  ${C_G}3)${C_0} Restart"
  echo "  ${C_G}0)${C_0} Back"
  echo ""
  local c
  c="$(ask 'Choose [0-3]: ')"
  case "${c}" in
    1) cmd_start ;;
    2) cmd_stop ;;
    3) cmd_restart ;;
    0|"") return 0 ;;
    *) warn "invalid choice" ;;
  esac
}

# menu_logs offers following separately because it blocks until Ctrl-C.
menu_logs() {
  echo ""
  echo "  ${C_G}1)${C_0} Show the last 100 lines"
  echo "  ${C_G}2)${C_0} Follow (Ctrl-C to stop)"
  echo "  ${C_G}0)${C_0} Back"
  echo ""
  local c
  c="$(ask 'Choose [0-2]: ')"
  case "${c}" in
    1) cmd_logs -n 100 ;;
    # Following needs the terminal, and Ctrl-C should only stop journalctl,
    # not the whole script.
    2) journalctl -u "${SERVICE_NAME}" -f --no-pager < /dev/tty || true ;;
    0|"") return 0 ;;
    *) warn "invalid choice" ;;
  esac
}

menu_firewall() {
  load_conf
  local ports
  ports="$(listen_ports "${BURROW_SIGNAL_LISTEN:-${DEF_LISTEN}}")"
  echo ""
  echo "  backend: $(fw_backend)"
  fw_show "${ports}"
  echo ""
  echo "  ${C_G}1)${C_0} Open all ports again"
  echo "  ${C_G}2)${C_0} Remove the rules added by this script"
  echo "  ${C_G}0)${C_0} Back"
  echo ""
  local c
  c="$(ask 'Choose [0-2]: ')"
  case "${c}" in
    1) need_root; fw_open "${ports}" ;;
    2) need_root; fw_close "${ports}" ;;
    0|"") return 0 ;;
    *) warn "invalid choice" ;;
  esac
}

# menu_view_conf shows the configuration read-only, without the secret.
menu_view_conf() {
  load_conf
  echo ""
  echo "  ${C_B}Configuration${C_0} (${CONF_FILE})"
  printf "    %-18s %s\n" "listen addresses" "${BURROW_SIGNAL_LISTEN:-<not configured>}"
  printf "    %-18s %s\n" "metrics endpoint" "${BURROW_SIGNAL_STAT:-<disabled>}"
  printf "    %-18s %s\n" "path prefix" "${BURROW_SIGNAL_PREFIX:-<root>}"
  if [[ -n "${BURROW_SIGNAL_SECRET:-}" ]]; then
    printf "    %-18s %s\n" "signal secret" "set"
  else
    printf "    %-18s %s\n" "signal secret" "not set"
  fi
  printf "    %-18s %s\n" "pinned version" "${BURROW_SIGNAL_VERSION:-<follow latest>}"
  echo ""
  cmd_status
}

# run_action runs one menu action in a subshell. die calls exit 1, which would
# otherwise kill the whole menu and flash the error away; in a subshell only the
# action ends and the menu continues. Changes to globals are not carried back,
# which is fine because every cmd_* calls load_conf itself.
run_action() {
  local rc=0
  ( "$@" ) || rc=$?
  if [[ "${rc}" != "0" ]]; then
    echo ""
    warn "action did not complete (exit code ${rc})"
  fi
  return 0
}

# main loop. Pause after each action so the output can be read.
main() {
  # Without a terminal (cron/CI) or with ASSUME_YES=1, install directly.
  if [[ "${ASSUME_YES:-0}" == "1" ]] || ! [[ -e /dev/tty ]]; then
    cmd_install
    return $?
  fi

  while true; do
    menu_header
    menu_show
    local choice
    if [[ -x "${BIN_PATH}" ]]; then
      choice="$(ask 'Choose [0-8]: ')"
      case "${choice}" in
        1) run_action cmd_install ;;
        2) run_action cmd_config ;;
        3) run_action menu_view_conf ;;
        4) run_action menu_run ;;
        5) run_action menu_logs ;;
        6) run_action cmd_update ;;
        7) run_action cmd_uninstall ;;
        8) run_action menu_firewall ;;
        0) echo ""; exit 0 ;;
        "") ;;
        *) warn "invalid choice: ${choice}" ;;
      esac
    else
      choice="$(ask 'Choose [0-1]: ')"
      case "${choice}" in
        1) run_action cmd_install ;;
        0) echo ""; exit 0 ;;
        "") ;;
        *) warn "invalid choice: ${choice}" ;;
      esac
    fi
    echo ""
    ask_silent 'Press Enter to return to the menu...'
  done
}

main "$@"
