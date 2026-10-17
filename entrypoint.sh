#!/bin/sh
# Entrypoint for the MosDNS container: starts MosDNS, then supervises both
# the gateway and MosDNS, restarting whichever child exits.
set -u

GATEWAY_BIN=${GATEWAY_BIN:-/app/doh-gateway}
MOSDNS_BIN=${MOSDNS_BIN:-/usr/bin/mosdns}
CONFIG=${CONFIG:-/etc/mosdns/config.yaml}

# Optional: extra args for the gateway (e.g. -listen :8080).
GATEWAY_ARGS=${GATEWAY_ARGS:-}

# When true (default), a MosDNS crash is fatal for the container; set to
# "false" only in exceptional debugging situations.
MOSDNS_REQUIRED=${MOSDNS_REQUIRED:-true}

log() { echo "[entrypoint] $*"; }

# is_alive uses /proc directly instead of `kill -0` so it also works when the
# child is a zombie (reaped only by our next `wait`).
is_alive() {
    local pid=$1 state
    [ -r "/proc/$pid/stat" ] || return 1
    state=$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null) || return 1
    # $3 is the process state character: R, S, D, Z, T, I... The parenthesized
    # comm field ($2) can itself contain spaces and parentheses, so we anchor
    # on the LAST ") " in the line: ${state##*) } strips everything up to it.
    state=${state##*) }
    [ "$state" != "Z" ]
}

# wait_reap blocks until the child is actually reaped, so a new process can
# reuse the PID slot safely.
wait_reap() {
    local pid=$1
    while [ -e "/proc/$pid" ]; do
        sleep 0.1
    done
}

start_gateway() {
    log "starting gateway: $GATEWAY_BIN $GATEWAY_ARGS"
    "$GATEWAY_BIN" $GATEWAY_ARGS &
    GATEWAY_PID=$!
    log "gateway pid $GATEWAY_PID"
}

start_mosdns() {
    log "starting mosdns: $MOSDNS_BIN run -d -c $CONFIG"
    "$MOSDNS_BIN" run -d -c "$CONFIG" &
    MOSDNS_PID=$!
    log "mosdns pid $MOSDNS_PID"
}

cleanup() {
    log "received shutdown signal, stopping children"
    trap - TERM INT
    [ -n "${GATEWAY_PID:-}" ] && kill "$GATEWAY_PID" 2>/dev/null
    [ -n "${MOSDNS_PID:-}" ] && kill "$MOSDNS_PID" 2>/dev/null
    wait
    log "all children stopped, exiting"
    exit 0
}
trap cleanup TERM INT

start_gateway
start_mosdns

log "supervising gateway=$GATEWAY_PID mosdns=$MOSDNS_PID"

while true; do
    if wait -n 2>/dev/null; then
        STATUS=$?
    else
        # BusyBox ash lacks `wait -n`; poll instead.
        STATUS=""
        if ! is_alive "$GATEWAY_PID"; then
            wait "$GATEWAY_PID"
            STATUS=$?
            EXITED=gateway
        elif ! is_alive "$MOSDNS_PID"; then
            wait "$MOSDNS_PID"
            STATUS=$?
            EXITED=mosdns
        else
            sleep 1
            continue
        fi
    fi

    if [ -z "$EXITED" ]; then
        if [ "$GATEWAY_PID" = "$!" ] 2>/dev/null; then
            EXITED=gateway
        elif [ "$MOSDNS_PID" = "$!" ] 2>/dev/null; then
            EXITED=mosdns
        else
            # Ambiguous exit (should not happen): determine from /proc.
            if is_alive "$GATEWAY_PID"; then
                EXITED=mosdns
            else
                EXITED=gateway
            fi
        fi
    fi

    log "$EXITED exited with status ${STATUS:-unknown}"

    case "$EXITED" in
        gateway)
            wait_reap "$GATEWAY_PID"
            log "restarting gateway"
            start_gateway
            ;;
        mosdns)
            wait_reap "$MOSDNS_PID"
            if [ "$MOSDNS_REQUIRED" = "true" ]; then
                log "mosdns is required; stopping container"
                kill "$GATEWAY_PID" 2>/dev/null
                wait
                exit 1
            fi
            log "restarting mosdns"
            start_mosdns
            ;;
    esac
    EXITED=""
done
