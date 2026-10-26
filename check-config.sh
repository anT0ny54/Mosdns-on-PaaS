#!/bin/sh
set -eu
CONFIG="${1:-mosdns.yaml}"
[ -f "$CONFIG" ] || {
    echo "check-config: file not found: $CONFIG" >&2
    exit 1
}
PYTHON=""
if command -v python3 >/dev/null 2>&1; then
    PYTHON=python3
elif command -v python >/dev/null 2>&1; then
    PYTHON=python
else
    echo "check-config: Python 3 with PyYAML is required to validate $CONFIG" >&2
    exit 1
fi
if ! "$PYTHON" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 8) else 1)' >/dev/null 2>&1; then
    echo "check-config: Python 3.8+ with PyYAML is required to validate $CONFIG" >&2
    exit 1
fi
exec "$PYTHON" - "$CONFIG" <<'PYTHON'
import ipaddress
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit
try:
    import yaml
except ImportError as exc:
    print("check-config: PyYAML is required (python3 -m pip install pyyaml)", file=sys.stderr)
    raise SystemExit(1) from exc
config_path = Path(sys.argv[1])
def fail(message: str) -> None:
    print(f"check-config: {message}", file=sys.stderr)
    raise SystemExit(1)
def expect_mapping(value, path: str) -> dict:
    if not isinstance(value, dict):
        fail(f"{path} must be a mapping")
    return value
def expect_list(value, path: str) -> list:
    if not isinstance(value, list):
        fail(f"{path} must be a list")
    return value
def non_empty_string(value, path: str) -> str:
    if not isinstance(value, str) or not value.strip():
        fail(f"{path} must be a non-empty string")
    return value
