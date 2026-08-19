#!/usr/bin/env bash
#
# deploy.sh - build and deploy this CLIProxyAPI fork on Linux.
#
# Usage:
#   ./deploy/deploy.sh install       build the binary and install it (systemd when available)
#   ./deploy/deploy.sh update        rebuild from the current checkout and restart
#   ./deploy/deploy.sh start|stop|restart|status|logs
#   ./deploy/deploy.sh uninstall     remove the service and binary (config and auth data are kept)
#
# Environment overrides:
#   PREFIX        install root for the binary        (default: /opt/cliproxyapi, or ~/.local/share/cliproxyapi without root)
#   CONFIG_DIR    directory holding config.yaml      (default: /etc/cliproxyapi, or $PREFIX/etc without root)
#   DATA_DIR      auth + log storage                 (default: /var/lib/cliproxyapi, or $PREFIX/var without root)
#   PORT          listen port written into a new config (default: 8317)
#   HOST          listen address written into a new config (default: "" = all interfaces)
#   SERVICE_NAME  systemd unit name                  (default: cliproxyapi)
#   SERVICE_USER  system user to run as              (default: cliproxy when root, else the current user)
#   NO_SYSTEMD=1  install the binary and config only, skip the service

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_NAME="CLIProxyAPI"
MIN_GO_MINOR=21
GO_BOOTSTRAP_VERSION="1.26.0"

if [[ "$(id -u)" == "0" ]]; then
  IS_ROOT=1
  PREFIX="${PREFIX:-/opt/cliproxyapi}"
  CONFIG_DIR="${CONFIG_DIR:-/etc/cliproxyapi}"
  DATA_DIR="${DATA_DIR:-/var/lib/cliproxyapi}"
  SERVICE_USER="${SERVICE_USER:-cliproxy}"
else
  IS_ROOT=0
  PREFIX="${PREFIX:-$HOME/.local/share/cliproxyapi}"
  CONFIG_DIR="${CONFIG_DIR:-$PREFIX/etc}"
  DATA_DIR="${DATA_DIR:-$PREFIX/var}"
  SERVICE_USER="${SERVICE_USER:-$(id -un)}"
fi

PORT="${PORT:-8317}"
HOST="${HOST:-}"
SERVICE_NAME="${SERVICE_NAME:-cliproxyapi}"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
AUTH_DIR="$DATA_DIR/auths"
# The relay resolves file logs relative to its working directory, which the unit pins to DATA_DIR.
LOG_DIR="$DATA_DIR/logs"
INSTALLED_BIN="$PREFIX/bin/$BIN_NAME"

STAGING_DIR=""
cleanup() {
  [[ -n "$STAGING_DIR" ]] && rm -rf "$STAGING_DIR"
  return 0
}
trap cleanup EXIT

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror\033[0m %s\n' "$*" >&2; exit 1; }

# Root manages the system instance; everyone else gets a systemd user unit.
if [[ "$IS_ROOT" == "1" ]]; then
  SCOPE=()
else
  SCOPE=(--user)
fi

# Containers often ship systemctl without a reachable bus, so probe the manager
# itself rather than trusting that the binary exists.
have_systemd() {
  [[ "${NO_SYSTEMD:-0}" != "1" ]] || return 1
  command -v systemctl >/dev/null 2>&1 || return 1
  systemctl "${SCOPE[@]}" show --property=Version >/dev/null 2>&1
}

unit_path() {
  if [[ "$IS_ROOT" == "1" ]]; then
    printf '%s' "/etc/systemd/system/$SERVICE_NAME.service"
  else
    printf '%s' "$HOME/.config/systemd/user/$SERVICE_NAME.service"
  fi
}

random_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 24
  else
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# go.mod pins go 1.26.0. Any toolchain >= 1.21 can fetch that automatically via
# GOTOOLCHAIN=auto, so only bootstrap a full toolchain when nothing usable exists.
resolve_go() {
  if command -v go >/dev/null 2>&1; then
    local minor
    minor="$(go env GOVERSION | sed -n 's/^go1\.\([0-9]*\).*/\1/p')"
    if [[ -n "$minor" && "$minor" -ge "$MIN_GO_MINOR" ]]; then
      GO_BIN="$(command -v go)"
      return
    fi
    warn "found $(go env GOVERSION), which cannot auto-download the required toolchain"
  fi

  local cache="${XDG_CACHE_HOME:-$HOME/.cache}/cliproxyapi/go$GO_BOOTSTRAP_VERSION"
  if [[ ! -x "$cache/go/bin/go" ]]; then
    local arch
    case "$(uname -m)" in
      x86_64|amd64) arch=amd64 ;;
      aarch64|arm64) arch=arm64 ;;
      *) die "unsupported architecture $(uname -m); install Go >= 1.$MIN_GO_MINOR manually" ;;
    esac
    log "downloading Go $GO_BOOTSTRAP_VERSION into $cache"
    mkdir -p "$cache"
    curl -fsSL "https://go.dev/dl/go$GO_BOOTSTRAP_VERSION.linux-$arch.tar.gz" \
      | tar -C "$cache" -xz || die "failed to download the Go toolchain"
  fi
  GO_BIN="$cache/go/bin/go"
}

