#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${MOSDNS_BACKEND_PORT:=18080}"
: "${DOH_PATH:=/dns-query}"
: "${HEALTH_PATH:=/health}"
: "${CACHE_SIZE:=4096}"
: "${CACHE_DUMP_FILE:=/var/cache/mosdns/cache.dump}"
: "${CACHE_DUMP_INTERVAL:=1800}"
: "${DOH_RATE_LIMIT:=5}"
: "${DOH_RATE_BURST:=12}"
: "${DOH_RATE_MAX_IPS:=512}"
: "${GLOBAL_RATE_LIMIT:=40}"
: "${GLOBAL_RATE_BURST:=80}"
: "${GLOBAL_CONN_LIMIT:=128}"
: "${DOH_MAX_BODY_BYTES:=4096}"
: "${IP_CONN_LIMIT:=0}"
: "${UPSTREAM_IDLE_TIMEOUT:=30}"
: "${DOH_IDLE_TIMEOUT:=90}"
: "${SERVER_TIMEOUT:=5}"
: "${HEALTH_TIMEOUT_MS:=1200}"
: "${HEALTH_CHECK:=true}"
: "${HAGEZI_UPSTREAM:=health}"
: "${HEALTH_EWMA_ALPHA:=0.35}"
: "${HEALTH_FAILURE_PENALTY_MS:=1500}"
: "${HEALTH_STATE_FILE:=/tmp/mosdns-upstream-state.tsv}"
: "${GOMEMLIMIT:=384MiB}"
: "${UPSTREAM_0_IP:=188.34.161.210}"
: "${UPSTREAM_1_IP:=159.69.155.94}"
: "${UPSTREAM_2_IP:=95.217.163.17}"

is_uint() { case "$2" in ''|*[!0-9]*) echo "Invalid $1: $2" >&2; exit 1;; esac; }
is_float() {
  awk -v v="$2" 'BEGIN { exit !(v >= 0) }' 2>/dev/null ||
    { echo "Invalid $1: $2" >&2; exit 1; }
}

is_uint PORT "$PORT"
is_uint MOSDNS_BACKEND_PORT "$MOSDNS_BACKEND_PORT"
is_uint CACHE_SIZE "$CACHE_SIZE"
is_uint CACHE_DUMP_INTERVAL "$CACHE_DUMP_INTERVAL"
is_uint DOH_RATE_BURST "$DOH_RATE_BURST"
is_uint DOH_RATE_MAX_IPS "$DOH_RATE_MAX_IPS"
is_uint GLOBAL_RATE_BURST "$GLOBAL_RATE_BURST"
is_uint GLOBAL_CONN_LIMIT "$GLOBAL_CONN_LIMIT"
is_uint DOH_MAX_BODY_BYTES "$DOH_MAX_BODY_BYTES"
is_uint IP_CONN_LIMIT "$IP_CONN_LIMIT"
is_uint UPSTREAM_IDLE_TIMEOUT "$UPSTREAM_IDLE_TIMEOUT"
is_uint DOH_IDLE_TIMEOUT "$DOH_IDLE_TIMEOUT"
is_uint SERVER_TIMEOUT "$SERVER_TIMEOUT"
is_uint HEALTH_TIMEOUT_MS "$HEALTH_TIMEOUT_MS"
is_uint HEALTH_FAILURE_PENALTY_MS "$HEALTH_FAILURE_PENALTY_MS"
is_float DOH_RATE_LIMIT "$DOH_RATE_LIMIT"
is_float GLOBAL_RATE_LIMIT "$GLOBAL_RATE_LIMIT"
is_float HEALTH_EWMA_ALPHA "$HEALTH_EWMA_ALPHA"

