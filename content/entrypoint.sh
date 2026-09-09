#!/bin/sh
set -eu

: "${PORT:=8080}"
: "${DOH_PATH:=/dns-query}"

# Generate the runtime listener from the Koyeb-provided PORT and
# optional custom DoH path without modifying the image at build time.
sed -i   -e "s|PORT_PLACEHOLDER|${PORT}|g"   -e "s|PATH_PLACEHOLDER|${DOH_PATH}|g"   /etc/mosdns/config.yaml

exec mosdns start -d /etc/mosdns
