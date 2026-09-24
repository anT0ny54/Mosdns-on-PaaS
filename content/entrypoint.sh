#!/bin/sh
set -eu
umask 077

: "${PORT:=8080}"
: "${MOSDNS_BACKEND_PORT:=18080}"
: "${IP_CONN_LIMIT:=16}"
: "${DOH_RATE_LIMIT:=5}"
: "${DOH_RATE_BURST:=100}"
: "${DOH_RATE_MAX_IPS:=4096}"
: "${GLOBAL_RATE_LIMIT:=80}"
: "${GLOBAL_RATE_BURST:=160}"
: "${HEALTH_RATE_LIMIT:=2}"
: "${HEALTH_RATE_BURST:=4}"
: "${GLOBAL_HEALTH_RATE_LIMIT:=10}"
: "${GLOBAL_HEALTH_RATE_BURST:=20}"
: "${GLOBAL_CONN_LIMIT:=256}"
: "${DOH_MAX_BODY_BYTES:=4096}"
: "${HEALTH_PATH:=/health}"
: "${DOH_PATH:=/dns-query}"
: "${CACHE_SIZE:=8192}"
: "${CACHE_MAX_ENTRY_BYTES:=8192}"
: "${CACHE_DUMP_FILE:=/var/cache/mosdns/cache.dump}"
: "${CACHE_DUMP_INTERVAL:=3300}"
: "${HAGEZI_UPSTREAM:=rotate}"
: "${UPSTREAM_IDLE_TIMEOUT:=60}"
: "${UPSTREAM_MAX_CONNS:=8}"
: "${SERVER_TIMEOUT:=10}"
: "${HEALTH_TIMEOUT_MS:=2000}"
: "${HEALTH_BACKEND_TIMEOUT_MS:=1000}"
: "${HEALTH_CHECK:=true}"
: "${HEALTH_EWMA_ALPHA:=0.35}"
: "${HEALTH_FAILURE_PENALTY_MS:=1500}"
: "${HEALTH_SWITCH_MARGIN_PCT:=0.20}"
: "${HEALTH_SWITCH_MARGIN_MS:=25}"
: "${HEALTH_STATE_FILE:=/tmp/mosdns-upstream-state.tsv}"
: "${HEALTH_INTERVAL:=60}"
: "${HEALTH_FAILS_TO_SWITCH:=2}"
: "${HEALTH_RESTART_COOLDOWN:=120}"
: "${GOMEMLIMIT:=288MiB}"
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

validate_uint_max() {
  validate_uint "$1" "$2"
  [ "$2" -le "$3" ] || { echo "Invalid $1: $2 (must be <= $3)" >&2; exit 1; }
}

validate_port() {
  validate_uint "$1" "$2"
  [ "$2" -ge 1 ] && [ "$2" -le 65535 ] || { echo "Invalid $1: $2 (must be 1-65535)" >&2; exit 1; }
}

validate_printable_ascii() {
  LC_ALL=C; export LC_ALL
  case "$2" in
    ''|*[![:print:]]*) echo "Invalid $1: unsupported character(s)" >&2; exit 1 ;;
  esac
}

validate_ipv4() {
  awk -v ip="$2" 'BEGIN {
    if (split(ip, octet, ".") != 4) exit 1
    for (i = 1; i <= 4; i++) {
      if (octet[i] !~ /^(0|[1-9][0-9]*)$/ || octet[i] + 0 > 255) exit 1
    }
  }' 2>/dev/null || {
    echo "Invalid $1: $2 (must be an IPv4 address)" >&2
    exit 1
  }
}

validate_float01() {
  awk -v v="$2" 'BEGIN {
    if (v !~ /^[+]?(0|[0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$/) exit 1
    exit !(v > 0 && v <= 1)
  }' 2>/dev/null || {
    echo "Invalid $1: $2 (must be >0 and <=1)" >&2
    exit 1
  }
}

validate_nonnegative_float() {
  awk -v v="$2" 'BEGIN {
    if (v !~ /^[+-]?(0|[0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$/) exit 1
    exit !(v >= 0 && v <= 1000000000)
  }' 2>/dev/null || {
    echo "Invalid $1: $2 (must be a number from 0 to 1000000000)" >&2
    exit 1
  }
}