[ "$PORT" -gt 0 ] || { echo "PORT must be > 0" >&2; exit 1; }
[ "$MOSDNS_BACKEND_PORT" -gt 0 ] || { echo "MOSDNS_BACKEND_PORT must be > 0" >&2; exit 1; }
[ "$CACHE_SIZE" -gt 0 ] || { echo "CACHE_SIZE must be > 0" >&2; exit 1; }
[ "$DOH_RATE_BURST" -gt 0 ] || { echo "DOH_RATE_BURST must be > 0" >&2; exit 1; }
[ "$DOH_RATE_MAX_IPS" -gt 0 ] || { echo "DOH_RATE_MAX_IPS must be > 0" >&2; exit 1; }
[ "$GLOBAL_RATE_BURST" -gt 0 ] || { echo "GLOBAL_RATE_BURST must be > 0" >&2; exit 1; }
[ "$GLOBAL_CONN_LIMIT" -gt 0 ] || { echo "GLOBAL_CONN_LIMIT must be > 0" >&2; exit 1; }
[ "$DOH_MAX_BODY_BYTES" -ge 512 ] || { echo "DOH_MAX_BODY_BYTES must be >= 512" >&2; exit 1; }
[ "$SERVER_TIMEOUT" -gt 0 ] || { echo "SERVER_TIMEOUT must be > 0" >&2; exit 1; }
[ "$IP_CONN_LIMIT" -ge 0 ] || { echo "IP_CONN_LIMIT must be >= 0" >&2; exit 1; }

