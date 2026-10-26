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
cp "$ROOT/check-config.sh" "$ROOT/mosdns.yaml" "$ROOT/main.go" "$ROOT/Dockerfile" "$ROOT/go.mod" "$TMP/"
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
# MosDNS v4.5.3 clamps threshold to stat_length during initialization.
write_case("valid-threshold-clamped-by-mosdns", lambda c, cache, seq, fb: fb.update(stat_length=5, threshold=10))
write_case("unknown-fallback-key", lambda c, cache, seq, fb: fb.update(typo_option=True))
write_case("negative-stat-length", lambda c, cache, seq, fb: fb.update(stat_length=-1))
write_case("string-stat-length", lambda c, cache, seq, fb: fb.update(stat_length="20"))
write_case("string-threshold", lambda c, cache, seq, fb: fb.update(threshold="5"))
write_case("string-cache-boolean", lambda c, cache, seq, fb: cache.update(cache_everything="true"))
write_case("extra-sequence-step", lambda c, cache, seq, fb: seq.append("fallback_hagezi"))
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
expect_fail() {
    name=$1
    if sh "$TMP/check-config.sh" "$TMP/$name.yaml" >"$TMP/$name.log" 2>&1; then
        echo "test-check-config: expected $name to fail, but it passed" >&2
        exit 1
    fi
    echo "PASS config validator rejects $name"
}

expect_ok valid-fallback-options
expect_ok valid-disabled-normal-fallback
expect_ok valid-threshold-clamped-by-mosdns
expect_fail unknown-fallback-key
expect_fail negative-stat-length
expect_fail string-stat-length
expect_fail string-threshold
expect_fail string-cache-boolean
expect_fail extra-sequence-step