build_binary() {
  resolve_go
  local version commit build_date
  version="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
  commit="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo none)"
  build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  log "building $BIN_NAME (version=$version commit=$commit) with $("$GO_BIN" env GOVERSION)"
  STAGING_DIR="$(mktemp -d)"
  BUILD_OUTPUT="$STAGING_DIR/$BIN_NAME"
  (
    cd "$REPO_ROOT"
    GOTOOLCHAIN=auto CGO_ENABLED=1 GOOS=linux "$GO_BIN" build \
      -buildvcs=false \
      -ldflags="-s -w -X 'main.Version=$version' -X 'main.Commit=$commit' -X 'main.BuildDate=$build_date'" \
      -o "$BUILD_OUTPUT" ./cmd/server/
  )
}

write_default_config() {
  local api_key mgmt_key
  api_key="sk-cpa-$(random_secret)"
  mgmt_key="$(random_secret)"

  # The relay rewrites this file whenever credentials change and flattens comments
  # nested inside mappings, so keep all guidance in the top-level header block.
  cat > "$CONFIG_FILE" <<EOF
# Generated by deploy/deploy.sh. Edit freely: the service reloads this file on change.
#
# api-keys are the bearer tokens clients must send.
# remote-management.secret-key is hashed in place on first startup.
# remote-management.allow-remote should only be turned on behind a TLS reverse proxy.
#
# Add a Cursor credential with either:
#   $INSTALLED_BIN --config $CONFIG_FILE --cursor-api-key "crsr_..."
#   curl -X PUT http://127.0.0.1:$PORT/v0/management/cursor-api-key \\
#        -H "Authorization: Bearer <management key>" \\
#        -d '[{"api-key":"crsr_..."}]'
# See deploy/README.md for the full list of options.

host: "$HOST"
port: $PORT

auth-dir: "$AUTH_DIR"

api-keys:
  - "$api_key"

debug: false

remote-management:
  allow-remote: false
  secret-key: "$mgmt_key"
EOF
  chmod 600 "$CONFIG_FILE"

  log "wrote $CONFIG_FILE"
  printf '    client API key : %s\n' "$api_key"
  printf '    management key : %s\n' "$mgmt_key"
  warn "the management key is hashed on first start; save it now"
}

install_service() {
  local unit
  unit="$(unit_path)"
  mkdir -p "$(dirname "$unit")"

  local wanted_by="default.target"
  [[ "$IS_ROOT" == "1" ]] && wanted_by="multi-user.target"

  cat > "$unit" <<EOF
[Unit]
Description=CLIProxyAPI relay ($SERVICE_NAME)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EOF

  # A user unit already runs as the invoking user, so User=/Group= only apply to root installs.
  if [[ "$IS_ROOT" == "1" ]]; then
    printf 'User=%s\nGroup=%s\n' "$SERVICE_USER" "$SERVICE_USER" >> "$unit"
  fi

  cat >> "$unit" <<EOF
ExecStart=$INSTALLED_BIN --config $CONFIG_FILE
WorkingDirectory=$DATA_DIR
Restart=always
RestartSec=3
# The relay rewrites config.yaml when credentials change, so the config dir stays writable.
ReadWritePaths=$DATA_DIR $CONFIG_DIR
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
LimitNOFILE=65535

[Install]
WantedBy=$wanted_by
EOF

  log "installed unit $unit"
  systemctl "${SCOPE[@]}" daemon-reload
  systemctl "${SCOPE[@]}" enable "$SERVICE_NAME" >/dev/null
}

