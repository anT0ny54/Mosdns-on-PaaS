#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PYTHON=$(command -v python3 || command -v python || true)
if [ -z "$PYTHON" ]; then
    echo "test-check-config: Python 3 with PyYAML is required" >&2
    exit 1
fi
if ! "$PYTHON" -c 'import sys, yaml; raise SystemExit(0 if sys.version_info >= (3, 8) else 1)' >/dev/null 2>&1; then
    echo "test-check-config: Python 3.8+ with PyYAML is required" >&2
    exit 1
fi
TMP=$(mktemp -d "${TMPDIR:-/tmp}/mosdns-config-check.XXXXXX")
cleanup() {
    rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

# Keep the cross-file checks active: check-config.sh discovers these files
# beside the config being tested.
cp "$ROOT/check-config.sh" "$ROOT/mosdns.yaml" "$ROOT/main.go" "$ROOT/Dockerfile" "$ROOT/go.mod" "$ROOT/VERSION" "$TMP/"
mkdir -p "$TMP/.github/workflows"
cp "$ROOT/.github/workflows/ci.yml" "$TMP/.github/workflows/ci.yml"

"$PYTHON" - "$TMP" <<'PYTHON'
import copy
import sys
from pathlib import Path
import yaml

tmp = Path(sys.argv[1])
base = yaml.safe_load((tmp / "mosdns.yaml").read_text(encoding="utf-8"))

def locations(config):
    plugins = {plugin["tag"]: plugin for plugin in config["plugins"]}
    sequence = plugins[config["servers"][0]["exec"]]["args"]["exec"]
    cache_index = next(
        i for i, item in enumerate(sequence)
        if isinstance(item, str) and item in plugins and plugins[item]["type"] == "cache"
    )
    return plugins[sequence[cache_index]]["args"], sequence, sequence[cache_index + 1]

def write_case(name, edit):
    config = copy.deepcopy(base)
    cache, sequence, fallback = locations(config)
    edit(config, cache, sequence, fallback)
    (tmp / f"{name}.yaml").write_text(
        yaml.safe_dump(config, sort_keys=False), encoding="utf-8"
    )

write_case("valid-fallback-options", lambda c, cache, seq, fb: fb.update(stat_length=20, threshold=5, always_standby=True))
write_case("valid-disabled-normal-fallback", lambda c, cache, seq, fb: fb.update(stat_length=0, threshold=0))

def valid_no_edns_filter(config, cache, sequence, fallback):
    sequence[1] = "_edns0_filter_no_edns0"
    cache["cache_everything"] = False

write_case("valid-no-edns-filter", valid_no_edns_filter)
# MosDNS v4.5.3 clamps threshold to stat_length during initialization.
write_case("valid-threshold-clamped-by-mosdns", lambda c, cache, seq, fb: fb.update(stat_length=5, threshold=10))
write_case("unknown-fallback-key", lambda c, cache, seq, fb: fb.update(typo_option=True))
write_case("negative-stat-length", lambda c, cache, seq, fb: fb.update(stat_length=-1))
write_case("string-stat-length", lambda c, cache, seq, fb: fb.update(stat_length="20"))
write_case("string-threshold", lambda c, cache, seq, fb: fb.update(threshold="5"))
write_case("string-cache-boolean", lambda c, cache, seq, fb: cache.update(cache_everything="true"))
write_case("extra-sequence-step", lambda c, cache, seq, fb: seq.append("fallback_hagezi"))
write_case("unknown-edns-filter", lambda c, cache, seq, fb: seq.insert(1, "_edns0_filter_typo"))

def set_malformed_upstream_url(config, cache, sequence, fallback):
    upstream = next(plugin for plugin in config["plugins"] if plugin["tag"] == "primary_fast")["args"]["upstream"]
    upstream[0]["addr"] = "https://[invalid/dns-query"

write_case("malformed-upstream-url", set_malformed_upstream_url)

def set_readiness_wrong_class(config, cache, sequence, fallback):
    readiness = next(plugin for plugin in config["plugins"] if plugin["tag"] == "readiness")
    readiness["args"]["rr"] = ["ready.doh-gateway.invalid. 0 CH A 127.0.0.1"]

write_case("readiness-wrong-class", set_readiness_wrong_class)
PYTHON

expect_ok() {
    name=$1
    if ! sh "$TMP/check-config.sh" "$TMP/$name.yaml" >"$TMP/$name.log" 2>&1; then
        cat "$TMP/$name.log" >&2
        echo "test-check-config: expected $name to pass" >&2
        exit 1
    fi
    echo "PASS config validator accepts $name"
}

# expect_fail NAME [DIAGNOSTIC]: the config must be rejected, without a Python
# traceback, and (when given) with the expected diagnostic.
expect_fail() {
    name=$1
    want=${2:-}
    if sh "$TMP/check-config.sh" "$TMP/$name.yaml" >"$TMP/$name.log" 2>&1; then
        echo "test-check-config: expected $name to fail, but it passed" >&2
        exit 1
    fi
    if grep -q "Traceback" "$TMP/$name.log" || { [ -n "$want" ] && ! grep -q "$want" "$TMP/$name.log"; }; then
        cat "$TMP/$name.log" >&2
        echo "test-check-config: $name was rejected for the wrong reason (want: ${want:-no traceback})" >&2
        exit 1
    fi
    echo "PASS config validator rejects $name"
}

expect_ok valid-fallback-options
expect_ok valid-disabled-normal-fallback
expect_ok valid-no-edns-filter
expect_ok valid-threshold-clamped-by-mosdns
expect_fail unknown-fallback-key "fallback contains unsupported key"
expect_fail negative-stat-length "stat_length must not be negative"
expect_fail string-stat-length "stat_length must be an integer"
expect_fail string-threshold "threshold must be an integer"
expect_fail string-cache-boolean "cache_everything must be true or false"
expect_fail extra-sequence-step "must contain exactly one primary/secondary fallback"
expect_fail unknown-edns-filter "uses unsupported EDNS filter"
expect_fail malformed-upstream-url "must be a valid upstream URL"
expect_fail readiness-wrong-class "IN A 127.0.0.1"

# Dockerfile cross-checks: each mutation must be rejected for its own reason.
cp "$TMP/Dockerfile" "$TMP/Dockerfile.orig"

reject_dockerfile() { # $1 = case name, $2 = sed script, $3 = expected diagnostic
    cp "$TMP/Dockerfile.orig" "$TMP/Dockerfile"
    sed "$2" "$TMP/Dockerfile.orig" > "$TMP/Dockerfile.mutated"
    mv "$TMP/Dockerfile.mutated" "$TMP/Dockerfile"
    if sh "$TMP/check-config.sh" "$TMP/mosdns.yaml" > "$TMP/$1.log" 2>&1; then
        echo "test-check-config: expected $1 to fail, but it passed" >&2
        exit 1
    fi
    if grep -q "Traceback" "$TMP/$1.log" || ! grep -q "$3" "$TMP/$1.log"; then
        cat "$TMP/$1.log" >&2
        echo "test-check-config: $1 was rejected for the wrong reason (want: $3)" >&2
        exit 1
    fi
    echo "PASS config validator rejects $1"
}

reject_dockerfile https-backend 's#MOSDNS_DOH_URL=http://#MOSDNS_DOH_URL=https://#' "does not match listener protocol"
reject_dockerfile invalid-backend-port 's#MOSDNS_DOH_URL=http://127.0.0.1:8081#MOSDNS_DOH_URL=http://127.0.0.1:bad#' "must be a valid URL"
reject_dockerfile mismatched-version 's/^ARG GATEWAY_VERSION=.*/ARG GATEWAY_VERSION=9.9.9/' "must equal the VERSION file"
