#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${MOSDNS_BACKEND_PORT:=18080}"
: "${IP_CONN_LIMIT:=0}"
: "${DOH_RATE_LIMIT:=5}"
: "${DOH_RATE_BURST:=12}"
: "${DOH_RATE_MAX_IPS:=512}"
: "${GLOBAL_RATE_LIMIT:=40}"
: "${GLOBAL_RATE_BURST:=80}"
: "${GLOBAL_CONN_LIMIT:=128}"
: "${DOH_MAX_BODY_BYTES:=4096}"
: "${HEALTH_PATH:=/health}"
: "${DOH_PATH:=/dns-query}"
: "${CACHE_SIZE:=2048}"
: "${CACHE_DUMP_FILE:=/var/cache/mosdns/cache.dump}"
: "${CACHE_DUMP_INTERVAL:=3300}"
: "${MAX_QPS:=15}"
: "${HAGEZI_UPSTREAM:=rotate}"
: "${UPSTREAM_IDLE_TIMEOUT:=30}"
: "${UPSTREAM_MAX_CONNS:=2}"
: "${SERVER_TIMEOUT:=8}"
: "${UPSTREAM_MODE:=doh-only}"
: "${HEALTH_TIMEOUT_MS:=1200}"
: "${HEALTH_CHECK:=true}"
: "${HEALTH_EWMA_ALPHA:=0.35}"
: "${HEALTH_FAILURE_PENALTY_MS:=1500}"
: "${HEALTH_SWITCH_MARGIN_PCT:=0.20}"
: "${HEALTH_SWITCH_MARGIN_MS:=25}"
: "${HEALTH_STATE_FILE:=/tmp/mosdns-upstream-state.tsv}"
: "${HEALTH_INTERVAL:=300}"
: "${HEALTH_FAILS_TO_SWITCH:=2}"
: "${HEALTH_RESTART_COOLDOWN:=900}"
: "${GOMEMLIMIT:=320MiB}"
: "${GOMAXPROCS:=1}"
: "${DOH_IDLE_TIMEOUT:=120}"
: "${UPSTREAM_0_IP:=188.34.161.210}"
: "${UPSTREAM_1_IP:=159.69.155.94}"
: "${UPSTREAM_2_IP:=95.217.163.17}"

validate_uint() {
  case "$2" in
    ''|*[!0-9]*) echo "Invalid $1: $2" >&2; exit 1 ;;
  esac
}

validate_float01() {
  awk -v v="$2" 'BEGIN { exit !(v > 0 && v <= 1) }' 2>/dev/null || {
    echo "Invalid $1: $2 (must be >0 and <=1)" >&2
    exit 1
  }
}

validate_uint PORT "$PORT"
validate_uint MOSDNS_BACKEND_PORT "$MOSDNS_BACKEND_PORT"
validate_uint IP_CONN_LIMIT "$IP_CONN_LIMIT"
validate_uint DOH_RATE_BURST "$DOH_RATE_BURST"
validate_uint DOH_RATE_MAX_IPS "$DOH_RATE_MAX_IPS"
validate_uint GLOBAL_RATE_BURST "$GLOBAL_RATE_BURST"
validate_uint GLOBAL_CONN_LIMIT "$GLOBAL_CONN_LIMIT"
validate_uint DOH_MAX_BODY_BYTES "$DOH_MAX_BODY_BYTES"
validate_uint CACHE_SIZE "$CACHE_SIZE"
validate_uint CACHE_DUMP_INTERVAL "$CACHE_DUMP_INTERVAL"
validate_uint MAX_QPS "$MAX_QPS"
validate_uint UPSTREAM_IDLE_TIMEOUT "$UPSTREAM_IDLE_TIMEOUT"
validate_uint UPSTREAM_MAX_CONNS "$UPSTREAM_MAX_CONNS"
validate_uint SERVER_TIMEOUT "$SERVER_TIMEOUT"
validate_uint HEALTH_TIMEOUT_MS "$HEALTH_TIMEOUT_MS"
validate_uint DOH_IDLE_TIMEOUT "$DOH_IDLE_TIMEOUT"
validate_uint HEALTH_FAILURE_PENALTY_MS "$HEALTH_FAILURE_PENALTY_MS"
validate_uint HEALTH_SWITCH_MARGIN_MS "$HEALTH_SWITCH_MARGIN_MS"
validate_uint HEALTH_INTERVAL "$HEALTH_INTERVAL"
validate_uint HEALTH_FAILS_TO_SWITCH "$HEALTH_FAILS_TO_SWITCH"
validate_uint HEALTH_RESTART_COOLDOWN "$HEALTH_RESTART_COOLDOWN"
validate_float01 HEALTH_EWMA_ALPHA "$HEALTH_EWMA_ALPHA"
validate_float01 HEALTH_SWITCH_MARGIN_PCT "$HEALTH_SWITCH_MARGIN_PCT"