validate_port PORT "$PORT"
validate_port MOSDNS_BACKEND_PORT "$MOSDNS_BACKEND_PORT"
[ "$PORT" -ge 1024 ] || { echo "Invalid PORT: $PORT (must be 1024-65535 for the non-root runtime user)" >&2; exit 1; }
[ "$MOSDNS_BACKEND_PORT" -ge 1024 ] || { echo "Invalid MOSDNS_BACKEND_PORT: $MOSDNS_BACKEND_PORT (must be 1024-65535 for the non-root runtime user)" >&2; exit 1; }
validate_uint_max IP_CONN_LIMIT "$IP_CONN_LIMIT" 65535
validate_uint_max DOH_RATE_BURST "$DOH_RATE_BURST" 1000000
validate_uint_max DOH_RATE_MAX_IPS "$DOH_RATE_MAX_IPS" 4096
validate_uint_max GLOBAL_RATE_BURST "$GLOBAL_RATE_BURST" 1000000
validate_uint_max HEALTH_RATE_BURST "$HEALTH_RATE_BURST" 1000000
validate_uint_max GLOBAL_HEALTH_RATE_BURST "$GLOBAL_HEALTH_RATE_BURST" 1000000
validate_uint_max GLOBAL_CONN_LIMIT "$GLOBAL_CONN_LIMIT" 65535
validate_uint_max DOH_MAX_BODY_BYTES "$DOH_MAX_BODY_BYTES" 65535
validate_uint_max CACHE_SIZE "$CACHE_SIZE" 8192
validate_uint_max CACHE_MAX_ENTRY_BYTES "$CACHE_MAX_ENTRY_BYTES" 65535
validate_uint_max CACHE_DUMP_INTERVAL "$CACHE_DUMP_INTERVAL" 604800
validate_uint_max UPSTREAM_IDLE_TIMEOUT "$UPSTREAM_IDLE_TIMEOUT" 3600
validate_uint_max UPSTREAM_MAX_CONNS "$UPSTREAM_MAX_CONNS" 64
validate_uint_max SERVER_TIMEOUT "$SERVER_TIMEOUT" 10
validate_uint_max HEALTH_TIMEOUT_MS "$HEALTH_TIMEOUT_MS" 600000
validate_uint_max HEALTH_BACKEND_TIMEOUT_MS "$HEALTH_BACKEND_TIMEOUT_MS" 600000
validate_uint_max DOH_IDLE_TIMEOUT "$DOH_IDLE_TIMEOUT" 3600
validate_uint_max GOMAXPROCS "$GOMAXPROCS" 2
# MosDNS v4.5.3 treats idle_timeout <= 0 as its 10s default. The proxy keeps
# its pooled backend connection at least 5s shorter, so explicit values 1-5s
# cannot satisfy the required safety margin and are rejected. Zero is allowed
# as the MosDNS-default sentinel.
case "$DOH_IDLE_TIMEOUT" in
  0) ;;
  1|2|3|4|5) echo "Invalid DOH_IDLE_TIMEOUT: $DOH_IDLE_TIMEOUT (must be 0 or >= 6; 0 uses MosDNS's 10s default)" >&2; exit 1 ;;
esac
validate_uint_max HEALTH_FAILURE_PENALTY_MS "$HEALTH_FAILURE_PENALTY_MS" 3600000
validate_uint_max HEALTH_SWITCH_MARGIN_MS "$HEALTH_SWITCH_MARGIN_MS" 3600000
validate_uint_max HEALTH_INTERVAL "$HEALTH_INTERVAL" 604800
validate_uint_max HEALTH_FAILS_TO_SWITCH "$HEALTH_FAILS_TO_SWITCH" 1000
validate_uint_max HEALTH_RESTART_COOLDOWN "$HEALTH_RESTART_COOLDOWN" 604800
validate_float01 HEALTH_EWMA_ALPHA "$HEALTH_EWMA_ALPHA"
validate_float01 HEALTH_SWITCH_MARGIN_PCT "$HEALTH_SWITCH_MARGIN_PCT"
# User-supplied text is substituted into the template one sed expression at a
# time, so a value that itself looks like a __PLACEHOLDER__ could be rewritten
# by a later expression. Reject that shape outright.
validate_no_placeholder() {
  if printf '%s' "$2" | grep -Eq '__[A-Z0-9_]+__'; then
    echo "Invalid $1: must not contain __UPPERCASE__ placeholder-like text" >&2
    exit 1
  fi
}

validate_printable_ascii DOH_PATH "$DOH_PATH"
validate_printable_ascii HEALTH_PATH "$HEALTH_PATH"
validate_printable_ascii CACHE_DUMP_FILE "$CACHE_DUMP_FILE"
validate_printable_ascii HEALTH_STATE_FILE "$HEALTH_STATE_FILE"
validate_printable_ascii GOMEMLIMIT "$GOMEMLIMIT"
validate_printable_ascii HAGEZI_UPSTREAM "$HAGEZI_UPSTREAM"
validate_no_placeholder DOH_PATH "$DOH_PATH"
validate_no_placeholder CACHE_DUMP_FILE "$CACHE_DUMP_FILE"
validate_no_placeholder HAGEZI_UPSTREAM "$HAGEZI_UPSTREAM"
validate_ipv4 UPSTREAM_0_IP "$UPSTREAM_0_IP"
validate_ipv4 UPSTREAM_1_IP "$UPSTREAM_1_IP"
validate_ipv4 UPSTREAM_2_IP "$UPSTREAM_2_IP"

