#!/usr/bin/env bash
# Sharebox server installer for Ubuntu/Debian with systemd.
#
#   sudo ./install.sh                      # interactive
#   sudo ./install.sh --domain share.example.org --email you@example.org --yes
#   sudo ./install.sh --upgrade            # replace the binary, keep the config
#   sudo ./install.sh --uninstall          # remove (keeps /etc/sharebox unless --purge)
#
# What it does: installs /usr/local/bin/sharebox-server, writes
# /etc/sharebox/env (secrets, mode 600), installs and starts the systemd
# service, opens the firewall in ufw if ufw is active, and checks that the
# server answers over HTTPS.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DST=/usr/local/bin/sharebox-server
ENV_DIR=/etc/sharebox
ENV_FILE=$ENV_DIR/env
UNIT=/etc/systemd/system/sharebox-server.service
TURN_PORTS="49160-49999"

DOMAIN="" EMAIL="" PUBLIC_IP="" DIRECT_ZONE="" DNS_NS=""
YES=0 MODE=install NO_TURN=0 PURGE=0 SKIP_CHECK=0

c_green=$'\e[32m' c_yellow=$'\e[33m' c_red=$'\e[31m' c_bold=$'\e[1m' c_off=$'\e[0m'
[ -t 1 ] || { c_green="" c_yellow="" c_red="" c_bold="" c_off=""; }
say()  { echo "${c_bold}==>${c_off} $*"; }
ok()   { echo "  ${c_green}✓${c_off} $*"; }
warn() { echo "  ${c_yellow}!${c_off} $*"; }
die()  { echo "${c_red}error:${c_off} $*" >&2; exit 1; }

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; cat <<EOF

Options:
  --domain NAME        public host name pointing at this server (A record)
  --email ADDR         contact for Let's Encrypt
  --public-ip IP       public IPv4 (auto-detected)
  --direct-zone ZONE   optional: DNS zone delegated here for direct HTTPS (see docs)
  --dns-ns NAME        public name of this server's DNS (with --direct-zone)
  --no-turn            do not run the relay (not recommended)
  --skip-check         do not wait for the HTTPS health check
  --yes                no questions; fail if something required is missing
  --upgrade            replace the binary only and restart
  --uninstall          stop and remove the service and binary
  --purge              with --uninstall: also delete /etc/sharebox and data
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --domain) DOMAIN="${2:-}"; shift ;;
    --email) EMAIL="${2:-}"; shift ;;
    --public-ip) PUBLIC_IP="${2:-}"; shift ;;
    --direct-zone) DIRECT_ZONE="${2:-}"; shift ;;
    --dns-ns) DNS_NS="${2:-}"; shift ;;
    --no-turn) NO_TURN=1 ;;
    --skip-check) SKIP_CHECK=1 ;;
    --yes|-y) YES=1 ;;
    --upgrade) MODE=upgrade ;;
    --uninstall) MODE=uninstall ;;
    --purge) PURGE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option $1 (see --help)" ;;
  esac
  shift
done

[ "$(id -u)" -eq 0 ] || die "run as root: sudo $0 $*"
command -v systemctl >/dev/null || die "systemd is required"

ask() { # var prompt default
  local __v=$1 prompt=$2 def=${3:-} reply
  if [ "$YES" -eq 1 ]; then printf -v "$__v" '%s' "$def"; return; fi
  if [ -n "$def" ]; then read -r -p "$prompt [$def]: " reply </dev/tty; else read -r -p "$prompt: " reply </dev/tty; fi
  printf -v "$__v" '%s' "${reply:-$def}"
}

find_binary() {
  local arch
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture $(uname -m)" ;;
  esac
  for f in "$HERE/sharebox-server" "$HERE"/sharebox-server-*-linux-"$arch"; do
    [ -f "$f" ] && { echo "$f"; return; }
  done
  die "sharebox-server binary for linux/$arch not found next to install.sh"
}