case "$DOH_RATE_LIMIT" in *[!0-9.]*|"") echo "Invalid DOH_RATE_LIMIT: $DOH_RATE_LIMIT" >&2; exit 1 ;; esac
case "$GLOBAL_RATE_LIMIT" in *[!0-9.]*|"") echo "Invalid GLOBAL_RATE_LIMIT: $GLOBAL_RATE_LIMIT" >&2; exit 1 ;; esac

[ "$PORT" -gt 0 ] || { echo "PORT must be > 0" >&2; exit 1; }
[ "$MOSDNS_BACKEND_PORT" -gt 0 ] || { echo "MOSDNS_BACKEND_PORT must be > 0" >&2; exit 1; }
[ "$IP_CONN_LIMIT" -ge 0 ] || { echo "IP_CONN_LIMIT must be >= 0" >&2; exit 1; }
[ "$DOH_RATE_BURST" -gt 0 ] || { echo "DOH_RATE_BURST must be > 0" >&2; exit 1; }
[ "$DOH_RATE_MAX_IPS" -gt 0 ] || { echo "DOH_RATE_MAX_IPS must be > 0" >&2; exit 1; }
[ "$GLOBAL_RATE_BURST" -gt 0 ] || { echo "GLOBAL_RATE_BURST must be > 0" >&2; exit 1; }
[ "$GLOBAL_CONN_LIMIT" -gt 0 ] || { echo "GLOBAL_CONN_LIMIT must be > 0" >&2; exit 1; }
[ "$DOH_MAX_BODY_BYTES" -ge 512 ] || { echo "DOH_MAX_BODY_BYTES must be >= 512" >&2; exit 1; }
[ "$CACHE_SIZE" -gt 0 ] || { echo "CACHE_SIZE must be > 0" >&2; exit 1; }
[ "$MAX_QPS" -gt 0 ] || { echo "MAX_QPS must be > 0" >&2; exit 1; }
[ "$SERVER_TIMEOUT" -gt 0 ] || { echo "SERVER_TIMEOUT must be > 0" >&2; exit 1; }
[ "$UPSTREAM_MAX_CONNS" -gt 0 ] || { echo "UPSTREAM_MAX_CONNS must be > 0" >&2; exit 1; }
[ "$HEALTH_INTERVAL" -ge 30 ] || { echo "HEALTH_INTERVAL must be >= 30" >&2; exit 1; }
[ "$HEALTH_FAILS_TO_SWITCH" -gt 0 ] || { echo "HEALTH_FAILS_TO_SWITCH must be > 0" >&2; exit 1; }
[ "$HEALTH_RESTART_COOLDOWN" -ge "$HEALTH_INTERVAL" ] || { echo "HEALTH_RESTART_COOLDOWN must be >= HEALTH_INTERVAL" >&2; exit 1; }

