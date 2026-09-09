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

for pair in \
  "PORT:${PORT}" "CACHE_SIZE:${CACHE_SIZE}" \
  "CACHE_DUMP_INTERVAL:${CACHE_DUMP_INTERVAL}" "MAX_QPS:${MAX_QPS}" \
  "ROTATE_INTERVAL:${ROTATE_INTERVAL}"; do
  key=${pair%%:*}; val=${pair#*:}
  case "$val" in ''|*[!0-9]*) echo "Invalid ${key}: ${val}" >&2; exit 1 ;; esac
done

[ "$PORT" -gt 0 ] || { echo "PORT must be > 0" >&2; exit 1; }
[ "$CACHE_SIZE" -gt 0 ] || { echo "CACHE_SIZE must be > 0" >&2; exit 1; }
[ "$MAX_QPS" -gt 0 ] || { echo "MAX_QPS must be > 0" >&2; exit 1; }
[ "$ROTATE_INTERVAL" -ge 60 ] || { echo "ROTATE_INTERVAL must be at least 60 seconds" >&2; exit 1; }
case "$DOH_PATH" in /*) ;; *) echo "DOH_PATH must start with /" >&2; exit 1 ;; esac

case "$HAGEZI_UPSTREAM" in
  rotate|random|https://*) ;;
  *) echo "HAGEZI_UPSTREAM must be 'rotate', 'random', or use https://" >&2; exit 1 ;;
esac

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
mkdir -p "$CACHE_DIR" 2>/dev/null || echo "Warning: unable to create cache directory $CACHE_DIR" >&2
export GOMEMLIMIT="${GOMEMLIMIT:-400MiB}"

# Three fixed HaGeZi endpoints. "rotate" is deterministic round-robin:
# every rotation advances to the next endpoint and never selects the same
# endpoint twice in a row. This avoids random repeats while retaining the
# user's requirement to rotate among all three servers.
UPSTREAM_0="https://root.hagezi.org/dns-query"
UPSTREAM_1="https://wurzn.hagezi.org/dns-query"
UPSTREAM_2="https://juuri.hagezi.org/dns-query"
UPSTREAM_INDEX=0

select_hagezi() {
  if [ "$HAGEZI_UPSTREAM" = "rotate" ]; then
    case "$UPSTREAM_INDEX" in
      0) HAGEZI_ACTIVE="$UPSTREAM_0" ;;
      1) HAGEZI_ACTIVE="$UPSTREAM_1" ;;
      *) HAGEZI_ACTIVE="$UPSTREAM_2" ;;
    esac
    UPSTREAM_INDEX=$(( (UPSTREAM_INDEX + 1) % 3 ))
    return
  fi

  if [ "$HAGEZI_UPSTREAM" = "random" ]; then
    case "$(( $(od -An -N2 -tu2 /dev/urandom) % 3 ))" in
      0) HAGEZI_ACTIVE="$UPSTREAM_0" ;;
      1) HAGEZI_ACTIVE="$UPSTREAM_1" ;;
      2) HAGEZI_ACTIVE="$UPSTREAM_2" ;;
    esac
    return
  fi

  HAGEZI_ACTIVE="$HAGEZI_UPSTREAM"
}

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

# Avoid logging the full DoH path or other runtime secrets. The endpoint is
# intentionally reported only by host for easier operational diagnostics.
echo "=== MosDNS runtime ==="
mosdns version
echo "======================"
echo "Upstream mode: ${HAGEZI_UPSTREAM}"
echo "Rotation: every ${ROTATE_INTERVAL}s"
echo "Warm cache: ${CACHE_DUMP_FILE}, snapshot every ${CACHE_DUMP_INTERVAL}s"

after_start=0
while :; do
  select_hagezi

  UPSTREAM_ESCAPED=$(sed_escape_replacement "$HAGEZI_ACTIVE")
  sed \
    -e "s|PORT_PLACEHOLDER|${PORT_ESCAPED}|g" \
    -e "s|PATH_PLACEHOLDER|${DOH_PATH_ESCAPED}|g" \
    -e "s|CACHE_SIZE_PLACEHOLDER|${CACHE_SIZE_ESCAPED}|g" \
    -e "s|CACHE_DUMP_FILE_PLACEHOLDER|${CACHE_DUMP_FILE_ESCAPED}|g" \
    -e "s|CACHE_DUMP_INTERVAL_PLACEHOLDER|${CACHE_DUMP_INTERVAL_ESCAPED}|g" \
    -e "s|MAX_QPS_PLACEHOLDER|${MAX_QPS_ESCAPED}|g" \
    -e "s|HAGEZI_UPSTREAM_PLACEHOLDER|${UPSTREAM_ESCAPED}|g" \
    "$TEMPLATE" > "$RUNTIME_CONFIG"

  echo "Active upstream: ${HAGEZI_ACTIVE}"

  mosdns start -c "$RUNTIME_CONFIG" &
  MOSDNS_PID=$!

  # A fixed upstream means no rotation. Keep MosDNS in the foreground from
  # the supervisor's point of view and let its exit status determine health.
  if [ "$HAGEZI_UPSTREAM" != "rotate" ] && [ "$HAGEZI_UPSTREAM" != "random" ]; then
    wait "$MOSDNS_PID"
    MOSDNS_PID=
    exit 0
  fi

  elapsed=0
  while kill -0 "$MOSDNS_PID" 2>/dev/null && [ "$elapsed" -lt "$ROTATE_INTERVAL" ]; do
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if kill -0 "$MOSDNS_PID" 2>/dev/null; then
    echo "Rotation interval reached; gracefully switching upstream."
    # MosDNS's cache backend writes a final snapshot from Close(). Because the
    # warm cache is bounded, the next process can restore it without losing the
    # hot cache between rotations.
    kill -TERM "$MOSDNS_PID" 2>/dev/null || true
    wait "$MOSDNS_PID" 2>/dev/null || true
    MOSDNS_PID=

    # A fixed upstream is not supposed to rotate.
    [ "$HAGEZI_UPSTREAM" = "rotate" ] || [ "$HAGEZI_UPSTREAM" = "random" ] || break
  else
    wait "$MOSDNS_PID" 2>/dev/null || true
    MOSDNS_PID=
    echo "MosDNS exited unexpectedly." >&2
    exit 1
  fi

done

exit 0