ensure_service_user() {
  [[ "$IS_ROOT" == "1" ]] || return 0
  id -u "$SERVICE_USER" >/dev/null 2>&1 && return 0
  log "creating system user $SERVICE_USER"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
}

cmd_install() {
  build_binary

  ensure_service_user
  mkdir -p "$PREFIX/bin" "$CONFIG_DIR" "$AUTH_DIR" "$LOG_DIR"
  install -m 755 "$BUILD_OUTPUT" "$INSTALLED_BIN"
  log "installed $INSTALLED_BIN"

  if [[ -f "$CONFIG_FILE" ]]; then
    log "keeping existing $CONFIG_FILE"
  else
    write_default_config
  fi

  if [[ "$IS_ROOT" == "1" ]]; then
    chown -R "$SERVICE_USER":"$SERVICE_USER" "$DATA_DIR"
    chown "$SERVICE_USER":"$SERVICE_USER" "$CONFIG_DIR" "$CONFIG_FILE"
  fi

  if have_systemd; then
    install_service
    cmd_restart
    cmd_status
  else
    warn "systemd not available; start the relay yourself:"
    printf '    %s --config %s\n' "$INSTALLED_BIN" "$CONFIG_FILE"
  fi
}

cmd_update() {
  build_binary
  install -m 755 "$BUILD_OUTPUT" "$INSTALLED_BIN"
  log "updated $INSTALLED_BIN"
  if have_systemd; then
    cmd_restart
    cmd_status
  fi
}

require_service() {
  have_systemd || die "systemd is not available; manage the process manually"
}

cmd_start()   { require_service; systemctl "${SCOPE[@]}" start "$SERVICE_NAME"; }
cmd_stop()    { require_service; systemctl "${SCOPE[@]}" stop "$SERVICE_NAME"; }
cmd_restart() { require_service; systemctl "${SCOPE[@]}" restart "$SERVICE_NAME"; }
cmd_logs()    { require_service; journalctl "${SCOPE[@]}" -u "$SERVICE_NAME" -f -n 100; }

cmd_status() {
  if have_systemd; then
    systemctl "${SCOPE[@]}" --no-pager --lines=0 status "$SERVICE_NAME" || true
  fi
  # An empty HOST means "bind everything", which is still reachable on loopback.
  local probe_host="${HOST:-127.0.0.1}"
  if command -v curl >/dev/null 2>&1; then
    log "probing http://$probe_host:$PORT/healthz"
    if curl -fsS --max-time 5 "http://$probe_host:$PORT/healthz" >/dev/null 2>&1; then
      log "relay is answering on port $PORT"
    else
      warn "no response on port $PORT yet (check the logs)"
    fi
  fi
}

cmd_uninstall() {
  if have_systemd; then
    systemctl "${SCOPE[@]}" disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
    rm -f "$(unit_path)"
    systemctl "${SCOPE[@]}" daemon-reload
  fi
  rm -f "$INSTALLED_BIN"
  log "removed the service and binary; $CONFIG_DIR and $DATA_DIR were left in place"
}

case "${1:-install}" in
  install)   cmd_install ;;
  update)    cmd_update ;;
  start)     cmd_start ;;
  stop)      cmd_stop ;;
  restart)   cmd_restart ;;
  status)    cmd_status ;;
  logs)      cmd_logs ;;
  uninstall) cmd_uninstall ;;
  *) die "unknown command '${1}'. Try: install | update | start | stop | restart | status | logs | uninstall" ;;
esac