validate_nonnegative_float DOH_RATE_LIMIT "$DOH_RATE_LIMIT"
validate_nonnegative_float GLOBAL_RATE_LIMIT "$GLOBAL_RATE_LIMIT"
validate_nonnegative_float HEALTH_RATE_LIMIT "$HEALTH_RATE_LIMIT"
validate_nonnegative_float GLOBAL_HEALTH_RATE_LIMIT "$GLOBAL_HEALTH_RATE_LIMIT"

[ "$DOH_RATE_BURST" -gt 0 ] || { echo "DOH_RATE_BURST must be > 0" >&2; exit 1; }
[ "$DOH_RATE_MAX_IPS" -gt 0 ] || { echo "DOH_RATE_MAX_IPS must be > 0" >&2; exit 1; }
[ "$GLOBAL_RATE_BURST" -gt 0 ] || { echo "GLOBAL_RATE_BURST must be > 0" >&2; exit 1; }
[ "$HEALTH_RATE_BURST" -gt 0 ] || { echo "HEALTH_RATE_BURST must be > 0" >&2; exit 1; }
[ "$GLOBAL_HEALTH_RATE_BURST" -gt 0 ] || { echo "GLOBAL_HEALTH_RATE_BURST must be > 0" >&2; exit 1; }
# GLOBAL_CONN_LIMIT=0 is a supported "unlimited" sentinel in ip-conn-proxy
# (see ip_conn_proxy.go), the same convention used by the rate-limit env vars
# below. Only reject a negative value here; validate_uint already did that.
[ "$DOH_MAX_BODY_BYTES" -ge 512 ] || { echo "DOH_MAX_BODY_BYTES must be >= 512" >&2; exit 1; }
[ "$CACHE_SIZE" -ge 1024 ] || { echo "CACHE_SIZE must be >= 1024" >&2; exit 1; }
[ "$CACHE_MAX_ENTRY_BYTES" -ge 512 ] || { echo "CACHE_MAX_ENTRY_BYTES must be >= 512" >&2; exit 1; }
[ "$SERVER_TIMEOUT" -gt 0 ] || { echo "SERVER_TIMEOUT must be > 0" >&2; exit 1; }
[ "$HEALTH_TIMEOUT_MS" -gt 0 ] || { echo "HEALTH_TIMEOUT_MS must be > 0" >&2; exit 1; }
[ "$HEALTH_BACKEND_TIMEOUT_MS" -gt 0 ] || { echo "HEALTH_BACKEND_TIMEOUT_MS must be > 0" >&2; exit 1; }
[ "$PORT" -ne "$MOSDNS_BACKEND_PORT" ] || { echo "PORT and MOSDNS_BACKEND_PORT must differ" >&2; exit 1; }
[ "$HEALTH_PATH" != "$DOH_PATH" ] || { echo "HEALTH_PATH and DOH_PATH must differ" >&2; exit 1; }
[ "$UPSTREAM_MAX_CONNS" -gt 0 ] || { echo "UPSTREAM_MAX_CONNS must be > 0" >&2; exit 1; }
[ "$HEALTH_INTERVAL" -ge 30 ] || { echo "HEALTH_INTERVAL must be >= 30" >&2; exit 1; }
[ "$HEALTH_FAILS_TO_SWITCH" -gt 0 ] || { echo "HEALTH_FAILS_TO_SWITCH must be > 0" >&2; exit 1; }
[ "$HEALTH_RESTART_COOLDOWN" -ge "$HEALTH_INTERVAL" ] || { echo "HEALTH_RESTART_COOLDOWN must be >= HEALTH_INTERVAL" >&2; exit 1; }
[ "$GOMAXPROCS" -gt 0 ] || { echo "GOMAXPROCS must be 1-2 for the 0.25 vCPU profile" >&2; exit 1; }

