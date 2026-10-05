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
def check_loopback(value: str, path: str) -> None:
    host = urlsplit("//" + value).hostname
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
plugins = expect_list(root.get("plugins"), "plugins")
plugin_by_tag = {}
for index, plugin in enumerate(plugins, start=1):
    plugin = expect_mapping(plugin, f"plugins[{index}]")
    tag = non_empty_string(plugin.get("tag"), f"plugins[{index}].tag")
    if tag in plugin_by_tag:
        fail(f"duplicate plugin tag: {tag}")
    plugin_by_tag[tag] = plugin
required_plugins = {
    "cache": "cache",
    "primary_fast": "fast_forward",
    "fallback_hagezi": "fast_forward",
    "main_sequence": "sequence",
}
for tag, expected_type in required_plugins.items():
    plugin = plugin_by_tag.get(tag)
    if plugin is None:
        fail(f"missing plugin: {tag}")
    actual_type = non_empty_string(plugin.get("type"), f"plugins[{tag}].type")
    if actual_type != expected_type:
        fail(f"plugin {tag} must use type {expected_type}")
cache = expect_mapping(plugin_by_tag["cache"].get("args"), "plugins[cache].args")
if exact_int(cache.get("size"), "plugins[cache].args.size") <= 0:
    fail("plugins[cache].args.size must be greater than 0")
if exact_int(cache.get("lazy_cache_ttl"), "plugins[cache].args.lazy_cache_ttl") < 0:
    fail("plugins[cache].args.lazy_cache_ttl must not be negative")
if exact_int(cache.get("lazy_cache_reply_ttl"), "plugins[cache].args.lazy_cache_reply_ttl") < 0:
    fail("plugins[cache].args.lazy_cache_reply_ttl must not be negative")
exact_bool(cache.get("cache_everything"), "plugins[cache].args.cache_everything")
if cache.get("when_hit") != "_return":
    fail("plugins[cache].args.when_hit must be _return")
for tag in ("primary_fast", "fallback_hagezi"):
    args = expect_mapping(plugin_by_tag[tag].get("args"), f"plugins[{tag}].args")
    upstreams = expect_list(args.get("upstream"), f"plugins[{tag}].args.upstream")
    expected_count = 2 if tag == "primary_fast" else 1
    if len(upstreams) != expected_count:
        fail(f"plugins[{tag}].args.upstream must contain exactly {expected_count} entries")
    for index, upstream in enumerate(upstreams, start=1):
        upstream = expect_mapping(upstream, f"plugins[{tag}].args.upstream[{index}]")
        check_upstream_addr(upstream.get("addr"), f"plugins[{tag}].args.upstream[{index}].addr")
        check_ip(upstream.get("dial_addr"), f"plugins[{tag}].args.upstream[{index}].dial_addr")
        exact_bool(upstream.get("enable_pipeline"), f"plugins[{tag}].args.upstream[{index}].enable_pipeline")
        if exact_int(upstream.get("idle_timeout"), f"plugins[{tag}].args.upstream[{index}].idle_timeout") <= 0:
            fail(f"plugins[{tag}].args.upstream[{index}].idle_timeout must be greater than 0")
        if exact_int(upstream.get("max_conns"), f"plugins[{tag}].args.upstream[{index}].max_conns") <= 0:
            fail(f"plugins[{tag}].args.upstream[{index}].max_conns must be greater than 0")
sequence = expect_mapping(plugin_by_tag["main_sequence"].get("args"), "plugins[main_sequence].args")
exec_chain = expect_list(sequence.get("exec"), "plugins[main_sequence].args.exec")
if len(exec_chain) < 2 or exec_chain[0] != "cache":
    fail("plugins[main_sequence].args.exec must start with cache")
fallback = None
for item in exec_chain:
    if isinstance(item, dict) and "primary" in item:
        fallback = item
        break
if fallback is None:
    fail("plugins[main_sequence].args.exec must contain a primary/secondary fallback entry")
primary = expect_list(fallback.get("primary"), "plugins[main_sequence].args.exec[].primary")
secondary = expect_list(fallback.get("secondary"), "plugins[main_sequence].args.exec[].secondary")
if primary != ["primary_fast"]:
    fail("main sequence primary must execute primary_fast")
