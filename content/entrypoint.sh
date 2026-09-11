#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${MOSDNS_BACKEND_PORT:=18080}"
: "${IP_CONN_LIMIT:=4}"
: "${HEALTH_PATH:=/health}"
: "${DOH_PATH:=/dns-query}"
: "${CACHE_SIZE:=4096}"
: "${CACHE_DUMP_FILE:=/var/cache/mosdns/cache.dump}"
: "${CACHE_DUMP_INTERVAL:=900}"
: "${MAX_QPS:=20}"
: "${HAGEZI_UPSTREAM:=rotate}"
: "${UPSTREAM_IDLE_TIMEOUT:=15}"
: "${SERVER_TIMEOUT:=5}"
: "${HEALTH_TIMEOUT_MS:=1200}"
: "${HEALTH_CHECK:=true}"
: "${HEALTH_EWMA_ALPHA:=0.35}"
: "${HEALTH_FAILURE_PENALTY_MS:=1500}"
: "${HEALTH_SWITCH_MARGIN_PCT:=0.20}"
: "${HEALTH_SWITCH_MARGIN_MS:=25}"
: "${HEALTH_STATE_FILE:=/tmp/mosdns-upstream-state.tsv}"
: "${GOMEMLIMIT:=384MiB}"

validate_uint() { case "$2" in ''|*[!0-9]*) echo "Invalid $1: $2" >&2; exit 1 ;; esac; }
validate_float01() {
  awk -v v="$2" 'BEGIN { exit !(v > 0 && v <= 1) }' 2>/dev/null || { echo "Invalid $1: $2 (must be >0 and <=1)" >&2; exit 1; }
}

validate_uint PORT "$PORT"
validate_uint MOSDNS_BACKEND_PORT "$MOSDNS_BACKEND_PORT"
validate_uint IP_CONN_LIMIT "$IP_CONN_LIMIT"
validate_uint CACHE_SIZE "$CACHE_SIZE"
validate_uint CACHE_DUMP_INTERVAL "$CACHE_DUMP_INTERVAL"
validate_uint MAX_QPS "$MAX_QPS"
validate_uint UPSTREAM_IDLE_TIMEOUT "$UPSTREAM_IDLE_TIMEOUT"
validate_uint SERVER_TIMEOUT "$SERVER_TIMEOUT"
validate_uint HEALTH_TIMEOUT_MS "$HEALTH_TIMEOUT_MS"
validate_uint HEALTH_FAILURE_PENALTY_MS "$HEALTH_FAILURE_PENALTY_MS"
validate_uint HEALTH_SWITCH_MARGIN_MS "$HEALTH_SWITCH_MARGIN_MS"
validate_float01 HEALTH_EWMA_ALPHA "$HEALTH_EWMA_ALPHA"
validate_float01 HEALTH_SWITCH_MARGIN_PCT "$HEALTH_SWITCH_MARGIN_PCT"

[ "$PORT" -gt 0 ] || { echo "PORT must be > 0" >&2; exit 1; }
[ "$MOSDNS_BACKEND_PORT" -gt 0 ] || { echo "MOSDNS_BACKEND_PORT must be > 0" >&2; exit 1; }
[ "$IP_CONN_LIMIT" -gt 0 ] || { echo "IP_CONN_LIMIT must be > 0" >&2; exit 1; }
[ "$CACHE_SIZE" -gt 0 ] || { echo "CACHE_SIZE must be > 0" >&2; exit 1; }
[ "$MAX_QPS" -gt 0 ] || { echo "MAX_QPS must be > 0" >&2; exit 1; }
[ "$SERVER_TIMEOUT" -gt 0 ] || { echo "SERVER_TIMEOUT must be > 0" >&2; exit 1; }

