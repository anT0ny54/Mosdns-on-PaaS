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
write_case("duplicate-fallback-plugin", lambda c, cache, seq, fb: fb["primary"].append(fb["primary"][0]))
write_case("unhashable-fallback-tag", lambda c, cache, seq, fb: fb["primary"].__setitem__(0, []))
write_case("unused-plugin", lambda c, cache, seq, fb: c["plugins"].append({"tag": "unused_probe", "type": "arbitrary", "args": {"rr": []}}))
write_case("duplicate-local-plugin", lambda c, cache, seq, fb: seq.insert(1, seq[0]))
write_case("duplicate-edns-filter", lambda c, cache, seq, fb: seq.insert(2, seq[1]))
write_case("readiness-extra-token", lambda c, cache, seq, fb: next(p for p in c["plugins"] if p["tag"] == "readiness")["args"].update(rr=["ready.doh-gateway.invalid. 0 IN A 127.0.0.1 EXTRA"]))
write_case("readiness-duplicate-a", lambda c, cache, seq, fb: next(p for p in c["plugins"] if p["tag"] == "readiness")["args"].update(rr=["ready.doh-gateway.invalid. 0 IN A 127.0.0.1", "ready.doh-gateway.invalid. 0 IN A 127.0.0.1"]))

def set_malformed_upstream_url(config, cache, sequence, fallback):
    upstream = next(plugin for plugin in config["plugins"] if plugin["tag"] == "primary_fast")["args"]["upstream"]
    upstream[0]["addr"] = "https://[invalid/dns-query"

write_case("malformed-upstream-url", set_malformed_upstream_url)

def set_readiness_wrong_class(config, cache, sequence, fallback):
    readiness = next(plugin for plugin in config["plugins"] if plugin["tag"] == "readiness")
    readiness["args"]["rr"] = ["ready.doh-gateway.invalid. 0 CH A 127.0.0.1"]

write_case("readiness-wrong-class", set_readiness_wrong_class)
write_case("valid-api-without-http", lambda c, cache, seq, fb: c.update(api={}))

# Preserve a duplicated key in the original YAML text; safe_dump would erase it.
base_yaml_text = (tmp / "mosdns.yaml").read_text(encoding="utf-8")
timeout_line = "    timeout: 2\n"
if timeout_line not in base_yaml_text:
    raise SystemExit("test-check-config: could not find server timeout line for duplicate-key case")
(tmp / "duplicate-yaml-key.yaml").write_text(
    base_yaml_text.replace(timeout_line, timeout_line + "    timeout: 3\n", 1),
    encoding="utf-8",
)

def set_listener_userinfo(config, cache, sequence, fallback):
    config["servers"][0]["listeners"][0]["addr"] = "user:pass@127.0.0.1:8081"

write_case("listener-userinfo", set_listener_userinfo)

def set_listener_whitespace(config, cache, sequence, fallback):
    config["servers"][0]["listeners"][0]["addr"] = "127.0.0.1:\t8081"

write_case("listener-whitespace", set_listener_whitespace)

def set_primary_upstream_url(value):
    def edit(config, cache, sequence, fallback):
        upstream = next(plugin for plugin in config["plugins"] if plugin["tag"] == "primary_fast")["args"]["upstream"]
        upstream[0]["addr"] = value
    return edit

write_case("upstream-whitespace", set_primary_upstream_url("https://root.hagezi.org\n/dns-query"))
write_case("upstream-control-character", set_primary_upstream_url("https://root.hagezi.org/\u0090dns-query"))
write_case("upstream-userinfo", set_primary_upstream_url("https://user:pass@root.hagezi.org/dns-query"))
write_case("upstream-fragment", set_primary_upstream_url("https://root.hagezi.org/dns-query#fragment"))
write_case("upstream-empty-fragment", set_primary_upstream_url("https://root.hagezi.org/dns-query#"))
write_case("upstream-empty-port", set_primary_upstream_url("https://root.hagezi.org:/dns-query"))
write_case("upstream-invalid-port", set_primary_upstream_url("https://root.hagezi.org:not-a-port/dns-query"))
write_case("upstream-out-of-range-port", set_primary_upstream_url("https://root.hagezi.org:65536/dns-query"))
write_case("upstream-unsupported-ftp", set_primary_upstream_url("ftp://root.hagezi.org/dns-query"))
write_case("upstream-unsupported-http", set_primary_upstream_url("http://root.hagezi.org/dns-query"))
write_case("valid-uppercase-upstream-scheme", set_primary_upstream_url("HTTPS://root.hagezi.org/dns-query"))

