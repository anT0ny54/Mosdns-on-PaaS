#!/bin/sh
set -eu

MOSDNS_CONFIG="${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}"
MOSDNS_DIR="${MOSDNS_DIR:-/etc/mosdns}"
MOSDNS_PID=""
GATEWAY_PID=""

terminate_children() {
  if [ -n "$GATEWAY_PID" ]; then
    kill -TERM "$GATEWAY_PID" 2>/dev/null || true
    wait "$GATEWAY_PID" 2>/dev/null || true
  fi
  if [ -n "$MOSDNS_PID" ]; then
    kill -TERM "$MOSDNS_PID" 2>/dev/null || true
    wait "$MOSDNS_PID" 2>/dev/null || true
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
while :; do
  if ! kill -0 "$MOSDNS_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi
  wait -n 2>/dev/null || true
done
