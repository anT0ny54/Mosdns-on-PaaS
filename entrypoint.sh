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

while :; do
  if ! kill -0 "$MOSDNS_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi
  if ! kill -0 "$GATEWAY_PID" 2>/dev/null; then
    terminate_children
    exit 1
  fi

  # Run sleep in the background so a trapped signal can interrupt the wait.
  sleep 1 &
  WAIT_PID=$!
  wait "$WAIT_PID" 2>/dev/null || true
done
