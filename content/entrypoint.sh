#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${DOH_PATH:=/dns-query}"
: "${CACHE_SIZE:=32768}"
: "${CACHE_DUMP_FILE:=/var/cache/mosdns/cache.dump}"
: "${CACHE_DUMP_INTERVAL:=300}"

# The cache dump is a native MosDNS v4.5.3 cache snapshot. It is intentionally
# optional/warm-start only: DNS continues to work if the file is missing,
# stale, corrupted, or the local disk is unavailable.
CACHE_DIR=$(dirname "${CACHE_DUMP_FILE}")
mkdir -p "${CACHE_DIR}"

sed -i \
  -e "s|PORT_PLACEHOLDER|${PORT}|g" \
  -e "s|PATH_PLACEHOLDER|${DOH_PATH}|g" \
  -e "s|CACHE_SIZE_PLACEHOLDER|${CACHE_SIZE}|g" \
  -e "s|CACHE_DUMP_FILE_PLACEHOLDER|${CACHE_DUMP_FILE}|g" \
  -e "s|CACHE_DUMP_INTERVAL_PLACEHOLDER|${CACHE_DUMP_INTERVAL}|g" \
  /etc/mosdns/config.yaml

exec mosdns start -d /etc/mosdns