if secondary != ["fallback_hagezi"]:
    fail("main sequence secondary must execute fallback_hagezi")
if exact_int(fallback.get("fast_fallback"), "plugins[main_sequence].args.exec[].fast_fallback") <= 0:
    fail("main sequence fast_fallback must be greater than 0 ms")
exact_bool(fallback.get("always_standby"), "plugins[main_sequence].args.exec[].always_standby")
servers = expect_list(root.get("servers"), "servers")
if len(servers) != 1:
    fail("servers must contain exactly one server entry")
server = expect_mapping(servers[0], "servers[0]")
if non_empty_string(server.get("exec"), "servers[0].exec") != "main_sequence":
    fail("servers[0].exec must be main_sequence")
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
    non_empty_string(listener.get("get_user_ip_from_header"), "servers[0].listeners[0].get_user_ip_from_header")
if "idle_timeout" in listener and exact_int(listener.get("idle_timeout"), "servers[0].listeners[0].idle_timeout") < 0:
    fail("servers[0].listeners[0].idle_timeout must not be negative")
check_loopback(listener["addr"], "servers[0].listeners[0].addr")
project_dir = config_path.resolve().parent
dockerfile = project_dir / "Dockerfile"
dockerfile_text = strip_comments(dockerfile.read_text(encoding="utf-8")) if dockerfile.is_file() else ""
if dockerfile.is_file():
    match = re.search(r"\bMOSDNS_DOH_URL=(\S+)", dockerfile_text)
    if match is None:
        fail("Dockerfile does not set MOSDNS_DOH_URL")
    backend = urlsplit(match.group(1).rstrip("\\"))
    listener_port = urlsplit("//" + listener["addr"]).port
    if backend.port != listener_port:
        fail(f"Dockerfile MOSDNS_DOH_URL port {backend.port} does not match listener port {listener_port}")
    if backend.path != listener["url_path"]:
        fail(f"Dockerfile MOSDNS_DOH_URL path {backend.path!r} does not match listener url_path {listener['url_path']!r}")
server_timeout_ms = server["timeout"] * 1000
if fallback["fast_fallback"] >= server_timeout_ms:
    fail(
        f"fast_fallback ({fallback['fast_fallback']} ms) must be below servers[0].timeout ({server_timeout_ms} ms)"
    )
gateway_timeout_ms = None
if re.search(r"\bUPSTREAM_TIMEOUT=", dockerfile_text):
    env_match = re.search(r"\bUPSTREAM_TIMEOUT=(\d+(?:\.\d+)?)(ms|s)\b", dockerfile_text)
    if env_match is None:
        fail("Dockerfile sets UPSTREAM_TIMEOUT in a format this check cannot read; use <number>ms or <number>s")
    gateway_timeout_ms = float(env_match.group(1)) * (1 if env_match.group(2) == "ms" else 1000)
else:
    main_go = project_dir / "main.go"
    if not main_go.is_file():
        fail("main.go not found next to the config; cannot verify the gateway UPSTREAM_TIMEOUT default")
    go_match = re.search(r"defaultUpstreamTO\s*=\s*(\d+)\s*\*\s*time\.Second", main_go.read_text(encoding="utf-8"))
    if go_match is None:
        fail("could not read defaultUpstreamTO from main.go (expected '<n> * time.Second'); the timeout-chain check would be skipped")
    gateway_timeout_ms = int(go_match.group(1)) * 1000
if server_timeout_ms >= gateway_timeout_ms:
    fail(
        f"servers[0].timeout ({server_timeout_ms} ms) must be below the gateway UPSTREAM_TIMEOUT ({gateway_timeout_ms:g} ms)"
    )
version_file = project_dir / "VERSION"
if version_file.is_file() and dockerfile_text:
    file_version = version_file.read_text(encoding="utf-8").strip()
    arg_match = re.search(r"^ARG[ \t]+GATEWAY_VERSION=(\S+)", dockerfile_text, re.MULTILINE)
    if arg_match is None:
        fail("Dockerfile does not declare a default ARG GATEWAY_VERSION")
    if arg_match.group(1) != file_version:
        fail(f"Dockerfile GATEWAY_VERSION {arg_match.group(1)} does not match VERSION {file_version}")
print("YAML parsed successfully; MosDNS project schema checks: OK")
PYTHON