def set_primary_upstream_options(config, addr=None, **options):
    upstream = next(plugin for plugin in config["plugins"] if plugin["tag"] == "primary_fast")["args"]["upstream"][0]
    if addr is not None:
        upstream["addr"] = addr
    upstream.update(options)

def set_udp_pipeline(config, cache, sequence, fallback):
    set_primary_upstream_options(config, "udp://root.hagezi.org", enable_pipeline=True)
    # UDP ignores idle_timeout too; keep this test focused on enable_pipeline.
    next(plugin for plugin in config["plugins"] if plugin["tag"] == "primary_fast")["args"]["upstream"][0].pop("idle_timeout", None)

write_case("upstream-udp-pipeline", set_udp_pipeline)
write_case("upstream-https-pipeline", lambda c, cache, seq, fb: set_primary_upstream_options(c, enable_pipeline=True))
write_case("upstream-udp-idle-timeout", lambda c, cache, seq, fb: set_primary_upstream_options(c, "udp://root.hagezi.org"))

def set_udp_without_connection_options(config, cache, sequence, fallback):
    set_primary_upstream_options(config, "udp://root.hagezi.org")
    upstream = next(plugin for plugin in config["plugins"] if plugin["tag"] == "primary_fast")["args"]["upstream"][0]
    upstream.pop("idle_timeout", None)

write_case("valid-udp-upstream", set_udp_without_connection_options)
write_case("valid-tcp-pipeline", lambda c, cache, seq, fb: set_primary_upstream_options(c, "tcp://root.hagezi.org:53", enable_pipeline=True))
write_case("valid-tls-pipeline", lambda c, cache, seq, fb: set_primary_upstream_options(c, "tls://root.hagezi.org:853", enable_pipeline=True))

def set_api_non_loopback(config, cache, sequence, fallback):
    config["api"] = {"http": "0.0.0.0:9091"}

write_case("api-non-loopback", set_api_non_loopback)
(tmp / "invalid-utf8.yaml").write_bytes(b"log:\n  level: error\n# invalid byte: \xff\n")
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
expect_ok valid-api-without-http
expect_ok valid-uppercase-upstream-scheme
expect_ok valid-udp-upstream
expect_ok valid-tcp-pipeline
expect_ok valid-tls-pipeline
expect_fail listener-userinfo "must be a host:port address without user-info"
expect_fail listener-whitespace "must not contain whitespace or control characters"
expect_fail upstream-whitespace "must not contain whitespace or control characters"
expect_fail upstream-control-character "must not contain whitespace or control characters"
expect_fail invalid-utf8 "must be valid UTF-8"
expect_fail upstream-userinfo "must not contain user-info"
expect_fail upstream-fragment "must not contain fragments"
expect_fail upstream-empty-fragment "must not contain fragments"
expect_fail upstream-empty-port "must not use an empty port"
expect_fail upstream-invalid-port "must contain a valid port"
expect_fail upstream-out-of-range-port "must contain a valid port"
expect_fail upstream-unsupported-ftp "uses unsupported upstream URL scheme 'ftp'"
expect_fail upstream-unsupported-http "uses unsupported upstream URL scheme 'http'"
expect_fail upstream-udp-pipeline "enable_pipeline is only supported for tcp:// and tls://"
expect_fail upstream-https-pipeline "enable_pipeline is only supported for tcp:// and tls://"
expect_fail upstream-udp-idle-timeout "idle_timeout is not supported for udp://"
expect_fail api-non-loopback "api.http must bind to loopback, not 0.0.0.0"
expect_fail unknown-fallback-key "fallback contains unsupported key"
expect_fail negative-stat-length "stat_length must not be negative"
expect_fail string-stat-length "stat_length must be an integer"
expect_fail string-threshold "threshold must be an integer"
expect_fail string-cache-boolean "cache_everything must be true or false"
expect_fail extra-sequence-step "must contain exactly one primary/secondary fallback"
expect_fail unknown-edns-filter "uses unsupported EDNS filter"
expect_fail duplicate-fallback-plugin "references plugin .* more than once"
expect_fail unhashable-fallback-tag "primary\[1\] must be a non-empty string"
expect_fail unused-plugin "unused plugin definition"
expect_fail duplicate-local-plugin "local-answer plugin .* more than once"
expect_fail duplicate-edns-filter "EDNS filter .* is repeated"
expect_fail readiness-extra-token "IN A 127.0.0.1"
expect_fail readiness-duplicate-a "exactly one arbitrary IN A record"
expect_fail duplicate-yaml-key "duplicate mapping key 'timeout'"
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