def exact_int(value, path: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        fail(f"{path} must be an integer")
    return value
def exact_bool(value, path: str) -> bool:
    if not isinstance(value, bool):
        fail(f"{path} must be true or false")
    return value
def strip_comments(text: str) -> str:
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#"))
DURATION_UNIT_MS = {"ms": 1, "s": 1000, "m": 60000, "h": 3600000,
                    "Millisecond": 1, "Second": 1000, "Minute": 60000, "Hour": 3600000}
def major_minor(version: str) -> str:
    return ".".join(version.split(".")[:2])
def check_ip(value, path: str) -> None:
    try:
        ipaddress.ip_address(non_empty_string(value, path))
    except ValueError:
        fail(f"{path} must be a valid IP address")
def check_listen_addr(value, path: str) -> None:
    value = non_empty_string(value, path)
    try:
        parsed = urlsplit("//" + value)
        port = parsed.port
    except ValueError:
        fail(f"{path} must be a valid host:port address")
    if port is None or not 1 <= port <= 65535:
        fail(f"{path} must include a valid port from 1 to 65535")
    if parsed.path or parsed.query or parsed.fragment:
        fail(f"{path} must be a host:port address")
def listen_host(value: str):
    host = urlsplit("//" + value).hostname
    if host is not None and "%" in host:
        # urlsplit keeps an IPv6 zone id in the hostname ([fe80::1%eth0]
        # parses to "fe80::1%eth0"); strip it before comparing.
        host = host.split("%", 1)[0]
    return host
def check_loopback(value: str, path: str) -> None:
    host = listen_host(value)
    if host is None:
        fail(f"{path} must bind to loopback explicitly (an empty host listens on every interface)")
    if host not in {"127.0.0.1", "localhost", "::1"}:
        fail(f"{path} must bind to loopback, not {host}")
def check_upstream_addr(value, path: str) -> None:
    value = non_empty_string(value, path)
    parsed = urlsplit(value)
    if not parsed.scheme or not parsed.netloc or not parsed.hostname:
        fail(f"{path} must be a valid upstream URL")
    try:
        port = parsed.port
    except ValueError:
        fail(f"{path} must contain a valid port")
    if port is not None and not 1 <= port <= 65535:
        fail(f"{path} must contain a valid port from 1 to 65535")
try:
    with config_path.open("r", encoding="utf-8") as fh:
        cfg = yaml.safe_load(fh)
except yaml.YAMLError as exc:
    problem = getattr(exc, "problem", None) or str(exc).splitlines()[0]
    mark = getattr(exc, "problem_mark", None)
    if mark is not None:
        problem = f"line {mark.line + 1}, column {mark.column + 1}: {problem}"
    fail(f"invalid YAML: {problem}")
except OSError as exc:
    fail(f"cannot read {config_path}: {exc}")
project_dir = config_path.resolve().parent
root = expect_mapping(cfg, "root")
log = expect_mapping(root.get("log"), "log")
level = non_empty_string(log.get("level"), "log.level")
if level not in {"debug", "info", "warn", "error"}:
    fail("log.level must be one of debug, info, warn, error")
api = root.get("api")
if api is not None:
    api = expect_mapping(api, "api")
    check_listen_addr(api.get("http"), "api.http")
    check_loopback(api["http"], "api.http")

# --- plugins -----------------------------------------------------------------
plugins = expect_list(root.get("plugins"), "plugins")
plugin_by_tag = {}
for index, plugin in enumerate(plugins, start=1):
    plugin = expect_mapping(plugin, f"plugins[{index}]")
    tag = non_empty_string(plugin.get("tag"), f"plugins[{index}].tag")
    if tag in plugin_by_tag:
        fail(f"duplicate plugin tag: {tag}")
    non_empty_string(plugin.get("type"), f"plugins[{tag}].type")
    plugin_by_tag[tag] = plugin
def plugin_of_type(tag, expected_type: str, path: str) -> dict:
    plugin = plugin_by_tag.get(tag) if isinstance(tag, str) else None
    if plugin is None:
        fail(f"{path}: unknown plugin {tag!r}")
    if plugin["type"] != expected_type:
        fail(f"{path}: plugin {tag} must use type {expected_type}, not {plugin['type']}")
    return plugin

# --- servers -----------------------------------------------------------------
servers = expect_list(root.get("servers"), "servers")
if len(servers) != 1:
    fail("servers must contain exactly one server entry")
server = expect_mapping(servers[0], "servers[0]")
entry = non_empty_string(server.get("exec"), "servers[0].exec")
if exact_int(server.get("timeout"), "servers[0].timeout") <= 0:
    fail("servers[0].timeout must be greater than 0 seconds")
listeners = expect_list(server.get("listeners"), "servers[0].listeners")
if len(listeners) != 1:
    fail("servers[0].listeners must contain exactly one listener")
listener = expect_mapping(listeners[0], "servers[0].listeners[0]")
protocol = non_empty_string(listener.get("protocol"), "servers[0].listeners[0].protocol")
if protocol != "http":
    fail(f"servers[0].listeners[0].protocol must be http (the gateway reaches MosDNS over plain HTTP), not {protocol}")
check_listen_addr(listener.get("addr"), "servers[0].listeners[0].addr")
path = non_empty_string(listener.get("url_path"), "servers[0].listeners[0].url_path")
if not path.startswith("/"):
    fail("servers[0].listeners[0].url_path must start with /")
if "get_user_ip_from_header" in listener:
    fail("servers[0].listeners[0].get_user_ip_from_header must not be set: the gateway does not forward the client address to MosDNS")
if "idle_timeout" in listener and exact_int(listener.get("idle_timeout"), "servers[0].listeners[0].idle_timeout") < 0:
    fail("servers[0].listeners[0].idle_timeout must not be negative")
check_loopback(listener["addr"], "servers[0].listeners[0].addr")

# --- entry sequence: [local answers / EDNS filters]..., cache, fallback ------
sequence = plugin_of_type(entry, "sequence", "servers[0].exec")
seq_path = f"plugins[{entry}].args.exec"
exec_chain = expect_list(expect_mapping(sequence.get("args"), f"plugins[{entry}].args").get("exec"), seq_path)
LOCAL_TYPES = {"arbitrary", "hosts"}
position = 0
local_tags = []
edns_filters = []
while position < len(exec_chain):
    item = exec_chain[position]
    if isinstance(item, str) and item.startswith("_edns0_filter_"):
        edns_filters.append(item)
    elif isinstance(item, str) and item in plugin_by_tag and plugin_by_tag[item]["type"] in LOCAL_TYPES:
        local_tags.append(item)
    else:
        break
    position += 1
if position >= len(exec_chain):
    fail(f"{seq_path} must contain a cache plugin")
cache_tag = exec_chain[position]
cache = expect_mapping(plugin_of_type(cache_tag, "cache", f"{seq_path}[{position}] (expected the cache after optional local-answer/_edns0_filter_* entries)").get("args"), f"plugins[{cache_tag}].args")
if exact_int(cache.get("size"), f"plugins[{cache_tag}].args.size") <= 0:
    fail(f"plugins[{cache_tag}].args.size must be greater than 0")
if exact_int(cache.get("lazy_cache_ttl"), f"plugins[{cache_tag}].args.lazy_cache_ttl") < 0:
    fail(f"plugins[{cache_tag}].args.lazy_cache_ttl must not be negative")
if exact_int(cache.get("lazy_cache_reply_ttl"), f"plugins[{cache_tag}].args.lazy_cache_reply_ttl") < 0:
    fail(f"plugins[{cache_tag}].args.lazy_cache_reply_ttl must not be negative")
cache_everything = exact_bool(cache.get("cache_everything"), f"plugins[{cache_tag}].args.cache_everything")
if not cache_everything and "_edns0_filter_no_edns0" not in edns_filters:
    fail(f"plugins[{cache_tag}].args.cache_everything must be true: MosDNS v4 skips the cache for every query with an EDNS0 record, which nearly all DoH clients send")
if cache_everything and not edns_filters:
    fail(f"{seq_path}: with cache_everything an _edns0_filter_* entry must precede {cache_tag}, or EDNS padding/cookies fragment the cache keys")
if cache.get("when_hit") != "_return":
    fail(f"plugins[{cache_tag}].args.when_hit must be _return")
if position + 2 != len(exec_chain):
    fail(f"{seq_path} must contain exactly one primary/secondary fallback after {cache_tag}; extra or unvalidated sequence entries are not allowed")
fallback = exec_chain[position + 1]
if not isinstance(fallback, dict) or "primary" not in fallback or "secondary" not in fallback:
    fail(f"{seq_path} must contain a primary/secondary fallback entry immediately after {cache_tag}")
unknown_fallback_keys = set(fallback) - {"primary", "secondary", "stat_length", "threshold", "fast_fallback", "always_standby"}
if unknown_fallback_keys:
    fail(f"{seq_path} fallback contains unsupported key(s): {', '.join(sorted(str(k) for k in unknown_fallback_keys))}")
# These optional fields are part of MosDNS v4.5.3's FallbackConfig.
# stat_length=0 disables normal fallback; negative values are almost certainly
# a config mistake. threshold is clamped to stat_length by MosDNS when enabled.
if "stat_length" in fallback and exact_int(fallback.get("stat_length"), f"{seq_path}[].stat_length") < 0:
    fail("main sequence stat_length must not be negative")
if "threshold" in fallback and exact_int(fallback.get("threshold"), f"{seq_path}[].threshold") < 0:
    fail("main sequence threshold must not be negative")
for side in ("primary", "secondary"):
    tags = expect_list(fallback.get(side), f"{seq_path}[].{side}")
    if not tags:
        fail(f"{seq_path}[].{side} must not be empty")
    for tag in tags:
        args = expect_mapping(plugin_of_type(tag, "fast_forward", f"{seq_path}[].{side}").get("args"), f"plugins[{tag}].args")
        upstreams = expect_list(args.get("upstream"), f"plugins[{tag}].args.upstream")
        if not upstreams:
            fail(f"plugins[{tag}].args.upstream must not be empty")
        for index, upstream in enumerate(upstreams, start=1):
            p = f"plugins[{tag}].args.upstream[{index}]"
            upstream = expect_mapping(upstream, p)
            check_upstream_addr(upstream.get("addr"), f"{p}.addr")
            if "dial_addr" in upstream:
                check_ip(upstream.get("dial_addr"), f"{p}.dial_addr")
            # Optional keys are validated only when present. MosDNS v4.5.3
            # only applies pipeline to TCP/DoT upstreams (a dead key for DoH),
            # and max_conns defaults to 2 for DoH.
            if "enable_pipeline" in upstream:
                exact_bool(upstream.get("enable_pipeline"), f"{p}.enable_pipeline")
            if "idle_timeout" in upstream and exact_int(upstream.get("idle_timeout"), f"{p}.idle_timeout") <= 0:
                fail(f"{p}.idle_timeout must be greater than 0")
            if "max_conns" in upstream and exact_int(upstream.get("max_conns"), f"{p}.max_conns") <= 0:
                fail(f"{p}.max_conns must be greater than 0")
            trusted = exact_bool(upstream["trusted"], f"{p}.trusted") if "trusted" in upstream else False
            # MosDNS v4 trusts only the first upstream implicitly and drops
            # non-NOERROR answers (NXDOMAIN, SERVFAIL) from untrusted ones, so
            # a race between untrusted upstreams silently degrades.
            if index > 1 and not trusted:
                fail(f"{p}.trusted must be true: MosDNS v4 drops NXDOMAIN/SERVFAIL answers from untrusted upstreams, so the parallel race in {tag} would only use the first upstream for them")
if exact_int(fallback.get("fast_fallback"), f"{seq_path}[].fast_fallback") <= 0:
    fail("main sequence fast_fallback must be greater than 0 ms")
if "always_standby" in fallback:
    exact_bool(fallback.get("always_standby"), f"{seq_path}[].always_standby")
server_timeout_ms = server["timeout"] * 1000
if fallback["fast_fallback"] >= server_timeout_ms:
    fail(f"fast_fallback ({fallback['fast_fallback']} ms) must be below servers[0].timeout ({server_timeout_ms} ms)")

# --- main.go: readiness name and default upstream timeout ---------------------
main_go = project_dir / "main.go"
main_text = main_go.read_text(encoding="utf-8") if main_go.is_file() else ""
if main_text:
    m = re.search(r'readinessProbeName\s*=\s*"([^"]+)"', main_text)
    if m is None:
        fail("could not read readinessProbeName from main.go")
    probe = m.group(1).rstrip(".").lower() + "."
    answered = False
    for tag in local_tags:
        if plugin_by_tag[tag]["type"] != "arbitrary":
            continue
        rrs = expect_list(expect_mapping(plugin_by_tag[tag].get("args"), f"plugins[{tag}].args").get("rr"), f"plugins[{tag}].args.rr")
        for rr in rrs:
            fields = non_empty_string(rr, f"plugins[{tag}].args.rr[]").split()
            if (len(fields) >= 5 and fields[0].lower() == probe
                    and fields[3].upper() == "A" and fields[4] == "127.0.0.1"):
                answered = True
    if not answered:
        fail(f"no arbitrary plugin before {cache_tag} answers 'A {probe} 127.0.0.1' (main.go readinessProbeName); /readyz would depend on the public upstreams")
    m = re.search(r"mosdnsServerTimeout\s*=\s*(\d+(?:\.\d+)?)\s*\*\s*time\.(Millisecond|Second|Minute|Hour)\b", main_text)
    if m is None:
        fail("could not read mosdnsServerTimeout from main.go (expected '<n> * time.Millisecond|Second|Minute|Hour')")
    go_server_timeout_ms = float(m.group(1)) * DURATION_UNIT_MS[m.group(2)]
    if go_server_timeout_ms != server_timeout_ms:
        fail(f"main.go mosdnsServerTimeout ({go_server_timeout_ms:g} ms) must equal servers[0].timeout ({server_timeout_ms} ms)")

# --- Dockerfile cross-checks -------------------------------------------------
dockerfile = project_dir / "Dockerfile"
dockerfile_text = strip_comments(dockerfile.read_text(encoding="utf-8")) if dockerfile.is_file() else ""
if dockerfile_text:
    match = re.search(r"\bMOSDNS_DOH_URL=(\S+)", dockerfile_text)
    if match is None:
        fail("Dockerfile does not set MOSDNS_DOH_URL")
    backend = urlsplit(match.group(1).rstrip("\\"))
    listener_host = listen_host(listener["addr"])
    listener_port = urlsplit("//" + listener["addr"]).port
    if backend.hostname != listener_host:
        fail(f"Dockerfile MOSDNS_DOH_URL host {backend.hostname} does not match listener host {listener_host}")
    if backend.port != listener_port:
        fail(f"Dockerfile MOSDNS_DOH_URL port {backend.port} does not match listener port {listener_port}")
    if backend.path != listener["url_path"]:
        fail(f"Dockerfile MOSDNS_DOH_URL path {backend.path!r} does not match listener url_path {listener['url_path']!r}")
gateway_timeout_ms = None
if re.search(r"\bUPSTREAM_TIMEOUT=", dockerfile_text):
    env_match = re.search(r"\bUPSTREAM_TIMEOUT=(\d+(?:\.\d+)?)(ms|s|m|h)\b", dockerfile_text)
    if env_match is None:
        fail("Dockerfile sets UPSTREAM_TIMEOUT in a format this check cannot read; use <number>ms, <number>s, <number>m or <number>h")
    gateway_timeout_ms = float(env_match.group(1)) * DURATION_UNIT_MS[env_match.group(2)]
else:
    if not main_text:
        fail("main.go not found next to the config; cannot verify the gateway UPSTREAM_TIMEOUT default")
    go_match = re.search(r"defaultUpstreamTO\s*=\s*(\d+(?:\.\d+)?)\s*\*\s*time\.(Millisecond|Second|Minute|Hour)\b", main_text)
    if go_match is None:
        fail("could not read defaultUpstreamTO from main.go (expected '<n> * time.Millisecond', '<n> * time.Second', '<n> * time.Minute' or '<n> * time.Hour'); the timeout-chain check failed")
    gateway_timeout_ms = float(go_match.group(1)) * DURATION_UNIT_MS[go_match.group(2)]
if server_timeout_ms >= gateway_timeout_ms:
    fail(f"servers[0].timeout ({server_timeout_ms} ms) must be below the gateway UPSTREAM_TIMEOUT ({gateway_timeout_ms:g} ms)")

# --- Go version consistency (major.minor) ------------------------------------
go_versions = {}
go_mod = project_dir / "go.mod"
if go_mod.is_file():
    m = re.search(r"^go[ \t]+(\d+(?:\.\d+)*)\s*$", go_mod.read_text(encoding="utf-8"), re.MULTILINE)
    if m is None:
        fail("go.mod has no 'go <version>' directive")
    go_versions["go.mod"] = m.group(1)
    if dockerfile_text:
        # FROM lines are literal (Dependabot cannot read ARG-templated ones).
        from_re = r"^FROM[ \t]+(?:--platform=\S+[ \t]+)?golang:(\d+(?:\.\d+)*)\S*[ \t]+AS[ \t]+{}[ \t]*$"
        m = re.search(from_re.format("gateway-builder"), dockerfile_text, re.MULTILINE | re.IGNORECASE)
        if m is None:
            fail("Dockerfile has no literal 'FROM golang:<version>... AS gateway-builder' line")
        go_versions["Dockerfile gateway-builder"] = m.group(1)
        m = re.search(from_re.format("mosdns-builder"), dockerfile_text, re.MULTILINE | re.IGNORECASE)
        if m is None:
            fail("Dockerfile has no literal 'FROM golang:<version>... AS mosdns-builder' line")
        if major_minor(m.group(1)) != "1.19":
            fail(f"Dockerfile mosdns-builder uses Go {m.group(1)}; MosDNS v4 (quic-go v0.30) only builds with Go 1.19")
    ci_file = project_dir / ".github" / "workflows" / "ci.yml"
    if ci_file.is_file():
        for n, m in enumerate(re.finditer(r"^\s*go-version:\s*[\"']?([^\s\"'#]+)", ci_file.read_text(encoding="utf-8"), re.MULTILINE), 1):
            go_versions[f"ci.yml go-version #{n}"] = m.group(1)
    if len({major_minor(v) for v in go_versions.values()}) > 1:
        fail("Go versions disagree (keep them in sync): " + ", ".join(f"{k}={v}" for k, v in go_versions.items()))
print("YAML parsed successfully; MosDNS project schema checks: OK")
PYTHON
