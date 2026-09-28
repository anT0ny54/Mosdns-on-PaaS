#!/bin/sh
set -eu

MOSDNS_CONFIG="${MOSDNS_CONFIG:-/etc/mosdns/config.yaml}"
MOSDNS_DIR="${MOSDNS_DIR:-/etc/mosdns}"

MOSDNS_PID=""
GATEWAY_PID=""

# Shut down in dependency order: the gateway first, so it can drain in-flight
# requests (up to its 10s shutdown timeout) while MosDNS is still answering,
# then MosDNS. Signalling both at once would make every draining request fail.
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

# Installed before either child is started so an early signal is not lost.
trap on_signal TERM INT HUP

mkdir -p "$MOSDNS_DIR"

/usr/local/bin/mosdns start -c "$MOSDNS_CONFIG" -d "$MOSDNS_DIR" &
MOSDNS_PID=$!

/usr/local/bin/doh-gateway &
GATEWAY_PID=$!

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