case "$DOH_PATH" in /*) ;; *) echo "DOH_PATH must start with /" >&2; exit 1 ;; esac
case "$HAGEZI_UPSTREAM" in rotate|random|https://*) ;; *) echo "HAGEZI_UPSTREAM must be 'rotate', 'random', or an https:// endpoint" >&2; exit 1 ;; esac

UPSTREAM_0="https://root.hagezi.org/dns-query"
UPSTREAM_1="https://wurzn.hagezi.org/dns-query"
UPSTREAM_2="https://juuri.hagezi.org/dns-query"

# Select a healthy order once at boot. MosDNS itself then provides sequential
# failover continuously; we deliberately do NOT restart MosDNS on a timer.
# Rebuilding the process every N minutes is unnecessary and can create avoidable
# connection gaps on a tiny Koyeb instance.
probe_and_score() {
  [ "$HEALTH_CHECK" = "true" ] || return 1
  command -v mosdns-probe >/dev/null 2>&1 || return 1
  export HEALTH_TIMEOUT_MS HEALTH_EWMA_ALPHA HEALTH_FAILURE_PENALTY_MS HEALTH_SWITCH_MARGIN_PCT HEALTH_SWITCH_MARGIN_MS HEALTH_STATE_FILE
  probe_output=$(mosdns-probe "$@" 2>/dev/null || true)
  [ -n "$probe_output" ] || return 1
  ORDER_0=$(printf '%s\n' "$probe_output" | sed -n '1p' | cut -f2)
  ORDER_1=$(printf '%s\n' "$probe_output" | sed -n '2p' | cut -f2)
  ORDER_2=$(printf '%s\n' "$probe_output" | sed -n '3p' | cut -f2)
  [ -n "$ORDER_0" ] && [ -n "$ORDER_1" ] && [ -n "$ORDER_2" ] || return 1
  printf '%s\n' "$probe_output" |
    awk -F '\t' '{ printf "  %s: raw=%sms ok=%s score=%s ewma=%sms failures=%s\n", $2,$3,$4,$5,$6,$7 }'
  return 0
}

select_order() {
  case "$HAGEZI_UPSTREAM" in
    rotate|random)
      if ! probe_and_score "$UPSTREAM_0" "$UPSTREAM_1" "$UPSTREAM_2"; then
        ORDER_0="$UPSTREAM_0"
        ORDER_1="$UPSTREAM_1"
        ORDER_2="$UPSTREAM_2"
      fi
      ;;
    *)
      if ! probe_and_score "$HAGEZI_UPSTREAM" "$UPSTREAM_0" "$UPSTREAM_1"; then
        ORDER_0="$HAGEZI_UPSTREAM"
        ORDER_1="$UPSTREAM_0"
        ORDER_2="$UPSTREAM_1"
      fi
      ;;
  esac
}

sed_escape_replacement() { printf '%s' "$1" | sed 's/[\\&|]/\\&/g'; }
PORT_ESCAPED=$(sed_escape_replacement "$PORT")
DOH_PATH_ESCAPED=$(sed_escape_replacement "$DOH_PATH")
CACHE_SIZE_ESCAPED=$(sed_escape_replacement "$CACHE_SIZE")
CACHE_DUMP_FILE_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_FILE")
CACHE_DUMP_INTERVAL_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_INTERVAL")
MAX_QPS_ESCAPED=$(sed_escape_replacement "$MAX_QPS")
MOSDNS_BACKEND_PORT_ESCAPED=$(sed_escape_replacement "$MOSDNS_BACKEND_PORT")
UPSTREAM_IDLE_TIMEOUT_ESCAPED=$(sed_escape_replacement "$UPSTREAM_IDLE_TIMEOUT")
SERVER_TIMEOUT_ESCAPED=$(sed_escape_replacement "$SERVER_TIMEOUT")
CACHE_DIR=$(dirname "$CACHE_DUMP_FILE")
mkdir -p "$CACHE_DIR" 2>/dev/null || echo "Warning: unable to create cache directory $CACHE_DIR" >&2
export GOMEMLIMIT

TEMPLATE=/etc/mosdns/config.yaml
RUNTIME_CONFIG=/tmp/mosdns-config.yaml
MOSDNS_PID=""
PROXY_PID=""

cleanup() {
  trap - TERM INT EXIT
  if [ -n "${PROXY_PID:-}" ] && kill -0 "$PROXY_PID" 2>/dev/null; then
    kill -TERM "$PROXY_PID" 2>/dev/null || true
    wait "$PROXY_PID" 2>/dev/null || true
  fi
  if [ -n "${MOSDNS_PID:-}" ] && kill -0 "$MOSDNS_PID" 2>/dev/null; then
    echo "Stopping MosDNS..."
    kill -TERM "$MOSDNS_PID" 2>/dev/null || true
    # Give the warm-cache backend time to snapshot.
    i=0
    while kill -0 "$MOSDNS_PID" 2>/dev/null && [ "$i" -lt 10 ]; do
      sleep 1
      i=$((i + 1))
    done
    if kill -0 "$MOSDNS_PID" 2>/dev/null; then
      kill -KILL "$MOSDNS_PID" 2>/dev/null || true
    fi
    wait "$MOSDNS_PID" 2>/dev/null || true
  fi
}
trap cleanup TERM INT EXIT

echo "=== MosDNS runtime ==="
mosdns version
echo "======================"
echo "Build: stable-v7.3 (collision-proof listener templating + backend-ready health + long-running supervisor + per-IP connection limiter)"
echo "Upstream mode: ${HAGEZI_UPSTREAM}"
echo "Sequential failover: enabled"
echo "Health scoring: ${HEALTH_CHECK}, probe timeout ${HEALTH_TIMEOUT_MS}ms"
echo "Health model: EWMA alpha ${HEALTH_EWMA_ALPHA}, failure penalty ${HEALTH_FAILURE_PENALTY_MS}ms"
echo "Connection idle timeout: ${UPSTREAM_IDLE_TIMEOUT}s"
echo "Server timeout: ${SERVER_TIMEOUT}s"
echo "Warm cache: ${CACHE_DUMP_FILE}, snapshot every ${CACHE_DUMP_INTERVAL}s"
echo "Automatic process restart: enabled; no scheduled rotation"
echo "Per-IP concurrent connection limit: ${IP_CONN_LIMIT}"
echo "Koyeb health endpoint: ${HEALTH_PATH}"
echo "MosDNS backend port: ${MOSDNS_BACKEND_PORT}"

select_order

U0_ESCAPED=$(sed_escape_replacement "$ORDER_0")
U1_ESCAPED=$(sed_escape_replacement "$ORDER_1")
U2_ESCAPED=$(sed_escape_replacement "$ORDER_2")
sed \
  -e "s|__SERVER_TIMEOUT__|${SERVER_TIMEOUT_ESCAPED}|g" \
  -e "s|__MOSDNS_BACKEND_PORT__|${MOSDNS_BACKEND_PORT_ESCAPED}|g" \
  -e "s|__DOH_PATH__|${DOH_PATH_ESCAPED}|g" \
  -e "s|__CACHE_SIZE__|${CACHE_SIZE_ESCAPED}|g" \
  -e "s|__CACHE_DUMP_FILE__|${CACHE_DUMP_FILE_ESCAPED}|g" \
  -e "s|__CACHE_DUMP_INTERVAL__|${CACHE_DUMP_INTERVAL_ESCAPED}|g" \
  -e "s|__MAX_QPS__|${MAX_QPS_ESCAPED}|g" \
  -e "s|__UPSTREAM_IDLE_TIMEOUT__|${UPSTREAM_IDLE_TIMEOUT_ESCAPED}|g" \
  -e "s|__UPSTREAM_0__|${U0_ESCAPED}|g" \
  -e "s|__UPSTREAM_1__|${U1_ESCAPED}|g" \
  -e "s|__UPSTREAM_2__|${U2_ESCAPED}|g" \
  "$TEMPLATE" > "$RUNTIME_CONFIG"

# Hard-fail on any unresolved template token.
if grep -Eq "(__[A-Z0-9_]+__)|BACKEND_[0-9]+|PORT_PLACEHOLDER|BACKEND_PORT_PLACEHOLDER|PATH_PLACEHOLDER" "$RUNTIME_CONFIG"; then
  echo "ERROR: unresolved placeholder in generated MosDNS config:" >&2
  grep -nE "(__[A-Z0-9_]+__)|BACKEND_[0-9]+|PORT_PLACEHOLDER|BACKEND_PORT_PLACEHOLDER|PATH_PLACEHOLDER" "$RUNTIME_CONFIG" >&2 || true
  exit 1
fi

echo "Generated MosDNS listener:"
grep -n "addr:" "$RUNTIME_CONFIG" || true

echo "Failover order:"
echo "  1. ${ORDER_0}"
echo "  2. ${ORDER_1}"
echo "  3. ${ORDER_2}"

# Public HTTP listener is the lightweight limiter proxy. MosDNS stays private
# on 127.0.0.1 so every public request passes through the per-IP limiter.
echo "Starting per-IP connection limiter on :${PORT} -> 127.0.0.1:${MOSDNS_BACKEND_PORT}"
LISTEN_ADDR=":${PORT}" BACKEND_ADDR="127.0.0.1:${MOSDNS_BACKEND_PORT}" IP_CONN_LIMIT="${IP_CONN_LIMIT}" HEALTH_PATH="${HEALTH_PATH}" ip-conn-proxy &
PROXY_PID=$!

# Long-running supervisor. A crashed MosDNS process is restarted in-place
# instead of terminating PID 1 and waiting for Koyeb to recreate the instance.
# Exponential backoff avoids a CPU spin if a bad deployment/config is supplied.
backoff=1
while :; do
  echo "Starting MosDNS..."
  mosdns start -c "$RUNTIME_CONFIG" &
  MOSDNS_PID=$!

  if wait "$MOSDNS_PID"; then
    status=0
  else
    status=$?
  fi
  MOSDNS_PID=

  if ! kill -0 "$PROXY_PID" 2>/dev/null; then
    echo "Limiter proxy exited; restarting it." >&2
    LISTEN_ADDR=":${PORT}" BACKEND_ADDR="127.0.0.1:${MOSDNS_BACKEND_PORT}" IP_CONN_LIMIT="${IP_CONN_LIMIT}" HEALTH_PATH="${HEALTH_PATH}" ip-conn-proxy &
    PROXY_PID=$!
  fi

  if [ "$status" -eq 0 ]; then
    echo "MosDNS exited normally; restarting in 2 seconds."
    backoff=2
  else
    echo "MosDNS exited with status ${status}; restarting in ${backoff}s." >&2
  fi

  sleep "$backoff"
  if [ "$backoff" -lt 30 ]; then
    backoff=$((backoff * 2))
    [ "$backoff" -gt 30 ] && backoff=30
  fi
done