case "$DOH_PATH" in /*) ;; *) echo "DOH_PATH must start with /" >&2; exit 1 ;; esac
case "$HEALTH_PATH" in /*) ;; *) echo "HEALTH_PATH must start with /" >&2; exit 1 ;; esac
case "$HAGEZI_UPSTREAM" in rotate|random|https://*) ;; *) echo "HAGEZI_UPSTREAM must be 'rotate', 'random', or an https:// endpoint" >&2; exit 1 ;; esac
case "$UPSTREAM_MODE" in doh-only) ;; *) echo "UPSTREAM_MODE must be doh-only" >&2; exit 1 ;; esac

UPSTREAM_0="https://root.hagezi.org/dns-query"
UPSTREAM_1="https://wurzn.hagezi.org/dns-query"
UPSTREAM_2="https://juuri.hagezi.org/dns-query"

# Hard fail if this file is ever changed to permit plain DNS upstreams.
case "$UPSTREAM_0 $UPSTREAM_1 $UPSTREAM_2" in
  *http://*|*udp://*|*tcp://*) echo "ERROR: plain-DNS upstream blocked (HTTPS DoH only)" >&2; exit 1 ;;
esac

probe_and_score() {
  [ "$HEALTH_CHECK" = "true" ] || return 1
  command -v mosdns-probe >/dev/null 2>&1 || return 1
  export HEALTH_TIMEOUT_MS HEALTH_EWMA_ALPHA HEALTH_FAILURE_PENALTY_MS HEALTH_SWITCH_MARGIN_PCT HEALTH_SWITCH_MARGIN_MS HEALTH_STATE_FILE HAGEZI_UPSTREAM
  probe_output=$(mosdns-probe "$@" 2>/dev/null || true)
  [ -n "$probe_output" ] || return 1

  ORDER_0=$(printf '%s\n' "$probe_output" | sed -n '1p' | cut -f2)
  ORDER_1=$(printf '%s\n' "$probe_output" | sed -n '2p' | cut -f2)
  ORDER_2=$(printf '%s\n' "$probe_output" | sed -n '3p' | cut -f2)
  [ -n "$ORDER_0" ] && [ -n "$ORDER_1" ] && [ -n "$ORDER_2" ] || return 1

  printf '%s\n' "$probe_output" |
    awk -F '\t' '{ printf "  %s: raw=%sms ok=%s score=%s ewma=%sms failures=%s\n", $2,$3,$4,$5,$6,$7 }'
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

upstream_ip_for_url() {
  case "$1" in
    "$UPSTREAM_0") printf '%s' "$UPSTREAM_0_IP" ;;
    "$UPSTREAM_1") printf '%s' "$UPSTREAM_1_IP" ;;
    "$UPSTREAM_2") printf '%s' "$UPSTREAM_2_IP" ;;
    *) printf '' ;;
  esac
}

sed_escape_replacement() { printf '%s' "$1" | sed 's/[\\&|]/\\&/g'; }

DOH_PATH_ESCAPED=$(sed_escape_replacement "$DOH_PATH")
HEALTH_PATH_ESCAPED=$(sed_escape_replacement "$HEALTH_PATH")
CACHE_SIZE_ESCAPED=$(sed_escape_replacement "$CACHE_SIZE")
CACHE_DUMP_FILE_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_FILE")
CACHE_DUMP_INTERVAL_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_INTERVAL")
MAX_QPS_ESCAPED=$(sed_escape_replacement "$MAX_QPS")
MOSDNS_BACKEND_PORT_ESCAPED=$(sed_escape_replacement "$MOSDNS_BACKEND_PORT")
UPSTREAM_IDLE_TIMEOUT_ESCAPED=$(sed_escape_replacement "$UPSTREAM_IDLE_TIMEOUT")
UPSTREAM_MAX_CONNS_ESCAPED=$(sed_escape_replacement "$UPSTREAM_MAX_CONNS")
DOH_IDLE_TIMEOUT_ESCAPED=$(sed_escape_replacement "$DOH_IDLE_TIMEOUT")
SERVER_TIMEOUT_ESCAPED=$(sed_escape_replacement "$SERVER_TIMEOUT")
CACHE_DIR=$(dirname "$CACHE_DUMP_FILE")
mkdir -p "$CACHE_DIR"
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy 2>/dev/null || true
export GOMEMLIMIT GOMAXPROCS

TEMPLATE=/etc/mosdns/config.yaml
RUNTIME_CONFIG=/tmp/mosdns-config.yaml
MOSDNS_PID=""
PROXY_PID=""

cleanup() {
  trap - TERM INT EXIT
  if [ -n "${PROXY_PID:-}" ] && kill -0 "$PROXY_PID" 2>/dev/null; then
    kill -TERM "$PROXY_PID" 2>/dev/null || true
  fi
  if [ -n "${MOSDNS_PID:-}" ] && kill -0 "$MOSDNS_PID" 2>/dev/null; then
    echo "Stopping MosDNS..."
    kill -TERM "$MOSDNS_PID" 2>/dev/null || true
  fi
  [ -n "${PROXY_PID:-}" ] && wait "$PROXY_PID" 2>/dev/null || true

  i=0
  if [ -n "${MOSDNS_PID:-}" ]; then
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

printf '%s\n' '=== MosDNS runtime ==='
mosdns version
printf '%s\n' '======================'
echo "Build: Koyeb tiny-instance profile; MosDNS v4.5.3; public DoH GET/POST; strict DoH-only upstreams; bounded warm cache; adaptive startup health ordering"
echo "Upstream mode: ${HAGEZI_UPSTREAM}"
echo "Sequential failover: enabled"
echo "Plain DNS listener: disabled"
echo "Health scoring: ${HEALTH_CHECK}, probe timeout ${HEALTH_TIMEOUT_MS}ms"
echo "Upstream idle timeout: ${UPSTREAM_IDLE_TIMEOUT}s, max conns: ${UPSTREAM_MAX_CONNS}"
echo "Server timeout: ${SERVER_TIMEOUT}s"
echo "Warm cache: ${CACHE_DUMP_FILE}, snapshot every ${CACHE_DUMP_INTERVAL}s"
echo "Runtime limits: GOMAXPROCS=${GOMAXPROCS}, GOMEMLIMIT=${GOMEMLIMIT}"
echo "DoH endpoint: ${DOH_PATH}"
echo "Anti-abuse: per-IP ${DOH_RATE_LIMIT}/s burst ${DOH_RATE_BURST}, global ${GLOBAL_RATE_LIMIT}/s burst ${GLOBAL_RATE_BURST}, global connections ${GLOBAL_CONN_LIMIT}, body <= ${DOH_MAX_BODY_BYTES}B"

echo "Selecting upstream order..."
select_order

U0_ESCAPED=$(sed_escape_replacement "$ORDER_0")
U1_ESCAPED=$(sed_escape_replacement "$ORDER_1")
U2_ESCAPED=$(sed_escape_replacement "$ORDER_2")
ORDER_0_IP=$(upstream_ip_for_url "$ORDER_0")
ORDER_1_IP=$(upstream_ip_for_url "$ORDER_1")
ORDER_2_IP=$(upstream_ip_for_url "$ORDER_2")
U0_IP_ESCAPED=$(sed_escape_replacement "$ORDER_0_IP")
U1_IP_ESCAPED=$(sed_escape_replacement "$ORDER_1_IP")
U2_IP_ESCAPED=$(sed_escape_replacement "$ORDER_2_IP")

sed \
  -e "s|__SERVER_TIMEOUT__|${SERVER_TIMEOUT_ESCAPED}|g" \
  -e "s|__MOSDNS_BACKEND_PORT__|${MOSDNS_BACKEND_PORT_ESCAPED}|g" \
  -e "s|__DOH_PATH__|${DOH_PATH_ESCAPED}|g" \
  -e "s|__CACHE_SIZE__|${CACHE_SIZE_ESCAPED}|g" \
  -e "s|__CACHE_DUMP_FILE__|${CACHE_DUMP_FILE_ESCAPED}|g" \
  -e "s|__CACHE_DUMP_INTERVAL__|${CACHE_DUMP_INTERVAL_ESCAPED}|g" \
  -e "s|__MAX_QPS__|${MAX_QPS_ESCAPED}|g" \
  -e "s|__UPSTREAM_IDLE_TIMEOUT__|${UPSTREAM_IDLE_TIMEOUT_ESCAPED}|g" \
  -e "s|__UPSTREAM_MAX_CONNS__|${UPSTREAM_MAX_CONNS_ESCAPED}|g" \
  -e "s|__UPSTREAM_0__|${U0_ESCAPED}|g" \
  -e "s|__UPSTREAM_1__|${U1_ESCAPED}|g" \
  -e "s|__UPSTREAM_2__|${U2_ESCAPED}|g" \
  -e "s|__UPSTREAM_0_IP__|${U0_IP_ESCAPED}|g" \
  -e "s|__UPSTREAM_1_IP__|${U1_IP_ESCAPED}|g" \
  -e "s|__UPSTREAM_2_IP__|${U2_IP_ESCAPED}|g" \
  -e "s|__DOH_IDLE_TIMEOUT__|${DOH_IDLE_TIMEOUT_ESCAPED}|g" \
  "$TEMPLATE" > "$RUNTIME_CONFIG"

# Custom HAGEZI_UPSTREAM endpoints do not have a pinned IP in this image.
sed -i '/^[[:space:]]*dial_addr:[[:space:]]*$/d' "$RUNTIME_CONFIG"

if grep -Eq '(__[A-Z0-9_]+__)|BACKEND_[0-9]+|PORT_PLACEHOLDER|BACKEND_PORT_PLACEHOLDER|PATH_PLACEHOLDER' "$RUNTIME_CONFIG"; then
  echo "ERROR: unresolved placeholder in generated MosDNS config:" >&2
  grep -nE '(__[A-Z0-9_]+__)|BACKEND_[0-9]+|PORT_PLACEHOLDER|BACKEND_PORT_PLACEHOLDER|PATH_PLACEHOLDER' "$RUNTIME_CONFIG" >&2 || true
  exit 1
fi

echo "Failover order:"
echo "  1. ${ORDER_0}"
echo "  2. ${ORDER_1}"
echo "  3. ${ORDER_2}"

echo "Starting DoH anti-abuse proxy on :${PORT} -> 127.0.0.1:${MOSDNS_BACKEND_PORT}"
LISTEN_ADDR=":${PORT}" \
BACKEND_ADDR="127.0.0.1:${MOSDNS_BACKEND_PORT}" \
IP_CONN_LIMIT="${IP_CONN_LIMIT}" \
DOH_RATE_LIMIT="${DOH_RATE_LIMIT}" \
DOH_RATE_BURST="${DOH_RATE_BURST}" \
DOH_RATE_MAX_IPS="${DOH_RATE_MAX_IPS}" \
GLOBAL_RATE_LIMIT="${GLOBAL_RATE_LIMIT}" \
GLOBAL_RATE_BURST="${GLOBAL_RATE_BURST}" \
GLOBAL_CONN_LIMIT="${GLOBAL_CONN_LIMIT}" \
DOH_MAX_BODY_BYTES="${DOH_MAX_BODY_BYTES}" \
HEALTH_PATH="${HEALTH_PATH}" \
DOH_PATH="${DOH_PATH}" \
GOMAXPROCS="${GOMAXPROCS}" \
ip-conn-proxy &
PROXY_PID=$!

echo "Starting MosDNS..."
mosdns start -c "$RUNTIME_CONFIG" &
MOSDNS_PID=$!

# Lightweight runtime health loop:
# - probes the three upstreams only every HEALTH_INTERVAL seconds
# - never restarts for a single transient failure
# - restarts MosDNS only when the active upstream is repeatedly bad or a materially
#   better healthy upstream appears, preserving the warm cache on disk
HEALTH_LOOP_PID=""
health_loop() {
  [ "$HEALTH_CHECK" = "true" ] || return 0
  failures=0
  last_restart=0
  while :; do
    sleep "$HEALTH_INTERVAL" || exit 0

    now=$(date +%s)
    [ $((now - last_restart)) -ge "$HEALTH_RESTART_COOLDOWN" ] || continue

    tmp_probe=$(mktemp /tmp/mosdns-probe.XXXXXX) || continue
    if ! probe_and_score "$UPSTREAM_0" "$UPSTREAM_1" "$UPSTREAM_2" >"$tmp_probe" 2>/dev/null; then
      rm -f "$tmp_probe"
      continue
    fi

    current="${ORDER_0:-$UPSTREAM_0}"
    best=$(sed -n 's/^  \([^:]*\):.*/\1/p' "$tmp_probe" | sed -n '1p')
    rm -f "$tmp_probe"
    [ -n "$best" ] || continue

    current_ok=0
    current_failures=0
    best_score=999999
    current_score=999999
    # Re-run the compact probe output once and parse tab-separated data from the
    # state-producing helper; this costs only three DoH probes per interval.
    raw=$(mosdns-probe "$UPSTREAM_0" "$UPSTREAM_1" "$UPSTREAM_2" 2>/dev/null || true)
    [ -n "$raw" ] || continue
    current_line=$(printf '%s\n' "$raw" | awk -F '\t' -v u="$current" '$2==u {print; exit}')
    best_line=$(printf '%s\n' "$raw" | sed -n '1p')
    current_ok=$(printf '%s\n' "$current_line" | awk -F '\t' '{print ($4=="true") ? 1 : 0}')
    current_failures=$(printf '%s\n' "$current_line" | awk -F '\t' '{print $7+0}')
    current_score=$(printf '%s\n' "$current_line" | awk -F '\t' '{print $5+0}')
    best=$(printf '%s\n' "$best_line" | cut -f2)
    best_score=$(printf '%s\n' "$best_line" | cut -f5)

    if [ "$current_ok" -ne 1 ]; then
      failures=$((failures + 1))
    else
      failures=0
    fi

    switch=0
    if [ "$current" != "$best" ]; then
      if [ "$current_ok" -ne 1 ] && [ "$failures" -ge "$HEALTH_FAILS_TO_SWITCH" ]; then
        switch=1
      elif [ "$current_score" -gt 0 ] && [ "$best_score" -gt 0 ]; then
        improvement=$(( (current_score - best_score) * 100 ))
        if [ "$improvement" -ge $((current_score * 30)) ] || [ $((current_score - best_score)) -ge 100 ]; then
          switch=1
        fi
      fi
    fi

    if [ "$switch" -eq 1 ]; then
      echo "Health supervisor: switching active upstream ${current} -> ${best}" >&2
      ORDER_0="$best"
      case "$best" in
        "$UPSTREAM_0") ORDER_1="$UPSTREAM_1"; ORDER_2="$UPSTREAM_2" ;;
        "$UPSTREAM_1") ORDER_1="$UPSTREAM_0"; ORDER_2="$UPSTREAM_2" ;;
        *) ORDER_1="$UPSTREAM_0"; ORDER_2="$UPSTREAM_1" ;;
      esac
      U0_ESCAPED=$(sed_escape_replacement "$ORDER_0")
      U1_ESCAPED=$(sed_escape_replacement "$ORDER_1")
      U2_ESCAPED=$(sed_escape_replacement "$ORDER_2")
      ORDER_0_IP=$(upstream_ip_for_url "$ORDER_0")
      ORDER_1_IP=$(upstream_ip_for_url "$ORDER_1")
      ORDER_2_IP=$(upstream_ip_for_url "$ORDER_2")
      U0_IP_ESCAPED=$(sed_escape_replacement "$ORDER_0_IP")
      U1_IP_ESCAPED=$(sed_escape_replacement "$ORDER_1_IP")
      U2_IP_ESCAPED=$(sed_escape_replacement "$ORDER_2_IP")
      sed \
        -e "s|__SERVER_TIMEOUT__|${SERVER_TIMEOUT_ESCAPED}|g" \
        -e "s|__MOSDNS_BACKEND_PORT__|${MOSDNS_BACKEND_PORT_ESCAPED}|g" \
        -e "s|__DOH_PATH__|${DOH_PATH_ESCAPED}|g" \
        -e "s|__CACHE_SIZE__|${CACHE_SIZE_ESCAPED}|g" \
        -e "s|__CACHE_DUMP_FILE__|${CACHE_DUMP_FILE_ESCAPED}|g" \
        -e "s|__CACHE_DUMP_INTERVAL__|${CACHE_DUMP_INTERVAL_ESCAPED}|g" \
        -e "s|__MAX_QPS__|${MAX_QPS_ESCAPED}|g" \
        -e "s|__UPSTREAM_IDLE_TIMEOUT__|${UPSTREAM_IDLE_TIMEOUT_ESCAPED}|g" \
        -e "s|__UPSTREAM_MAX_CONNS__|${UPSTREAM_MAX_CONNS_ESCAPED}|g" \
        -e "s|__UPSTREAM_0__|${U0_ESCAPED}|g" \
        -e "s|__UPSTREAM_1__|${U1_ESCAPED}|g" \
        -e "s|__UPSTREAM_2__|${U2_ESCAPED}|g" \
        -e "s|__UPSTREAM_0_IP__|${U0_IP_ESCAPED}|g" \
        -e "s|__UPSTREAM_1_IP__|${U1_IP_ESCAPED}|g" \
        -e "s|__UPSTREAM_2_IP__|${U2_IP_ESCAPED}|g" \
        -e "s|__DOH_IDLE_TIMEOUT__|${DOH_IDLE_TIMEOUT_ESCAPED}|g" \
        "$TEMPLATE" > "$RUNTIME_CONFIG"
      oldpid="$MOSDNS_PID"
      mosdns start -c "$RUNTIME_CONFIG" &
      newpid=$!
      sleep 1
      if kill -0 "$newpid" 2>/dev/null; then
        kill -TERM "$oldpid" 2>/dev/null || true
        MOSDNS_PID="$newpid"
        last_restart=$(date +%s)
        failures=0
      else
        echo "Health supervisor: new MosDNS config failed to start; keeping current process" >&2
      fi
    fi
  done
}
health_loop &
HEALTH_LOOP_PID=$!

while :; do
  if ! kill -0 "$MOSDNS_PID" 2>/dev/null; then
    echo "MosDNS exited; restarting instance via Koyeb" >&2
    exit 1
  fi
  if ! kill -0 "$PROXY_PID" 2>/dev/null; then
    echo "DoH proxy exited; restarting instance via Koyeb" >&2
    exit 1
  fi
  sleep 2
done
