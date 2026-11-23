#!/bin/bash
# install.sh — install WGuard (WG-over-TCP client) on a Muse VPS.
#
# Usage:
#   ./install.sh [--config /path/to/wg-muse.conf] [--dir /path/to/install]
#
# Env overrides:
#   WG_SSH_KEY     SSH private key for Cloud D (default ~/.ssh/id_ed25519)
#   WG_SSH_TARGET  SSH target for Cloud D (default root@217.216.146.111)
#   WG_RELEASE_URL Prebuilt wgtcp binary URL (default: v1.0.0 release asset)
#   GO_VERSION     Go version used when building from source (default 1.27.4)
#
# Prerequisites (must be done BEFORE running this):
#   1. This Muse has its OWN WireGuard peer on Cloud D (wg0). Create it in
#      the WG panel (http://10.200.200.1:8088, menu Tambah Device) — the
#      panel generates a .conf with a unique private key + IP (10.200.200.x).
#      NEVER reuse another Muse's .conf: one private key = one peer.
#   2. This Muse has SSH key access to Cloud D as root (its own key in
#      /root/.ssh/authorized_keys on Cloud D).
#   3. x86-64 Linux. The wgtcp binary is fully static; no Go needed unless
#      building from source (installer does that automatically when needed).
set -eu

DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALL_DIR="${INSTALL_DIR:-$HOME/wg-tcp}"
WG_SSH_KEY="${WG_SSH_KEY:-$HOME/.ssh/id_ed25519}"
WG_SSH_TARGET="${WG_SSH_TARGET:-root@217.216.146.111}"
WG_RELEASE_URL="${WG_RELEASE_URL:-https://github.com/wahyusetiaone/WGuard/releases/download/v1.0.0/wgtcp}"
GO_VERSION="${GO_VERSION:-1.27.4}"
CONFIG_SRC=""

while [ $# -gt 0 ]; do
    case "$1" in
        --config) CONFIG_SRC="$2"; shift 2 ;;
        --dir) INSTALL_DIR="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 1 ;;
    esac
done

echo "== WGuard installer =="
echo "install dir : $INSTALL_DIR"
echo "ssh target  : $WG_SSH_TARGET (key $WG_SSH_KEY)"

[ "$(uname -m)" = "x86_64" ] || { echo "ERROR: only x86_64 is supported (got $(uname -m))" >&2; exit 1; }
command -v ssh >/dev/null || { echo "ERROR: ssh not found" >&2; exit 1; }
command -v curl >/dev/null || { echo "ERROR: curl not found" >&2; exit 1; }
[ -f "$WG_SSH_KEY" ] || { echo "ERROR: SSH key not found: $WG_SSH_KEY" >&2; exit 1; }

mkdir -p "$INSTALL_DIR/logs"
cp "$DIR/supervise.sh" "$INSTALL_DIR/"
chmod +x "$INSTALL_DIR/supervise.sh"

# Config: explicit --config wins, else a wg-muse.conf next to the installer.
if [ -n "$CONFIG_SRC" ]; then
    cp "$CONFIG_SRC" "$INSTALL_DIR/wg-muse.conf"
elif [ -f "$DIR/wg-muse.conf" ]; then
    cp "$DIR/wg-muse.conf" "$INSTALL_DIR/wg-muse.conf"
elif [ -f "$INSTALL_DIR/wg-muse.conf" ]; then
    echo "keeping existing $INSTALL_DIR/wg-muse.conf"
else
    echo "ERROR: no WireGuard config found." >&2
    echo "Pass your own .conf: ./install.sh --config /path/to/wg-muse.conf" >&2
    echo "Get one from the WG panel (Tambah Device) — one peer per Muse." >&2
    exit 1
fi
chmod 600 "$INSTALL_DIR/wg-muse.conf"

# Sanity: config must have the three fields, and must not be the template.
grep -q '^PrivateKey *= *[^<]' "$INSTALL_DIR/wg-muse.conf" || { echo "ERROR: PrivateKey missing or still a placeholder in wg-muse.conf" >&2; exit 1; }
grep -q '^Address *=' "$INSTALL_DIR/wg-muse.conf" || { echo "ERROR: Address missing in wg-muse.conf" >&2; exit 1; }
grep -q '^PublicKey *=' "$INSTALL_DIR/wg-muse.conf" || { echo "ERROR: PublicKey missing in wg-muse.conf" >&2; exit 1; }

