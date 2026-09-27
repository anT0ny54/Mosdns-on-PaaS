#!/bin/sh
set -eu

CONFIG="${1:-mosdns.yaml}"

grep -q 'https://root.hagezi.org/dns-query' "$CONFIG"
grep -q '188.34.161.210' "$CONFIG"
grep -q 'https://wurzn.hagezi.org/dns-query' "$CONFIG"
grep -q '159.69.155.94' "$CONFIG"
grep -q 'https://juuri.hagezi.org/dns-query' "$CONFIG"
grep -q '95.217.163.17' "$CONFIG"
grep -q 'type: fast_forward' "$CONFIG"
grep -q 'type: http_server' "$CONFIG"
grep -q 'listen: "127.0.0.1:8081"' "$CONFIG"

echo "configuration references, fallback chain, and DoH listener: OK"