install_binary() {
  local src; src=$(find_binary)
  install -m 755 "$src" "$BIN_DST.new"
  "$BIN_DST.new" -h >/dev/null 2>&1 || true
  mv -f "$BIN_DST.new" "$BIN_DST"
  ok "installed $BIN_DST"
}

# ---------------------------------------------------------------- uninstall
if [ "$MODE" = uninstall ]; then
  say "Removing Sharebox server"
  systemctl disable --now sharebox-server 2>/dev/null || true
  rm -f "$UNIT" "$BIN_DST"
  systemctl daemon-reload
  if [ "$PURGE" -eq 1 ]; then
    rm -rf "$ENV_DIR" /var/lib/sharebox /var/lib/private/sharebox
    ok "removed configuration and data"
  else
    warn "kept $ENV_DIR (use --purge to delete it)"
  fi
  ok "done"
  exit 0
fi

# ------------------------------------------------------------------ upgrade
if [ "$MODE" = upgrade ]; then
  [ -f "$ENV_FILE" ] || die "no existing installation ($ENV_FILE missing); run without --upgrade"
  say "Upgrading Sharebox server"
  install_binary
  install -m 644 "$HERE/sharebox-server.service" "$UNIT"
  systemctl daemon-reload
  systemctl restart sharebox-server
  ok "restarted"
  exit 0
fi

# ------------------------------------------------------------------ install
say "Sharebox server setup"

if [ -f "$ENV_FILE" ]; then
  warn "existing configuration found in $ENV_FILE — it will be kept."
  warn "use --upgrade to only replace the binary, or --uninstall --purge to start over."
  KEEP_ENV=1
else
  KEEP_ENV=0
fi