# --- Get the wgtcp binary -----------------------------------------------
# 1) bundled next to this script, 2) prebuilt release asset, 3) build from source.
get_wgtcp() {
    if [ -x "$DIR/wgtcp" ]; then
        echo "using bundled wgtcp binary"
        cp "$DIR/wgtcp" "$INSTALL_DIR/wgtcp"
        return 0
    fi
    echo "downloading prebuilt wgtcp from $WG_RELEASE_URL ..."
    if curl -fsSL -o "$INSTALL_DIR/wgtcp" "$WG_RELEASE_URL" && [ -s "$INSTALL_DIR/wgtcp" ]; then
        chmod +x "$INSTALL_DIR/wgtcp"
        if "$INSTALL_DIR/wgtcp" --help >/dev/null 2>&1; then
            echo "prebuilt wgtcp OK"
            return 0
        fi
        echo "downloaded file is not a working wgtcp binary, falling back to source build"
        rm -f "$INSTALL_DIR/wgtcp"
    else
        echo "release download failed, falling back to source build"
        rm -f "$INSTALL_DIR/wgtcp"
    fi
    build_wgtcp
}

build_wgtcp() {
    echo "building wgtcp from source..."
    local go_bin=""
    if command -v go >/dev/null 2>&1; then
        go_bin="$(command -v go)"
    else
        local go_dir="$HOME/go"
        if [ ! -x "$go_dir/bin/go" ]; then
            echo "downloading Go $GO_VERSION ..."
            mkdir -p "$HOME/go-dl"
            curl -fsSL -o "$HOME/go-dl/go.tgz" "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" || { echo "ERROR: Go download failed" >&2; return 1; }
            tar -C "$HOME" -xzf "$HOME/go-dl/go.tgz"
            rm -f "$HOME/go-dl/go.tgz"
        fi
        go_bin="$go_dir/bin/go"
    fi
    (cd "$DIR/src" && CGO_ENABLED=0 "$go_bin" build -trimpath -ldflags='-s -w' -o "$INSTALL_DIR/wgtcp" .) || { echo "ERROR: source build failed" >&2; return 1; }
    chmod +x "$INSTALL_DIR/wgtcp"
    echo "source build OK"
}

get_wgtcp || exit 1

# SSH to Cloud D must work (key auth, no password).
echo "testing SSH to Cloud D..."
if ! ssh -i "$WG_SSH_KEY" -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=accept-new "$WG_SSH_TARGET" 'echo cloud-d-ok' 2>&1 | grep -q cloud-d-ok; then
    echo "ERROR: cannot SSH to $WG_SSH_TARGET with $WG_SSH_KEY" >&2
    echo "Make sure this Muse's public key is in /root/.ssh/authorized_keys on Cloud D." >&2
    exit 1
fi
echo "SSH OK"

# Relay on Cloud D: check the shared relay is listening (it is installed once,
# shared by all Muses — do NOT reinstall it per Muse).
if ! ssh -i "$WG_SSH_KEY" -o BatchMode=yes -o ConnectTimeout=15 "$WG_SSH_TARGET" 'ss -ltn | grep -q ":51822 "' 2>/dev/null; then
    echo "WARNING: relay on Cloud D port 51822 is not listening." >&2
    echo "Install it once on Cloud D (see README.md, section 'Relay Cloud D')." >&2
    echo "Continuing anyway — the tunnel will come up once the relay is running." >&2
else
    echo "relay on Cloud D OK"
fi

# Start (or restart) the tunnel + client via the supervisor.
export WG_SSH_KEY WG_SSH_TARGET
"$INSTALL_DIR/supervise.sh"
sleep 2

# Wait for the WG handshake (up to 30s).
echo "waiting for WireGuard handshake..."
ok=""
for i in $(seq 1 15); do
    hs=$(curl -s --max-time 5 http://127.0.0.1:18080/status 2>/dev/null | grep '^last_handshake_time_sec=' | cut -d= -f2 || true)
    if [ -n "$hs" ] && [ "$hs" != "0" ]; then ok=1; break; fi
    sleep 2
done

if [ -n "$ok" ]; then
    echo ""
    echo "SUCCESS — this Muse is now a WireGuard peer."
    curl -s --max-time 5 http://127.0.0.1:18080/status
    echo ""
    echo "Local forwards (through the tunnel):"
    echo "  ADB to the phone : 127.0.0.1:15555 -> 10.200.200.3:5555"
    echo "  WG panel         : 127.0.0.1:18088 -> 10.200.200.1:8088"
    echo "  Status           : http://127.0.0.1:18080/status"
    echo ""
    echo "Next: set up the 5-minute watchdog (see README.md, section 'Watchdog')."
    echo "Then test: curl -s http://127.0.0.1:18088/ (expect HTTP 302, WG login page)."
else
    echo "WARNING: no handshake yet after 30s. Check logs:" >&2
    echo "  $INSTALL_DIR/logs/wgtcp.log" >&2
    echo "  $INSTALL_DIR/logs/ssh-tunnel.log" >&2
    echo "  Cloud D: journalctl -u wg-tcp-relay.service" >&2
    exit 1
fi
