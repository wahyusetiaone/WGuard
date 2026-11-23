#!/bin/bash
# supervise.sh — watchdog for the WG-over-TCP tunnel (generic version).
# Safe to run every few minutes (e.g. via a Muse cron every 5 minutes).
# Restarts whatever is down:
#   1. SSH tunnel to Cloud D (local forward 51822)
#   2. wgtcp (the WireGuard-over-TCP client)
# The Cloud D relay is a systemd service (wg-tcp-relay.service) and restarts
# itself; nothing to do for it here.
# Uses PID files, never pkill patterns (pkill -f can match this script itself).
#
# Env overrides:
#   WG_SSH_KEY     SSH private key for Cloud D (default ~/.ssh/id_ed25519)
#   WG_SSH_TARGET  SSH target for Cloud D (default root@217.216.146.111)
#   WG_CONFIG      WireGuard .conf (default $BASE/wg-muse.conf)
set -u
BASE="$(cd "$(dirname "$0")" && pwd)"
LOG="$BASE/logs/supervise.log"
mkdir -p "$BASE/logs"

WG_SSH_KEY="${WG_SSH_KEY:-$HOME/.ssh/id_ed25519}"
WG_SSH_TARGET="${WG_SSH_TARGET:-root@217.216.146.111}"
WG_CONFIG="${WG_CONFIG:-$BASE/wg-muse.conf}"

log() { echo "$(date '+%F %T') [supervise] $*" >> "$LOG"; }

alive_pid() { # alive_pid <pidfile> <pattern-in-cmdline>
    local pidfile="$1" pattern="$2"
    [ -f "$pidfile" ] || return 1
    local pid
    pid=$(cat "$pidfile" 2>/dev/null) || return 1
    [ -n "$pid" ] || return 1
    [ -d "/proc/$pid" ] || return 1
    grep -q "$pattern" "/proc/$pid/cmdline" 2>/dev/null
}

# 1. SSH tunnel: local port 51822 must listen.
if ! ss -ltn 2>/dev/null | grep -q ':51822 '; then
    log "SSH tunnel down, starting"
    setsid nohup ssh -i "$WG_SSH_KEY" -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -N -L 51822:127.0.0.1:51822 "$WG_SSH_TARGET" > "$BASE/logs/ssh-tunnel.log" 2>&1 < /dev/null &
    echo $! > "$BASE/logs/ssh-tunnel.pid"
    sleep 3
    if ss -ltn 2>/dev/null | grep -q ':51822 '; then
        log "SSH tunnel started"
    else
        log "SSH tunnel FAILED to start"
    fi
fi

# 2. wgtcp: health endpoint must answer.
if ! curl -s --max-time 5 http://127.0.0.1:18080/healthz 2>/dev/null | grep -q ok; then
    log "wgtcp down, starting"
    if alive_pid "$BASE/logs/wgtcp.pid" "wgtcp"; then
        kill "$(cat "$BASE/logs/wgtcp.pid")" 2>/dev/null || true
        sleep 1
    fi
    setsid nohup "$BASE/wgtcp" -config "$WG_CONFIG" > "$BASE/logs/wgtcp.log" 2>&1 < /dev/null &
    echo $! > "$BASE/logs/wgtcp.pid"
    sleep 5
    if curl -s --max-time 5 http://127.0.0.1:18080/healthz 2>/dev/null | grep -q ok; then
        log "wgtcp started"
    else
        log "wgtcp FAILED to start"
    fi
fi