if [ "$KEEP_ENV" -eq 0 ]; then
  # Public IP: from the routing table; if that's private (cloud 1:1 NAT), ask an
  # external service.
  if [ -z "$PUBLIC_IP" ]; then
    PUBLIC_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}' || true)
    case "$PUBLIC_IP" in
      10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*|100.6[4-9].*|100.[7-9][0-9].*|100.1[01][0-9].*|100.12[0-7].*|"")
        ext=$(curl -4 -fsS --max-time 5 https://api.ipify.org 2>/dev/null || curl -4 -fsS --max-time 5 https://ifconfig.me 2>/dev/null || true)
        [ -n "$ext" ] && PUBLIC_IP=$ext ;;
    esac
  fi
  ask PUBLIC_IP "Public IPv4 address of this server" "$PUBLIC_IP"
  [[ "$PUBLIC_IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid public IP '$PUBLIC_IP'"

  ask DOMAIN "Domain name for Sharebox (e.g. share.example.org)" "$DOMAIN"
  DOMAIN=${DOMAIN#https://}; DOMAIN=${DOMAIN%%/*}
  [[ "$DOMAIN" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || die "a domain name is required (an A record pointing at $PUBLIC_IP)"
  ask EMAIL "E-mail for Let's Encrypt notices" "$EMAIL"
  [ -n "$EMAIL" ] || die "an e-mail address is required for Let's Encrypt"
fi

# DNS check: autocert fails if the domain doesn't point here.
if [ "$KEEP_ENV" -eq 0 ]; then
  resolved=$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk 'NR==1{print $1}' || true)
  if [ "$resolved" = "$PUBLIC_IP" ]; then
    ok "$DOMAIN resolves to $PUBLIC_IP"
  else
    warn "$DOMAIN resolves to '${resolved:-nothing}', expected $PUBLIC_IP"
    warn "create an A record ${DOMAIN} -> ${PUBLIC_IP}; HTTPS won't work until it propagates"
    if [ "$YES" -eq 0 ]; then
      ask cont "Continue anyway? (y/N)" "n"
      [[ "$cont" =~ ^[Yy] ]] || die "aborted"
    fi
  fi
fi

# Ports in use by something else?
for p in 80 443; do
  if ss -ltnH "sport = :$p" 2>/dev/null | grep -q . && ! ss -ltnpH "sport = :$p" 2>/dev/null | grep -q sharebox; then
    warn "port $p is already in use ($(ss -ltnpH "sport = :$p" | awk '{print $NF}' | head -1)); stop that service (e.g. nginx/apache) first"
  fi
done

install_binary
mkdir -p "$ENV_DIR"; chmod 700 "$ENV_DIR"

if [ "$KEEP_ENV" -eq 0 ]; then
  SECRET=$(head -c 32 /dev/urandom | base64 | tr -d '\n')
  TURN=1; [ "$NO_TURN" -eq 1 ] && TURN=0
  umask 077
  cat > "$ENV_FILE" <<EOF
# Sharebox server configuration. Restart after editing:
#   sudo systemctl restart sharebox-server
SHAREBOX_PUBLIC_URL=https://$DOMAIN
SHAREBOX_LISTEN=:443
SHAREBOX_AUTOCERT=1
SHAREBOX_ACME_EMAIL=$EMAIL

SHAREBOX_TURN=$TURN
SHAREBOX_TURN_PUBLIC_IP=$PUBLIC_IP
SHAREBOX_TURN_SECRET=$SECRET
SHAREBOX_TURN_UDP=:3478
SHAREBOX_TURN_TCP=:3478
SHAREBOX_TURN_PORTS=$TURN_PORTS

# Optional direct HTTPS to hosts (needs a delegated DNS zone, see docs).
SHAREBOX_DIRECT_ZONE=$DIRECT_ZONE
SHAREBOX_DNS_NS=$DNS_NS
EOF
  chmod 600 "$ENV_FILE"
  ok "wrote $ENV_FILE (relay secret generated)"
fi

install -m 644 "$HERE/sharebox-server.service" "$UNIT"
systemctl daemon-reload
systemctl enable sharebox-server >/dev/null 2>&1
systemctl restart sharebox-server
ok "service sharebox-server enabled and started"

# Firewall (ufw only if active; cloud firewalls must be configured separately).
if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
  ufw allow 80/tcp >/dev/null; ufw allow 443/tcp >/dev/null
  ufw allow 3478/udp >/dev/null; ufw allow 3478/tcp >/dev/null
  ufw allow "${TURN_PORTS/-/:}"/udp >/dev/null
  if grep -q '^SHAREBOX_DIRECT_ZONE=.\+' "$ENV_FILE"; then ufw allow 53/udp >/dev/null; ufw allow 53/tcp >/dev/null; fi
  ok "ufw: opened 80, 443/tcp, 3478/udp+tcp, $TURN_PORTS/udp"
else
  warn "ufw is not active; make sure your cloud firewall allows TCP 80, 443, UDP+TCP 3478 and UDP $TURN_PORTS"
fi

sleep 2
if ! systemctl is-active --quiet sharebox-server; then
  journalctl -u sharebox-server -n 20 --no-pager || true
  die "the service failed to start (log above)"
fi

URL=$(grep '^SHAREBOX_PUBLIC_URL=' "$ENV_FILE" | cut -d= -f2-)
if [ "$SKIP_CHECK" -eq 0 ]; then
  say "Waiting for HTTPS (first start requests a Let's Encrypt certificate)…"
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 10 "$URL/healthz" >/dev/null 2>&1; then
      ok "$URL is up with a valid certificate"
      HEALTHY=1; break
    fi
    sleep 3
  done
  if [ "${HEALTHY:-0}" -ne 1 ]; then
    warn "could not reach $URL/healthz yet. Common causes: DNS not propagated, port 80/443 blocked by the cloud firewall."
    warn "watch the log: journalctl -u sharebox-server -f"
  fi
fi

cat <<EOF

${c_bold}Sharebox server is installed.${c_off}

  In the Sharebox app open Settings → Sharebox server and enter:
      ${c_bold}$URL${c_off}

  Status:   systemctl status sharebox-server
  Logs:     journalctl -u sharebox-server -f
  Config:   $ENV_FILE
  Upgrade:  sudo ./install.sh --upgrade   (from a newer package)
EOF
