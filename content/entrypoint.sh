#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${DOH_PATH:=/dns-query}"
: "${CACHE_SIZE:=4096}"
: "${CACHE_DUMP_FILE:=/var/cache/mosdns/cache.dump}"
: "${CACHE_DUMP_INTERVAL:=900}"
: "${MAX_QPS:=20}"
: "${HAGEZI_UPSTREAM:=rotate}"
: "${ROTATE_INTERVAL:=900}"
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
validate_uint CACHE_SIZE "$CACHE_SIZE"
validate_uint CACHE_DUMP_INTERVAL "$CACHE_DUMP_INTERVAL"
validate_uint MAX_QPS "$MAX_QPS"
validate_uint ROTATE_INTERVAL "$ROTATE_INTERVAL"
validate_uint UPSTREAM_IDLE_TIMEOUT "$UPSTREAM_IDLE_TIMEOUT"
validate_uint SERVER_TIMEOUT "$SERVER_TIMEOUT"
validate_uint HEALTH_TIMEOUT_MS "$HEALTH_TIMEOUT_MS"
validate_uint HEALTH_FAILURE_PENALTY_MS "$HEALTH_FAILURE_PENALTY_MS"
validate_uint HEALTH_SWITCH_MARGIN_MS "$HEALTH_SWITCH_MARGIN_MS"
validate_float01 HEALTH_EWMA_ALPHA "$HEALTH_EWMA_ALPHA"
validate_float01 HEALTH_SWITCH_MARGIN_PCT "$HEALTH_SWITCH_MARGIN_PCT"

[ "$PORT" -gt 0 ] || { echo "PORT must be > 0" >&2; exit 1; }
[ "$CACHE_SIZE" -gt 0 ] || { echo "CACHE_SIZE must be > 0" >&2; exit 1; }
[ "$MAX_QPS" -gt 0 ] || { echo "MAX_QPS must be > 0" >&2; exit 1; }
[ "$ROTATE_INTERVAL" -ge 60 ] || { echo "ROTATE_INTERVAL must be at least 60 seconds" >&2; exit 1; }
[ "$SERVER_TIMEOUT" -gt 0 ] || { echo "SERVER_TIMEOUT must be > 0" >&2; exit 1; }

