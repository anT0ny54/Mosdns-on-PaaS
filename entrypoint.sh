#!/bin/sh
set -eu

MOSDNS_CONFIG="${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}"
MOSDNS_DIR="${MOSDNS_DIR:-/etc/mosdns}"
MOSDNS_PID=""
GATEWAY_PID=""
# Shutdown grace periods in seconds. The gateway drains in-flight requests for
# up to its own shutdown timeout (10s by default), so it gets the larger
# budget; MosDNS stops fast, so it gets a short one. A watchdog escalates to
# KILL after the grace period so a wedged child cannot hang the container.
GATEWAY_SHUTDOWN_GRACE=15
MOSDNS_SHUTDOWN_GRACE=5

# TERM a child and wait for it to exit -- `wait` is what reaps it, so this
# returns promptly on a clean exit even though `kill -0` would still succeed
# on the zombie. A watchdog KILLs the child once the grace period elapses;
# it is cancelled as soon as the child is reaped.
wait_pid() {
  pid=$1
  grace=$2
  kill -TERM "$pid" 2>/dev/null || true
  ( sleep "$grace"; kill -KILL "$pid" 2>/dev/null || true ) &
  watchdog=$!
  wait "$pid" 2>/dev/null || true
  kill "$watchdog" 2>/dev/null || true
  wait "$watchdog" 2>/dev/null || true
}

terminate_children() {
  if [ -n "$GATEWAY_PID" ]; then
    wait_pid "$GATEWAY_PID" "$GATEWAY_SHUTDOWN_GRACE"
  fi
  if [ -n "$MOSDNS_PID" ]; then
    wait_pid "$MOSDNS_PID" "$MOSDNS_SHUTDOWN_GRACE"
  fi
}

on_signal() {
  trap - TERM INT HUP
  terminate_children
  exit 0
}

trap on_signal TERM INT HUP

mkdir -p "$MOSDNS_DIR"

/usr/local/bin/mosdns start -c "$MOSDNS_CONFIG" -d "$MOSDNS_DIR" &
MOSDNS_PID=$!

/usr/local/bin/doh-gateway &
GATEWAY_PID=$!

# Block in `wait -n` until EITHER child exits, instead of polling `kill -0`
# once per second. This fixes two failure modes of the old poll loop:
#  - An exited child becomes a zombie, and `kill -0` SUCCEEDS on a zombie,
#    so the old loop could never detect the death: the container hung until
#    the platform's health check killed it, and the graceful-shutdown path
#    never ran. `wait` reaps the child, after which `kill -0` correctly fails.
#  - The 1s poll also delayed death detection by up to a second; `wait -n`
#    reacts immediately.
# tini (the PID 1 here) reaps anything reparented to it, but a zombie whose
# parent is this script is only reaped when this script waits on it -- so the
# supervision loop itself must do the reaping regardless of tini.
# A trapped signal interrupts `wait` and runs on_signal, so shutdown stays
# prompt. Requires busybox ash >= 1.30 (Alpine 3.24 ships 1.37).
# The `kill -0` checks deliberately run only AFTER `wait -n` returns. The
# pre-wait checks the old loop had here could never fire: at that point an
# exited child is still an unreaped zombie and `kill -0` succeeds on zombies.
# `wait -n` is the reaper; the post-reap `kill -0` on the next iteration is
# the detector.
while :; do
  wait -n 2>/dev/null || true
  if ! kill -0 "$MOSDNS_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi
done
