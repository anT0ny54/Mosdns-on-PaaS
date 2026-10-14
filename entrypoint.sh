#!/bin/sh
set -eu

MOSDNS_CONFIG="${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}"
MOSDNS_DIR="${MOSDNS_DIR:-/etc/mosdns}"
MOSDNS_BIN="${MOSDNS_BIN:-/usr/local/bin/mosdns}"
GATEWAY_BIN="${GATEWAY_BIN:-/usr/local/bin/doh-gateway}"
MOSDNS_PID=""
GATEWAY_PID=""
# Shutdown grace periods in seconds. The gateway drains in-flight requests for
# up to its own shutdown timeout (4s), so it gets the larger budget; MosDNS
# stops fast, so it gets a short one. Together (9s) they fit inside Docker's
# default 10s stop timeout. A child still alive after its grace period is
# KILLed so a wedged child cannot hang the container.
GATEWAY_SHUTDOWN_GRACE=6
MOSDNS_SHUTDOWN_GRACE=3

# True while the process exists and is not a zombie. `kill -0` alone is not
# enough: an exited child stays a zombie (and `kill -0` keeps succeeding) until
# its parent reaps it, so the state is read from /proc as well. Without /proc
# the check degrades to `kill -0`. Only shell builtins are used, so the
# once-per-second poll forks nothing.
is_alive() {
  kill -0 "$1" 2>/dev/null || return 1
  [ -r "/proc/$1/stat" ] || return 0
  # The state field follows the ")" that closes the command name.
  read -r state < "/proc/$1/stat" 2>/dev/null || return 0
  state=${state##*) }
  case "$state" in
    Z*|X*) return 1 ;;
  esac
  return 0
}

# TERM a child, wait up to $2 seconds for it to exit, then KILL it. Polling
# (instead of a watchdog subshell) means an already-dead child costs no wait.
stop_child() {
  pid=$1
  ticks=$(($2 * 5))
  kill -TERM "$pid" 2>/dev/null || true
  while [ "$ticks" -gt 0 ] && is_alive "$pid"; do
    sleep 0.2
    ticks=$((ticks - 1))
  done
  if is_alive "$pid"; then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  # Reap it so no zombie is left behind.
  wait "$pid" 2>/dev/null || true
}

terminate_children() {
  if [ -n "$GATEWAY_PID" ]; then
    stop_child "$GATEWAY_PID" "$GATEWAY_SHUTDOWN_GRACE"
  fi
  if [ -n "$MOSDNS_PID" ]; then
    stop_child "$MOSDNS_PID" "$MOSDNS_SHUTDOWN_GRACE"
  fi
}

on_signal() {
  trap - TERM INT HUP
  terminate_children
  exit 0
}

trap on_signal TERM INT HUP

mkdir -p "$MOSDNS_DIR"

"$MOSDNS_BIN" start -c "$MOSDNS_CONFIG" -d "$MOSDNS_DIR" &
MOSDNS_PID=$!

"$GATEWAY_BIN" &
GATEWAY_PID=$!

# Supervision: poll once per second until either child is gone. `wait -n` is
# not used because BusyBox ash does not return from it when a child is killed
# by a signal (OOM kill, SIGSEGV, SIGKILL), which left the container running
# with a single process. The sleep runs in the background and is waited on so
# a trapped TERM/INT/HUP interrupts it immediately.
while :; do
  if ! is_alive "$MOSDNS_PID" || ! is_alive "$GATEWAY_PID"; then
    # Already tearing down because a child died: a TERM arriving now must
    # not re-run the shutdown and turn this exit 1 into exit 0.
    trap '' TERM INT HUP
    terminate_children
    exit 1
  fi
  sleep 1 &
  wait $! 2>/dev/null || true
done
