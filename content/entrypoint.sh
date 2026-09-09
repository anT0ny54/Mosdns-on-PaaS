#!/bin/sh
set -eu

CONFIG=/etc/mosdns/config.yaml
PORT="${PORT:-8080}"
DOH_PATH="${DOH_PATH:-/dns-query}"

case "$DOH_PATH" in
  /*) ;;
  *) DOH_PATH="/$DOH_PATH" ;;
esac

# Escape values before inserting them into YAML.
ESCAPED_PORT=$(printf '%s' "$PORT" | sed 's/[\\&|]/\\&/g')
ESCAPED_PATH=$(printf '%s' "$DOH_PATH" | sed 's/[\\&|]/\\&/g')

sed -i "s|0.0.0.0:8080|0.0.0.0:${ESCAPED_PORT}|; s|path: /dns-query|path: ${ESCAPED_PATH}|" "$CONFIG"

exec mosdns start -d /etc/mosdns