case "$DOH_PATH" in /*) ;; *) echo "DOH_PATH must start with /" >&2; exit 1 ;; esac
case "$HEALTH_PATH" in /*) ;; *) echo "HEALTH_PATH must start with /" >&2; exit 1 ;; esac
case "$DOH_PATH" in *'?'*|*'#'*|*' '*) echo "DOH_PATH must be a path without query, fragment, or spaces" >&2; exit 1 ;; esac
case "$HEALTH_PATH" in *'?'*|*'#'*|*' '*) echo "HEALTH_PATH must be a path without query, fragment, or spaces" >&2; exit 1 ;; esac
case "$HAGEZI_UPSTREAM" in rotate|random|https://*) ;; *) echo "HAGEZI_UPSTREAM must be 'rotate', 'random', or an https:// endpoint" >&2; exit 1 ;; esac
case "$HEALTH_CHECK" in true|false) ;; *) echo "HEALTH_CHECK must be true or false" >&2; exit 1 ;; esac
if [ "$HEALTH_CHECK" = "true" ] && ! command -v mosdns-probe >/dev/null 2>&1; then
  echo "ERROR: mosdns-probe binary not found while HEALTH_CHECK=true" >&2
  exit 1
fi

UPSTREAM_0="https://root.hagezi.org/dns-query"
UPSTREAM_1="https://wurzn.hagezi.org/dns-query"
UPSTREAM_2="https://juuri.hagezi.org/dns-query"

probe_upstreams() {
  [ "$HEALTH_CHECK" = "true" ] || return 1
  export HEALTH_TIMEOUT_MS HEALTH_EWMA_ALPHA HEALTH_FAILURE_PENALTY_MS HEALTH_SWITCH_MARGIN_PCT HEALTH_SWITCH_MARGIN_MS HEALTH_FAILS_TO_SWITCH HEALTH_STATE_FILE HEALTH_ACTIVE_UPSTREAM HAGEZI_UPSTREAM UPSTREAM_0_IP UPSTREAM_1_IP UPSTREAM_2_IP
  GOMEMLIMIT=32MiB GOMAXPROCS=1 mosdns-probe "$@" 2>/dev/null
}

# Static candidate order for the selected HAGEZI_UPSTREAM mode. Writes CAND_0..2
# only (never ORDER_*), so it is safe to call while a config is running.
set_candidate_order() {
  case "$HAGEZI_UPSTREAM" in
    rotate|random|"$UPSTREAM_0")
      CAND_0="$UPSTREAM_0"; CAND_1="$UPSTREAM_1"; CAND_2="$UPSTREAM_2"
      ;;
    "$UPSTREAM_1")
      CAND_0="$UPSTREAM_1"; CAND_1="$UPSTREAM_0"; CAND_2="$UPSTREAM_2"
      ;;
    "$UPSTREAM_2")
      CAND_0="$UPSTREAM_2"; CAND_1="$UPSTREAM_0"; CAND_2="$UPSTREAM_1"
      ;;
    *)
      # A fixed custom endpoint replaces the first built-in slot. Keep the
      # other two built-ins as fallbacks.
      CAND_0="$HAGEZI_UPSTREAM"; CAND_1="$UPSTREAM_1"; CAND_2="$UPSTREAM_2"
      ;;
  esac
}

probe_candidate_set() {
  set_candidate_order
  probe_upstreams "$CAND_0" "$CAND_1" "$CAND_2"
}

probe_and_score() {
  probe_output=$(probe_candidate_set || true)
  [ -n "$probe_output" ] || return 1

  ORDER_0=$(printf '%s\n' "$probe_output" | sed -n '1p' | cut -f2)
  ORDER_1=$(printf '%s\n' "$probe_output" | sed -n '2p' | cut -f2)
  ORDER_2=$(printf '%s\n' "$probe_output" | sed -n '3p' | cut -f2)
  [ -n "$ORDER_0" ] && [ -n "$ORDER_1" ] && [ -n "$ORDER_2" ] || return 1

  printf '%s\n' "$probe_output" |
    awk -F '\t' '{ printf "  %s: raw=%sms ok=%s score=%s ewma=%sms failures=%s\n", $2,$3,$4,$5,$6,$7 }'
}

set_default_order() {
  set_candidate_order
  ORDER_0="$CAND_0"
  ORDER_1="$CAND_1"
  ORDER_2="$CAND_2"
}

# A fixed HAGEZI_UPSTREAM (an https:// endpoint) names a preferred first upstream.
# rotate/random are the only modes where the probe alone decides the first slot.
is_fixed_mode() {
  case "$HAGEZI_UPSTREAM" in
    rotate|random) return 1 ;;
    *) return 0 ;;
  esac
}

# $1 = probe rows (tab-separated, as printed by mosdns-probe), $2 = upstream URL.
# Succeeds only when that upstream's row reports ok=true.
probe_row_ok() {
  printf '%s\n' "$1" | awk -F '\t' -v u="$2" '$2 == u && $4 == "true" { found = 1 } END { exit !found }'
}

# stdin = probe rows, $1 = preferred URL. Prints the URLs with the preferred one
# first and the others in their measured order.
preferred_first_urls() {
  awk -F '\t' -v p="$1" '$2 == p { print $2 } $2 != p { rest[++n] = $2 } END { for (i = 1; i <= n; i++) print rest[i] }'
}

# $1 = preferred URL, $2 = probe rows. Sets ORDER_0..2 to preferred-first order.
apply_preferred_first() {
  ordered=$(printf '%s\n' "$2" | preferred_first_urls "$1")
  o_0=$(printf '%s\n' "$ordered" | sed -n '1p')
  o_1=$(printf '%s\n' "$ordered" | sed -n '2p')
  o_2=$(printf '%s\n' "$ordered" | sed -n '3p')
  [ -n "$o_0" ] && [ -n "$o_1" ] && [ -n "$o_2" ] || return 0
  ORDER_0="$o_0"; ORDER_1="$o_1"; ORDER_2="$o_2"
}

select_order() {
  if ! probe_and_score; then
    if [ "$HEALTH_CHECK" = "true" ]; then
      echo "Startup probe returned no usable result; using the default upstream order" >&2
    fi
    set_default_order
    return 0
  fi
  # With a fixed endpoint the probe may only reorder the fallbacks; the preferred
  # endpoint is demoted at startup only if it actually failed its probe. (A
  # custom endpoint has no pinned IP, so a plain latency sort would almost always
  # push it behind the pinned built-ins.)
  if is_fixed_mode; then
    set_candidate_order
    if probe_row_ok "$probe_output" "$CAND_0"; then
      apply_preferred_first "$CAND_0" "$probe_output"
    else
      echo "Preferred upstream ${CAND_0} failed its startup probe; using measured order" >&2
    fi
  fi
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
yaml_single_quote() { escaped=$(printf '%s' "$1" | sed "s/'/''/g"); printf "'%s'" "$escaped"; }

CACHE_SIZE_ESCAPED=$(sed_escape_replacement "$CACHE_SIZE")
CACHE_MAX_ENTRY_BYTES_ESCAPED=$(sed_escape_replacement "$CACHE_MAX_ENTRY_BYTES")
CACHE_DUMP_FILE_ESCAPED=$(yaml_single_quote "$CACHE_DUMP_FILE" | sed 's/[\\&|]/\\&/g')
CACHE_DUMP_INTERVAL_ESCAPED=$(sed_escape_replacement "$CACHE_DUMP_INTERVAL")
MOSDNS_BACKEND_PORT_ESCAPED=$(sed_escape_replacement "$MOSDNS_BACKEND_PORT")
UPSTREAM_IDLE_TIMEOUT_ESCAPED=$(sed_escape_replacement "$UPSTREAM_IDLE_TIMEOUT")
UPSTREAM_MAX_CONNS_ESCAPED=$(sed_escape_replacement "$UPSTREAM_MAX_CONNS")
DOH_IDLE_TIMEOUT_ESCAPED=$(sed_escape_replacement "$DOH_IDLE_TIMEOUT")
SERVER_TIMEOUT_ESCAPED=$(sed_escape_replacement "$SERVER_TIMEOUT")
CACHE_DIR=$(dirname "$CACHE_DUMP_FILE")
mkdir -p "$CACHE_DIR" 2>/dev/null || echo "WARNING: cannot create cache directory ${CACHE_DIR}; warm cache snapshots may be skipped" >&2
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy 2>/dev/null || true
export GOMEMLIMIT GOMAXPROCS

TEMPLATE=/etc/mosdns/config.yaml
RUNTIME_CONFIG=/tmp/mosdns-config.yaml
MOSDNS_PID=""
PROXY_PID=""

# Renders $TEMPLATE -> $RUNTIME_CONFIG using the current ORDER_0/ORDER_1/ORDER_2
# upstream selection. Used both at startup and by the runtime health
# supervisor, so the dial_addr cleanup and placeholder check can never drift
# out of sync between the two call sites the way two hand-copied blocks can.
# Renders to a temp file first and only replaces RUNTIME_CONFIG on success,
# so a bad render never clobbers a config that is already working.
render_config() {
  ORDER_0_IP=$(upstream_ip_for_url "$ORDER_0")
  ORDER_1_IP=$(upstream_ip_for_url "$ORDER_1")
  ORDER_2_IP=$(upstream_ip_for_url "$ORDER_2")
  U0_ESCAPED=$(yaml_single_quote "$ORDER_0" | sed 's/[\\&|]/\\&/g')
  U1_ESCAPED=$(yaml_single_quote "$ORDER_1" | sed 's/[\\&|]/\\&/g')
  U2_ESCAPED=$(yaml_single_quote "$ORDER_2" | sed 's/[\\&|]/\\&/g')
  U0_IP_ESCAPED=$(yaml_single_quote "$ORDER_0_IP" | sed 's/[\\&|]/\\&/g')
  U1_IP_ESCAPED=$(yaml_single_quote "$ORDER_1_IP" | sed 's/[\\&|]/\\&/g')
  U2_IP_ESCAPED=$(yaml_single_quote "$ORDER_2_IP" | sed 's/[\\&|]/\\&/g')

  candidate="${RUNTIME_CONFIG}.new"
  sed \
    -e "s|__SERVER_TIMEOUT__|${SERVER_TIMEOUT_ESCAPED}|g" \
    -e "s|__MOSDNS_BACKEND_PORT__|${MOSDNS_BACKEND_PORT_ESCAPED}|g" \
    -e "s|__DOH_PATH__|$(yaml_single_quote "$DOH_PATH" | sed 's/[\\&|]/\\&/g')|" \
    -e "s|__CACHE_SIZE__|${CACHE_SIZE_ESCAPED}|g" \
    -e "s|__CACHE_MAX_ENTRY_BYTES__|${CACHE_MAX_ENTRY_BYTES_ESCAPED}|g" \
    -e "s|__CACHE_DUMP_FILE__|${CACHE_DUMP_FILE_ESCAPED}|g" \
    -e "s|__CACHE_DUMP_INTERVAL__|${CACHE_DUMP_INTERVAL_ESCAPED}|g" \
    -e "s|__UPSTREAM_IDLE_TIMEOUT__|${UPSTREAM_IDLE_TIMEOUT_ESCAPED}|g" \
    -e "s|__UPSTREAM_MAX_CONNS__|${UPSTREAM_MAX_CONNS_ESCAPED}|g" \
    -e "s|__UPSTREAM_0__|${U0_ESCAPED}|g" \
    -e "s|__UPSTREAM_1__|${U1_ESCAPED}|g" \
    -e "s|__UPSTREAM_2__|${U2_ESCAPED}|g" \
    -e "s|__UPSTREAM_0_IP__|${U0_IP_ESCAPED}|g" \
    -e "s|__UPSTREAM_1_IP__|${U1_IP_ESCAPED}|g" \
    -e "s|__UPSTREAM_2_IP__|${U2_IP_ESCAPED}|g" \
    -e "s|__DOH_IDLE_TIMEOUT__|${DOH_IDLE_TIMEOUT_ESCAPED}|g" \
    "$TEMPLATE" > "$candidate" || {
      # `set -e` is ignored while render_config runs inside `if !`, so a failed
      # or partial render must be rejected explicitly instead of being installed.
      echo "ERROR: failed to render MosDNS config from ${TEMPLATE}" >&2
      rm -f "$candidate"
      return 1
    }

  # Custom HAGEZI_UPSTREAM endpoints do not have a pinned IP in this image.
  # Remove empty quoted dial_addr values for custom endpoints that have no
  # pinned IP. Quoting the value keeps environment overrides inside the YAML
  # scalar instead of letting YAML punctuation alter the generated config.
  sed -i "/^[[:space:]]*dial_addr:[[:space:]]*''[[:space:]]*$/d" "$candidate" || {
    echo "ERROR: failed to post-process generated MosDNS config" >&2
    rm -f "$candidate"
    return 1
  }

  # Every __PLACEHOLDER__ that exists in the template must have been consumed.
  # (Checking the template's own names avoids false hits on user-supplied values.)
  unresolved=""
  for placeholder in $(grep -o '__[A-Z0-9_]*__' "$TEMPLATE" | sort -u); do
    if grep -qF -- "$placeholder" "$candidate"; then
      unresolved="${unresolved} ${placeholder}"
    fi
  done
  if [ -n "$unresolved" ]; then
    echo "ERROR: unresolved placeholder(s) in generated MosDNS config:${unresolved}" >&2
    rm -f "$candidate"
    return 1
  fi

  mv "$candidate" "$RUNTIME_CONFIG"
}

child_running() {
  pid="$1"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  # BusyBox ps (used by the Alpine runtime image) does not support
  # `ps -o stat= -p PID`. Read the kernel-maintained process state instead.
  if grep -q '^State:[[:space:]]*Z' "/proc/$pid/status" 2>/dev/null; then
    return 1
  fi
  return 0
}

stop_child() {
  pid="$1"
  label="$2"
  [ -n "$pid" ] || return 0
  if ! child_running "$pid"; then
    wait "$pid" 2>/dev/null || true
    return 0
  fi

  echo "Stopping ${label}..."
  kill -TERM "$pid" 2>/dev/null || true

  # Do not use kill -0 as the completion test: an exited child remains a zombie
  # until the parent calls wait, so kill -0 can report it as alive for the entire
  # grace period. A watchdog preserves the hard stop without adding that delay.
  (
    sleep 10
    kill -KILL "$pid" 2>/dev/null || true
  ) &
  watchdog_pid=$!
  wait "$pid" 2>/dev/null || true
  kill "$watchdog_pid" 2>/dev/null || true
  wait "$watchdog_pid" 2>/dev/null || true
}

cleanup() {
  trap - TERM INT EXIT
  stop_child "${PROXY_PID:-}" "DoH proxy"
  stop_child "${MOSDNS_PID:-}" "MosDNS"
}
# TERM/INT must end the script. A handler that merely returns would resume the
# supervisor loop, which then reports the just-stopped MosDNS as a crash and
# exits 1. Exiting from the signal traps runs cleanup via the EXIT trap.
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

printf '%s\n' '=== MosDNS runtime ==='
RELEASE_VERSION=$(cat /etc/mosdns/VERSION)
echo "Release: ${RELEASE_VERSION}"
mosdns version
printf '%s\n' '======================'
echo "Build: PaaS tiny-instance profile (512 MiB / 0.25 vCPU); MosDNS v4.5.3; public DoH GET/POST; strict DoH-only upstreams; bounded warm cache; adaptive startup health ordering"
echo "Upstream mode: ${HAGEZI_UPSTREAM}"
echo "Sequential failover: enabled"
echo "Plain DNS listener: disabled"
echo "Health scoring: ${HEALTH_CHECK}, probe timeout ${HEALTH_TIMEOUT_MS}ms"
echo "Upstream idle timeout: ${UPSTREAM_IDLE_TIMEOUT}s, max conns: ${UPSTREAM_MAX_CONNS}"
if [ "$DOH_IDLE_TIMEOUT" -eq 0 ]; then
  echo "DoH listener idle timeout: MosDNS default (10s); proxy backend pool timeout: 5s"
elif [ "$DOH_IDLE_TIMEOUT" -le 95 ]; then
  echo "DoH listener idle timeout: ${DOH_IDLE_TIMEOUT}s; proxy backend pool timeout: $((DOH_IDLE_TIMEOUT - 5))s"
else
  echo "DoH listener idle timeout: ${DOH_IDLE_TIMEOUT}s; proxy backend pool timeout: 90s (capped)"
fi
echo "Server timeout: ${SERVER_TIMEOUT}s"
echo "Warm cache: ${CACHE_DUMP_FILE}, snapshot every ${CACHE_DUMP_INTERVAL}s, max cached response ${CACHE_MAX_ENTRY_BYTES}B"
echo "Runtime limits: GOMAXPROCS=${GOMAXPROCS}, GOMEMLIMIT=${GOMEMLIMIT}"
echo "DoH endpoint: ${DOH_PATH}"
echo "Anti-abuse: per-IP conn ${IP_CONN_LIMIT}, per-IP ${DOH_RATE_LIMIT}/s burst ${DOH_RATE_BURST}, fixed source state <= ${DOH_RATE_MAX_IPS}, global ${GLOBAL_RATE_LIMIT}/s burst ${GLOBAL_RATE_BURST}, global connections ${GLOBAL_CONN_LIMIT}, body <= ${DOH_MAX_BODY_BYTES}B"

echo "Selecting upstream order..."
unset HEALTH_ACTIVE_UPSTREAM 2>/dev/null || true
select_order

if ! render_config; then
  echo "ERROR: failed to render initial MosDNS config; aborting startup" >&2
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
HEALTH_RATE_LIMIT="${HEALTH_RATE_LIMIT}" \
HEALTH_RATE_BURST="${HEALTH_RATE_BURST}" \
GLOBAL_HEALTH_RATE_LIMIT="${GLOBAL_HEALTH_RATE_LIMIT}" \
GLOBAL_HEALTH_RATE_BURST="${GLOBAL_HEALTH_RATE_BURST}" \
HEALTH_BACKEND_TIMEOUT_MS="${HEALTH_BACKEND_TIMEOUT_MS}" \
DOH_PATH="${DOH_PATH}" \
DOH_IDLE_TIMEOUT="${DOH_IDLE_TIMEOUT}" \
GOMEMLIMIT=80MiB \
GOMAXPROCS=1 \
ip-conn-proxy &
PROXY_PID=$!

echo "Starting MosDNS..."
mosdns start -c "$RUNTIME_CONFIG" &
MOSDNS_PID=$!

# Lightweight runtime health supervisor:
# - probes the active candidate set only once per HEALTH_INTERVAL
# - never restarts for a single transient failure
# - applies the probe helper's configured hysteresis before changing order
# - runs in the main shell so MOSDNS_PID and ORDER_* updates remain authoritative
# - stops the current MosDNS process before starting the replacement, avoiding
#   a second listener binding to the same 127.0.0.1:${MOSDNS_BACKEND_PORT}
health_check_once() {
  [ "$HEALTH_CHECK" = "true" ] || return 0

  now=$(date +%s)
  current="${ORDER_0:-$UPSTREAM_0}"
  raw=$(HEALTH_ACTIVE_UPSTREAM="$current" probe_candidate_set || true)
  [ -n "$raw" ] || return 0

  current_line=$(printf '%s\n' "$raw" | awk -F '\t' -v u="$current" '$2==u {print; exit}')
  best_line=$(printf '%s\n' "$raw" | sed -n '1p')
  [ -n "$current_line" ] && [ -n "$best_line" ] || return 0

  current_ok=$(printf '%s\n' "$current_line" | awk -F '\t' '{print ($4=="true") ? 1 : 0}')
  current_failures=$(printf '%s\n' "$current_line" | awk -F '\t' '{print $7+0}')
  best=$(printf '%s\n' "$best_line" | cut -f2)
  best_ok=$(printf '%s\n' "$best_line" | awk -F '\t' '{print ($4=="true") ? 1 : 0}')

  # Fixed mode: the preferred endpoint is "best" whenever it is healthy, so a
  # supervisor restart can never demote it for latency, and a fallback that took
  # over during an outage hands back to it once it recovers.
  pref_first=""
  if is_fixed_mode; then
    set_candidate_order
    if probe_row_ok "$raw" "$CAND_0"; then
      pref_first="$CAND_0"
      best="$CAND_0"
      best_ok=1
    fi
  fi

  # Switch only to a healthy best candidate, and only when the active upstream
  # is healthy-but-beaten (upstream_probe.go already applied the margins) or has
  # failed HEALTH_FAILS_TO_SWITCH times in a row.
  switch=0
  if [ "$current" != "$best" ] && [ "$best_ok" -eq 1 ] &&
     { [ "$current_ok" -eq 1 ] || [ "$current_failures" -ge "$HEALTH_FAILS_TO_SWITCH" ]; }; then
    switch=1
  fi

  if [ "$switch" -ne 1 ]; then
    return 0
  fi

  # Restart cooldown protects against healthy-performance churn, but it must
  # never prevent evacuation from an actively failed upstream.
  if [ "$current_ok" -eq 1 ] && [ $((now - last_restart)) -lt "$HEALTH_RESTART_COOLDOWN" ]; then
    return 0
  fi

  # Adopt the probe's complete measured order (exactly like startup) so the
  # fallback positions are health-aware too, not a fixed static permutation.
  if [ -n "$pref_first" ]; then
    ordered=$(printf '%s\n' "$raw" | preferred_first_urls "$pref_first")
  else
    ordered=$(printf '%s\n' "$raw" | cut -f2)
  fi
  new_0=$(printf '%s\n' "$ordered" | sed -n '1p')
  new_1=$(printf '%s\n' "$ordered" | sed -n '2p')
  new_2=$(printf '%s\n' "$ordered" | sed -n '3p')
  [ -n "$new_0" ] && [ -n "$new_1" ] && [ -n "$new_2" ] || return 0

  echo "Health supervisor: switching active upstream ${current} -> ${best}" >&2
  prev_0="$ORDER_0"; prev_1="$ORDER_1"; prev_2="$ORDER_2"
  ORDER_0="$new_0"; ORDER_1="$new_1"; ORDER_2="$new_2"
  if ! render_config; then
    # Keep ORDER_* in sync with the config the running process actually uses.
    ORDER_0="$prev_0"; ORDER_1="$prev_1"; ORDER_2="$prev_2"
    echo "Health supervisor: failed to render switched config; keeping current process" >&2
    return 0
  fi

  oldpid="$MOSDNS_PID"
  stop_child "$oldpid" "MosDNS"
  MOSDNS_PID=""

  mosdns start -c "$RUNTIME_CONFIG" &
  MOSDNS_PID=$!
  sleep 1
  if child_running "$MOSDNS_PID"; then
    last_restart=$(date +%s)
  else
    echo "Health supervisor: replacement MosDNS failed to start; exiting for instance restart" >&2
    exit 1
  fi
}

last_health_check=$(date +%s)
last_restart=0
while :; do
  if ! child_running "$MOSDNS_PID"; then
    wait "$MOSDNS_PID" 2>/dev/null || true
    echo "MosDNS exited; restarting instance via platform" >&2
    exit 1
  fi
  if ! child_running "$PROXY_PID"; then
    wait "$PROXY_PID" 2>/dev/null || true
    echo "DoH proxy exited; restarting instance via platform" >&2
    exit 1
  fi

  if [ "$HEALTH_CHECK" = "true" ]; then
    now=$(date +%s)
    if [ $((now - last_health_check)) -ge "$HEALTH_INTERVAL" ]; then
      last_health_check="$now"
      health_check_once
    fi
  fi
  # Interruptible sleep: `wait` returns as soon as a trapped signal arrives.
  sleep 5 &
  wait $! || true
done
