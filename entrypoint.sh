#!/bin/sh
set -eu

MOSDNS_CONFIG="${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}"
MOSDNS_DIR="${MOSDNS_DIR:-/etc/mosdns}"
MOSDNS_BIN="${MOSDNS_BIN:-/usr/local/bin/mosdns}"
GATEWAY_BIN="${GATEWAY_BIN:-/usr/local/bin/doh-gateway}"
MOSDNS_PID=""
GATEWAY_PID=""
SLEEP_PID=""
# Shutdown grace periods in seconds. The gateway drains in-flight requests for
# up to its own shutdown timeout (5s), so it gets the larger budget; MosDNS
# stops fast, so it gets a short one. Together (9s) they fit inside Docker's
# default 10s stop timeout. A child still alive after its grace period is
# KILLed so a wedged child cannot hang the container.
GATEWAY_SHUTDOWN_GRACE=6
MOSDNS_SHUTDOWN_GRACE=3

# When /proc is readable, true only while PID is a live direct child of this
# entrypoint; otherwise fall back to kill -0. The Linux parent-PID check keeps
# a reaped child PID from matching an unrelated process after PID reuse.
is_child_alive() {
  pid=$1
  kill -0 "$pid" 2>/dev/null || return 1
  [ -r "/proc/$pid/stat" ] || return 0
  read -r proc_stat < "/proc/$pid/stat" 2>/dev/null || return 1
  proc_stat=${proc_stat##*) }
  proc_state=${proc_stat%% *}
  proc_fields=${proc_stat#* }
  proc_ppid=${proc_fields%% *}
  [ "$proc_ppid" = "$$" ] || return 1
  case "$proc_state" in
    Z*|X*) return 1 ;;
  esac
  return 0
}

# TERM a child, wait up to $2 seconds for it to exit, then KILL it. Polling
# (instead of a watchdog subshell) means an already-dead child costs no wait.
stop_child() {
  pid=$1
  ticks=$(($2 * 5))
  if is_child_alive "$pid"; then
    kill -TERM "$pid" 2>/dev/null || true
  fi
  while [ "$ticks" -gt 0 ] && is_child_alive "$pid"; do
    sleep 0.2
    ticks=$((ticks - 1))
  done
  if is_child_alive "$pid"; then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  # Reap it so no zombie is left behind.
  wait "$pid" 2>/dev/null || true
}

terminate_children() {
  # Cancel the supervision sleep so it cannot outlive the entrypoint. When
  # /proc is readable, child signals are guarded by direct-child ownership so a
  # reaped PID cannot be mistaken for an unrelated process after PID reuse.
  if [ -n "$SLEEP_PID" ]; then
    if is_child_alive "$SLEEP_PID"; then
      kill "$SLEEP_PID" 2>/dev/null || true
    fi
    wait "$SLEEP_PID" 2>/dev/null || true
  fi
  if [ -n "$GATEWAY_PID" ]; then
    stop_child "$GATEWAY_PID" "$GATEWAY_SHUTDOWN_GRACE"
  fi
  if [ -n "$MOSDNS_PID" ]; then
    stop_child "$MOSDNS_PID" "$MOSDNS_SHUTDOWN_GRACE"
  fi
}

on_signal() {
  # Keep later TERM/INT/HUP signals from interrupting child cleanup. The
  # children still get their full grace periods and are KILLed/reaped if needed.
  trap '' TERM INT HUP
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
# a trapped TERM/INT/HUP interrupts it immediately; teardown then cancels it.
while :; do
  if ! is_child_alive "$MOSDNS_PID" || ! is_child_alive "$GATEWAY_PID"; then
    # Already tearing down because a child died: a TERM arriving now must
    # not re-run the shutdown and turn this exit 1 into exit 0.
    trap '' TERM INT HUP
    terminate_children
    exit 1
  fi
  sleep 1 &
  SLEEP_PID=$!
  wait "$SLEEP_PID" 2>/dev/null || true
  SLEEP_PID=""
done