# URI schemes are case-insensitive and must be treated the same by the
# static checker and Go startup validation.
sed 's#MOSDNS_DOH_URL=http://#MOSDNS_DOH_URL=HTTP://#' "$TMP/Dockerfile.orig" > "$TMP/Dockerfile"
if ! sh "$TMP/check-config.sh" "$TMP/mosdns.yaml" > "$TMP/uppercase-backend-scheme.log" 2>&1; then
    cat "$TMP/uppercase-backend-scheme.log" >&2
    echo "test-check-config: uppercase HTTP backend scheme should be accepted" >&2
    exit 1
fi
echo "PASS config validator accepts uppercase HTTP backend scheme"

reject_dockerfile https-backend 's#MOSDNS_DOH_URL=http://#MOSDNS_DOH_URL=https://#' "does not match listener protocol"
reject_dockerfile invalid-backend-port 's#MOSDNS_DOH_URL=http://127.0.0.1:8081#MOSDNS_DOH_URL=http://127.0.0.1:bad#' "must be a valid URL"
reject_dockerfile backend-userinfo 's|MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query|MOSDNS_DOH_URL=http://user:pass@127.0.0.1:8081/dns-query|' "must not contain user-info"
reject_dockerfile backend-fragment 's|MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query|MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query#fragment|' "must not contain fragments"
reject_dockerfile backend-empty-fragment 's|MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query|MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query#|' "must not contain fragments"
reject_dockerfile backend-empty-port 's|MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query|MOSDNS_DOH_URL=http://127.0.0.1:/dns-query|' "must not use an empty port"
# A non-whitespace C1 control character is not matched by Python's \S
# class, so ensure the explicit raw-character check catches it too.
"$PYTHON" - "$TMP" <<'PYTHON'
import sys
from pathlib import Path
root = Path(sys.argv[1])
text = (root / "Dockerfile.orig").read_text(encoding="utf-8")
needle = "MOSDNS_DOH_URL=http://127.0.0.1:8081/dns-query"
if needle not in text:
    raise SystemExit("test-check-config: could not find backend URL in Dockerfile")
(root / "Dockerfile").write_text(text.replace(needle, needle + "\u0090", 1), encoding="utf-8")
PYTHON
if sh "$TMP/check-config.sh" "$TMP/mosdns.yaml" > "$TMP/backend-control-character.log" 2>&1; then
    echo "test-check-config: Dockerfile backend control character was accepted" >&2
    exit 1
fi
if grep -q "Traceback" "$TMP/backend-control-character.log" || ! grep -q "must not contain whitespace or control characters" "$TMP/backend-control-character.log"; then
    cat "$TMP/backend-control-character.log" >&2
    echo "test-check-config: Dockerfile backend control character was rejected for the wrong reason" >&2
    exit 1
fi
echo "PASS config validator rejects Dockerfile backend control character"

reject_dockerfile mismatched-version 's/^ARG GATEWAY_VERSION=.*/ARG GATEWAY_VERSION=9.9.9/' "must equal the VERSION file"
