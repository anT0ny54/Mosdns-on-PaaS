#!/bin/sh
set -eu

MOSDNS_CONFIG="${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}"
MOSDNS_DIR="${MOSDNS_DIR:-/etc/mosdns}"

mkdir -p "$MOSDNS_DIR"

/usr/local/bin/mosdns start -c "$MOSDNS_CONFIG" -d "$MOSDNS_DIR" &
MOSDNS_PID=$!

/usr/local/bin/doh-gateway &
GATEWAY_PID=$!

terminate_children() {
  kill -TERM "$MOSDNS_PID" "$GATEWAY_PID" 2>/dev/null || true
  wait "$MOSDNS_PID" 2>/dev/null || true
  wait "$GATEWAY_PID" 2>/dev/null || true
}

on_signal() {
  trap - TERM INT HUP
  terminate_children
  exit 0
}

trap on_signal TERM INT HUP

# One process dying is a deployment failure; do not leave a partial service alive.
# The poll sleeps in the background and is awaited with `wait`, so a signal
# interrupts it immediately and the trap runs without waiting out the sleep.
while :; do
  if ! kill -0 "$MOSDNS_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi
  sleep 1 &
  wait $!
done