case "$DOH_PATH" in /*) ;; *) echo "DOH_PATH must start with /" >&2; exit 1 ;; esac
case "$HAGEZI_UPSTREAM" in rotate|random|https://*) ;; *) echo "HAGEZI_UPSTREAM must be 'rotate', 'random', or an https:// endpoint" >&2; exit 1 ;; esac

UPSTREAM_0="https://root.hagezi.org/dns-query"
UPSTREAM_1="https://wurzn.hagezi.org/dns-query"
UPSTREAM_2="https://juuri.hagezi.org/dns-query"
UPSTREAM_INDEX=0
ACTIVE_UPSTREAM=""

probe_and_score() {
  [ "$HEALTH_CHECK" = "true" ] || return 1
  command -v mosdns-probe >/dev/null 2>&1 || return 1
  export HEALTH_TIMEOUT_MS HEALTH_EWMA_ALPHA HEALTH_FAILURE_PENALTY_MS HEALTH_SWITCH_MARGIN_PCT HEALTH_SWITCH_MARGIN_MS HEALTH_STATE_FILE HEALTH_ACTIVE_UPSTREAM HAGEZI_UPSTREAM
  probe_output=$(mosdns-probe "$@" 2>/dev/null || true)
  [ -n "$probe_output" ] || return 1
  ORDER_0=$(printf '%s\n' "$probe_output" | sed -n '1p' | cut -f2)
  ORDER_1=$(printf '%s\n' "$probe_output" | sed -n '2p' | cut -f2)
  ORDER_2=$(printf '%s\n' "$probe_output" | sed -n '3p' | cut -f2)
  [ -n "$ORDER_0" ] && [ -n "$ORDER_1" ] && [ -n "$ORDER_2" ] || return 1
  echo "Health scores (smoothed; lower is better; 10000 = failed):"
  printf '%s\n' "$probe_output" | awk -F '\t' '{ printf "  %s: raw=%sms, ok=%s, score=%s, ewma=%sms, failures=%s\n", $2, $3, $4, $5, $6, $7 }'
  return 0
}

select_order() {
  case "$HAGEZI_UPSTREAM" in
    rotate)
      case "$UPSTREAM_INDEX" in
        0) SELECTED="$UPSTREAM_0" ;; 1) SELECTED="$UPSTREAM_1" ;; *) SELECTED="$UPSTREAM_2" ;;
      esac
      UPSTREAM_INDEX=$(( (UPSTREAM_INDEX + 1) % 3 ))
      ;;
    random)
      selected=$(( $(od -An -N2 -tu2 /dev/urandom) % 3 ))
      case "$selected" in
        0) SELECTED="$UPSTREAM_0" ;; 1) SELECTED="$UPSTREAM_1" ;; *) SELECTED="$UPSTREAM_2" ;;
      esac
      ;;
    *) SELECTED="$HAGEZI_UPSTREAM" ;;
  esac

  HEALTH_ACTIVE_UPSTREAM="$ACTIVE_UPSTREAM"
  if [ "$HAGEZI_UPSTREAM" = "rotate" ] || [ "$HAGEZI_UPSTREAM" = "random" ]; then
    if probe_and_score "$UPSTREAM_0" "$UPSTREAM_1" "$UPSTREAM_2"; then :; else
      case "$SELECTED" in
        "$UPSTREAM_0") ORDER_0="$UPSTREAM_0"; ORDER_1="$UPSTREAM_1"; ORDER_2="$UPSTREAM_2" ;;
        "$UPSTREAM_1") ORDER_0="$UPSTREAM_1"; ORDER_1="$UPSTREAM_2"; ORDER_2="$UPSTREAM_0" ;;
        *) ORDER_0="$UPSTREAM_2"; ORDER_1="$UPSTREAM_0"; ORDER_2="$UPSTREAM_1" ;;
      esac
    fi
  else
    if probe_and_score "$HAGEZI_UPSTREAM" "$UPSTREAM_0" "$UPSTREAM_1"; then :; else
      ORDER_0="$HAGEZI_UPSTREAM"; ORDER_1="$UPSTREAM_0"; ORDER_2="$UPSTREAM_1"
    fi
  fi

  # For rotate, the health-aware recommendation is primary. The old round-robin
  # selection is used only when health scoring is unavailable.
  if [ "$HAGEZI_UPSTREAM" = "rotate" ] || [ "$HAGEZI_UPSTREAM" = "random" ]; then
    if [ "$HEALTH_CHECK" != "true" ] || [ -z "${probe_output:-}" ]; then
      case "$SELECTED" in
        "$UPSTREAM_0") ORDER_0="$UPSTREAM_0"; ORDER_1="$UPSTREAM_1"; ORDER_2="$UPSTREAM_2" ;;
        "$UPSTREAM_1") ORDER_0="$UPSTREAM_1"; ORDER_1="$UPSTREAM_2"; ORDER_2="$UPSTREAM_0" ;;
        *) ORDER_0="$UPSTREAM_2"; ORDER_1="$UPSTREAM_0"; ORDER_2="$UPSTREAM_1" ;;
      esac
    fi
  else
    # Fixed custom endpoint is preferred only if healthy; otherwise best healthy
    # endpoint goes first and the custom endpoint remains the final fallback.
    fixed_ok=$(printf '%s\n' "${probe_output:-}" | awk -F '\t' -v u="$HAGEZI_UPSTREAM" '$2==u {print $4; exit}')
    if [ "$fixed_ok" = "true" ]; then
      ORDER_0="$HAGEZI_UPSTREAM"
      ORDER_1=$(printf '%s\n' "$probe_output" | awk -F '\t' -v u="$HAGEZI_UPSTREAM" '$2!=u {print $2}' | sed -n '1p')
      ORDER_2=$(printf '%s\n' "$probe_output" | awk -F '\t' -v u="$HAGEZI_UPSTREAM" '$2!=u {print $2}' | sed -n '2p')
    fi
  fi
  ACTIVE_UPSTREAM="$ORDER_0"
}

sed_escape_replacement() { printf '%s' "$1" | sed 's/[\\&|]/\\&/g'; }
PORT_ESCAPED=$(sed_escape_replacement "$PORT")
DOH_PATH_ESCAPED=$(sed_escape_replacement "$DOH_PATH")
CACHE_SIZE_ESCAPED=$(sed_escape_replacement "$CACHE_SIZE")
CACHE_DUMP_FILE_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_FILE")
CACHE_DUMP_INTERVAL_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_INTERVAL")
MAX_QPS_ESCAPED=$(sed_escape_replacement "$MAX_QPS")
UPSTREAM_IDLE_TIMEOUT_ESCAPED=$(sed_escape_replacement "$UPSTREAM_IDLE_TIMEOUT")
SERVER_TIMEOUT_ESCAPED=$(sed_escape_replacement "$SERVER_TIMEOUT")
CACHE_DIR=$(dirname "$CACHE_DUMP_FILE")
mkdir -p "$CACHE_DIR" 2>/dev/null || echo "Warning: unable to create cache directory $CACHE_DIR" >&2
export GOMEMLIMIT HEALTH_TIMEOUT_MS HEALTH_EWMA_ALPHA HEALTH_FAILURE_PENALTY_MS HEALTH_SWITCH_MARGIN_PCT HEALTH_SWITCH_MARGIN_MS HEALTH_STATE_FILE

TEMPLATE=/etc/mosdns/config.yaml
RUNTIME_CONFIG=/tmp/mosdns-config.yaml
MOSDNS_PID=
cleanup() {
  if [ -n "${MOSDNS_PID:-}" ] && kill -0 "$MOSDNS_PID" 2>/dev/null; then
    echo "Stopping MosDNS..."
    kill -TERM "$MOSDNS_PID" 2>/dev/null || true
    wait "$MOSDNS_PID" 2>/dev/null || true
  fi
}
trap cleanup TERM INT EXIT

echo "=== MosDNS runtime ==="
mosdns version
echo "======================"
echo "Build: adaptive-v5 (EWMA + failure penalty + hysteresis)"
echo "Upstream mode: ${HAGEZI_UPSTREAM}"
echo "Failover: sequential (selected -> next -> next)"
echo "Health scoring: ${HEALTH_CHECK}, probe timeout ${HEALTH_TIMEOUT_MS}ms"
echo "Health model: EWMA alpha ${HEALTH_EWMA_ALPHA}, failure penalty ${HEALTH_FAILURE_PENALTY_MS}ms"
echo "Switch hysteresis: ${HEALTH_SWITCH_MARGIN_PCT} or ${HEALTH_SWITCH_MARGIN_MS}ms"
echo "Upstream transport: MosDNS v4.5.3 fast_forward default request timeout; idle timeout ${UPSTREAM_IDLE_TIMEOUT}s"
echo "Rotation: every ${ROTATE_INTERVAL}s"
echo "Warm cache: ${CACHE_DUMP_FILE}, snapshot every ${CACHE_DUMP_INTERVAL}s"

while :; do
  select_order
  U0_ESCAPED=$(sed_escape_replacement "$ORDER_0")
  U1_ESCAPED=$(sed_escape_replacement "$ORDER_1")
  U2_ESCAPED=$(sed_escape_replacement "$ORDER_2")
  sed \
    -e "s|PORT_PLACEHOLDER|${PORT_ESCAPED}|g" \
    -e "s|PATH_PLACEHOLDER|${DOH_PATH_ESCAPED}|g" \
    -e "s|CACHE_SIZE_PLACEHOLDER|${CACHE_SIZE_ESCAPED}|g" \
    -e "s|CACHE_DUMP_FILE_PLACEHOLDER|${CACHE_DUMP_FILE_ESCAPED}|g" \
    -e "s|CACHE_DUMP_INTERVAL_PLACEHOLDER|${CACHE_DUMP_INTERVAL_ESCAPED}|g" \
    -e "s|MAX_QPS_PLACEHOLDER|${MAX_QPS_ESCAPED}|g" \
    -e "s|UPSTREAM_IDLE_TIMEOUT_PLACEHOLDER|${UPSTREAM_IDLE_TIMEOUT_ESCAPED}|g" \
    -e "s|SERVER_TIMEOUT_PLACEHOLDER|${SERVER_TIMEOUT_ESCAPED}|g" \
    -e "s|UPSTREAM_0_PLACEHOLDER|${U0_ESCAPED}|g" \
    -e "s|UPSTREAM_1_PLACEHOLDER|${U1_ESCAPED}|g" \
    -e "s|UPSTREAM_2_PLACEHOLDER|${U2_ESCAPED}|g" \
    "$TEMPLATE" > "$RUNTIME_CONFIG"
  echo "Failover order:"
  echo "  1. ${ORDER_0}"
  echo "  2. ${ORDER_1}"
  echo "  3. ${ORDER_2}"

  mosdns start -c "$RUNTIME_CONFIG" &
  MOSDNS_PID=$!
  if [ "$HAGEZI_UPSTREAM" != "rotate" ] && [ "$HAGEZI_UPSTREAM" != "random" ]; then
    wait "$MOSDNS_PID"; status=$?; MOSDNS_PID=; exit "$status"
  fi
  elapsed=0
  while kill -0 "$MOSDNS_PID" 2>/dev/null && [ "$elapsed" -lt "$ROTATE_INTERVAL" ]; do
    sleep 5
    elapsed=$((elapsed + 5))
  done
  if kill -0 "$MOSDNS_PID" 2>/dev/null; then
    echo "Rotation interval reached; gracefully switching upstream."
    kill -TERM "$MOSDNS_PID" 2>/dev/null || true
    wait "$MOSDNS_PID" 2>/dev/null || true
    MOSDNS_PID=
  else
    wait "$MOSDNS_PID" 2>/dev/null || true
    MOSDNS_PID=
    echo "MosDNS exited unexpectedly." >&2
    exit 1
  fi
done
