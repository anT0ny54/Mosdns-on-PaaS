#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${DOH_PATH:=/dns-query}"
: "${CACHE_SIZE:=32768}"

case "${PORT}" in
  ''|*[!0-9]*) echo "Invalid PORT: ${PORT}" >&2; exit 1 ;;
esac
case "${CACHE_SIZE}" in
  ''|*[!0-9]*) echo "Invalid CACHE_SIZE: ${CACHE_SIZE}" >&2; exit 1 ;;
esac
case "${DOH_PATH}" in
  /*) ;;
  *) echo "DOH_PATH must start with /" >&2; exit 1 ;;
esac

sed_escape_replacement() {
  printf '%s' "$1" | sed 's/[\\&|]/\\&/g'
}

PORT_ESCAPED=$(sed_escape_replacement "${PORT}")
DOH_PATH_ESCAPED=$(sed_escape_replacement "${DOH_PATH}")
CACHE_SIZE_ESCAPED=$(sed_escape_replacement "${CACHE_SIZE}")

sed -i   -e "s|PORT_PLACEHOLDER|${PORT_ESCAPED}|g"   -e "s|PATH_PLACEHOLDER|${DOH_PATH_ESCAPED}|g"   -e "s|CACHE_SIZE_PLACEHOLDER|${CACHE_SIZE_ESCAPED}|g"   /etc/mosdns/config.yaml

echo "Starting MosDNS:"
mosdns version || true

exec mosdns start -d /etc/mosdns
