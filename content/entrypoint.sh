#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${DOH_PATH:=/dns-query}"
: "${CACHE_SIZE:=8192}"
: "${CACHE_DUMP_FILE:=/var/cache/mosdns/cache.dump}"
: "${CACHE_DUMP_INTERVAL:=900}"
: "${MAX_QPS:=20}"
: "${HAGEZI_UPSTREAM:=rotate}"
: "${ROTATE_INTERVAL:=1800}"
: "${GOMEMLIMIT:=384MiB}"

validate_uint() {
  case "$2" in
    ''|*[!0-9]*) echo "Invalid $1: $2" >&2; exit 1 ;;
  esac
}

validate_uint PORT "$PORT"
validate_uint CACHE_SIZE "$CACHE_SIZE"
validate_uint CACHE_DUMP_INTERVAL "$CACHE_DUMP_INTERVAL"
validate_uint MAX_QPS "$MAX_QPS"
validate_uint ROTATE_INTERVAL "$ROTATE_INTERVAL"

[ "$PORT" -gt 0 ] || { echo "PORT must be > 0" >&2; exit 1; }
[ "$CACHE_SIZE" -gt 0 ] || { echo "CACHE_SIZE must be > 0" >&2; exit 1; }
[ "$MAX_QPS" -gt 0 ] || { echo "MAX_QPS must be > 0" >&2; exit 1; }
[ "$ROTATE_INTERVAL" -ge 60 ] || { echo "ROTATE_INTERVAL must be at least 60 seconds" >&2; exit 1; }

case "$DOH_PATH" in
  /*) ;;
  *) echo "DOH_PATH must start with /" >&2; exit 1 ;;
esac

case "$HAGEZI_UPSTREAM" in
  rotate|random|https://*) ;;
  *) echo "HAGEZI_UPSTREAM must be 'rotate', 'random', or an https:// endpoint" >&2; exit 1 ;;
esac

# Fixed upstreams. Failover always follows the selected endpoint in cyclic order.
UPSTREAM_0="https://root.hagezi.org/dns-query"
UPSTREAM_1="https://wurzn.hagezi.org/dns-query"
UPSTREAM_2="https://juuri.hagezi.org/dns-query"
UPSTREAM_INDEX=0

select_order() {
  case "$HAGEZI_UPSTREAM" in
    rotate)
      selected=$UPSTREAM_INDEX
      UPSTREAM_INDEX=$(( (UPSTREAM_INDEX + 1) % 3 ))
      ;;
    random)
      selected=$(( $(od -An -N2 -tu2 /dev/urandom) % 3 ))
      ;;
    *)
      ORDER_0="$HAGEZI_UPSTREAM"
      ORDER_1="$UPSTREAM_0"
      ORDER_2="$UPSTREAM_1"
      HAGEZI_ACTIVE="$HAGEZI_UPSTREAM"
      return
      ;;
  esac

  case "$selected" in
    0)
      ORDER_0="$UPSTREAM_0"; ORDER_1="$UPSTREAM_1"; ORDER_2="$UPSTREAM_2"
      ;;
    1)
      ORDER_0="$UPSTREAM_1"; ORDER_1="$UPSTREAM_2"; ORDER_2="$UPSTREAM_0"
      ;;
    2)
      ORDER_0="$UPSTREAM_2"; ORDER_1="$UPSTREAM_0"; ORDER_2="$UPSTREAM_1"
      ;;
  esac
  HAGEZI_ACTIVE="$ORDER_0"
}

sed_escape_replacement() {
  printf '%s' "$1" | sed 's/[\\&|]/\\&/g'
}

PORT_ESCAPED=$(sed_escape_replacement "$PORT")
DOH_PATH_ESCAPED=$(sed_escape_replacement "$DOH_PATH")
CACHE_SIZE_ESCAPED=$(sed_escape_replacement "$CACHE_SIZE")
CACHE_DUMP_FILE_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_FILE")
CACHE_DUMP_INTERVAL_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_INTERVAL")
MAX_QPS_ESCAPED=$(sed_escape_replacement "$MAX_QPS")

CACHE_DIR=$(dirname "$CACHE_DUMP_FILE")
mkdir -p "$CACHE_DIR" 2>/dev/null || {
  echo "Warning: unable to create cache directory $CACHE_DIR" >&2
}

export GOMEMLIMIT

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
echo "Upstream mode: ${HAGEZI_UPSTREAM}"
echo "Failover: sequential (selected -> next -> next)"
echo "Rotation: every ${ROTATE_INTERVAL}s"
echo "Warm cache: ${CACHE_DUMP_FILE}, snapshot every ${CACHE_DUMP_INTERVAL}s"

while :; do
  # This function mutates UPSTREAM_0/1/2 so the selected server is always first.
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

  # A fixed endpoint is still protected by the two built-in fallbacks.
  # Rotation/random modes restart gracefully so the cache snapshot is kept.
  if [ "$HAGEZI_UPSTREAM" != "rotate" ] && [ "$HAGEZI_UPSTREAM" != "random" ]; then
    wait "$MOSDNS_PID"
    status=$?
    MOSDNS_PID=
    exit "$status"
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