case "$DOH_PATH" in /*) ;; *) echo "DOH_PATH must start with /" >&2; exit 1;; esac
case "$HEALTH_PATH" in /*) ;; *) echo "HEALTH_PATH must start with /" >&2; exit 1;; esac

# These are deliberately HTTPS-only DoH endpoints.
UPSTREAM_0="https://root.hagezi.org/dns-query"
UPSTREAM_1="https://wurzn.hagezi.org/dns-query"
UPSTREAM_2="https://juuri.hagezi.org/dns-query"

esc() { printf '%s' "$1" | sed 's/[\\&|]/\\&/g'; }

PORT_E=$(esc "$PORT")
BACKEND_E=$(esc "$MOSDNS_BACKEND_PORT")
DOH_PATH_E=$(esc "$DOH_PATH")
CACHE_SIZE_E=$(esc "$CACHE_SIZE")
CACHE_FILE_E=$(esc "$CACHE_DUMP_FILE")
CACHE_INTERVAL_E=$(esc "$CACHE_DUMP_INTERVAL")
UPSTREAM_IDLE_E=$(esc "$UPSTREAM_IDLE_TIMEOUT")
SERVER_TIMEOUT_E=$(esc "$SERVER_TIMEOUT")
DOH_IDLE_E=$(esc "$DOH_IDLE_TIMEOUT")
U0_E=$(esc "$UPSTREAM_0")
U1_E=$(esc "$UPSTREAM_1")
U2_E=$(esc "$UPSTREAM_2")
U0IP_E=$(esc "$UPSTREAM_0_IP")
U1IP_E=$(esc "$UPSTREAM_1_IP")
U2IP_E=$(esc "$UPSTREAM_2_IP")

CACHE_DIR=$(dirname "$CACHE_DUMP_FILE")
mkdir -p "$CACHE_DIR" 2>/dev/null || true
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy 2>/dev/null || true
export GOMEMLIMIT

# Probe all three endpoints once at startup. The result only chooses the
# preferred order; MosDNS remains responsible for live sequential failover.
ORDER_0="$UPSTREAM_0"
ORDER_1="$UPSTREAM_1"
ORDER_2="$UPSTREAM_2"

probe_and_order() {
  [ "$HEALTH_CHECK" = "true" ] || return 1
  command -v mosdns-probe >/dev/null 2>&1 || return 1

  out=$(
    HEALTH_TIMEOUT_MS="$HEALTH_TIMEOUT_MS" \
    HEALTH_EWMA_ALPHA="$HEALTH_EWMA_ALPHA" \
    HEALTH_FAILURE_PENALTY_MS="$HEALTH_FAILURE_PENALTY_MS" \
    HEALTH_STATE_FILE="$HEALTH_STATE_FILE" \
        mosdns-probe "$UPSTREAM_0" "$UPSTREAM_1" "$UPSTREAM_2" 2>/dev/null || true
  )
  [ -n "$out" ] || return 1

  ORDER_0=$(printf '%s\n' "$out" | sed -n '1p' | cut -f1)
  ORDER_1=$(printf '%s\n' "$out" | sed -n '2p' | cut -f1)
  ORDER_2=$(printf '%s\n' "$out" | sed -n '3p' | cut -f1)
  [ -n "$ORDER_0" ] && [ -n "$ORDER_1" ] && [ -n "$ORDER_2" ]
}

case "$HAGEZI_UPSTREAM" in
  health|rotate|random) probe_and_order || true ;;
  *) echo "HAGEZI_UPSTREAM must be health, rotate, or random" >&2; exit 1 ;;
esac

# Resolve the pinned dial address matching the final endpoint order.
dial_for() {
  case "$1" in
    "$UPSTREAM_0") printf '%s' "$UPSTREAM_0_IP" ;;
    "$UPSTREAM_1") printf '%s' "$UPSTREAM_1_IP" ;;
    "$UPSTREAM_2") printf '%s' "$UPSTREAM_2_IP" ;;
    *) printf '' ;;
  esac
}

U0IP=$(dial_for "$ORDER_0"); U1IP=$(dial_for "$ORDER_1"); U2IP=$(dial_for "$ORDER_2")
U0_E=$(esc "$ORDER_0"); U1_E=$(esc "$ORDER_1"); U2_E=$(esc "$ORDER_2")
U0IP_E=$(esc "$U0IP"); U1IP_E=$(esc "$U1IP"); U2IP_E=$(esc "$U2IP")

sed \
  -e "s|__CACHE_SIZE__|$CACHE_SIZE_E|g" \
  -e "s|__CACHE_DUMP_FILE__|$CACHE_FILE_E|g" \
  -e "s|__CACHE_DUMP_INTERVAL__|$CACHE_INTERVAL_E|g" \
  -e "s|__UPSTREAM_0__|$U0_E|g" \
  -e "s|__UPSTREAM_1__|$U1_E|g" \
  -e "s|__UPSTREAM_2__|$U2_E|g" \
  -e "s|__UPSTREAM_0_IP__|$U0IP_E|g" \
  -e "s|__UPSTREAM_1_IP__|$U1IP_E|g" \
  -e "s|__UPSTREAM_2_IP__|$U2IP_E|g" \
  -e "s|__UPSTREAM_IDLE_TIMEOUT__|$UPSTREAM_IDLE_E|g" \
  -e "s|__MOSDNS_BACKEND_PORT__|$BACKEND_E|g" \
  -e "s|__SERVER_TIMEOUT__|$SERVER_TIMEOUT_E|g" \
  -e "s|__DOH_IDLE_TIMEOUT__|$DOH_IDLE_E|g" \
  -e "s|__DOH_PATH__|$DOH_PATH_E|g" \
  /etc/mosdns/config.yaml > /tmp/mosdns-config.yaml

if grep -Eq '__[A-Z0-9_]+__' /tmp/mosdns-config.yaml; then
  echo "ERROR: unresolved template token" >&2
  exit 1
fi

echo "MosDNS $(mosdns version 2>/dev/null || true)"
echo "Preferred upstream order:"
echo "  1. $ORDER_0"
echo "  2. $ORDER_1"
echo "  3. $ORDER_2"

ip-conn-proxy &
PROXY_PID=$!

mosdns start -c /tmp/mosdns-config.yaml &
MOSDNS_PID=$!

cleanup() {
  trap - TERM INT EXIT
  kill -TERM "$PROXY_PID" 2>/dev/null || true
  kill -TERM "$MOSDNS_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
  wait "$MOSDNS_PID" 2>/dev/null || true
}
trap cleanup TERM INT EXIT

while :; do
  if ! kill -0 "$MOSDNS_PID" 2>/dev/null; then
    echo "MosDNS exited; terminating instance" >&2
    exit 1
  fi
  if ! kill -0 "$PROXY_PID" 2>/dev/null; then
    echo "DoH proxy exited; terminating instance" >&2
    exit 1
  fi
  sleep 2
done
