#!/usr/bin/env bash
set -euo pipefail

script_dir=${BASH_SOURCE[0]%/*}
root=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
timeout_seconds=${LO_RELEASE_SMOKE_TIMEOUT:-120}
shutdown_timeout_seconds=${LO_RELEASE_SMOKE_SHUTDOWN_TIMEOUT:-45}
max_rss_bytes=${LO_RELEASE_MAX_RSS_BYTES:-524288000}
smoke_output=${LO_RELEASE_SMOKE_OUTPUT:-}

skip() {
  printf 'SKIP: %s\n' "$*" >&2
  exit 77
}

fail() {
  failure_context=$*
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

for command in curl env go mkdir mktemp ps tail rm sleep; do
  command -v "$command" >/dev/null 2>&1 || skip "required command is unavailable: $command"
done

# 内嵌的校验脚本只用 Python 标准库。解释器由 scripts/lib/uv-python.sh 解析：uv 受管的固定版本
# （LO_SCRIPTS_PYTHON_VERSION，默认 3.13），不用系统 python3、不用 pip；uv 缺失是失败而不是 SKIP。
# shellcheck source=lib/uv-python.sh
. "$root/scripts/lib/uv-python.sh"
python_command=$(lo_uv_python) || exit 1

case $timeout_seconds in
  ''|*[!0-9]*) fail "LO_RELEASE_SMOKE_TIMEOUT must be a positive integer" ;;
esac
case $shutdown_timeout_seconds in
  ''|*[!0-9]*) fail "LO_RELEASE_SMOKE_SHUTDOWN_TIMEOUT must be a positive integer" ;;
esac
case $max_rss_bytes in
  ''|*[!0-9]*) fail "LO_RELEASE_MAX_RSS_BYTES must be a positive integer" ;;
esac
(( timeout_seconds > 0 )) || fail "LO_RELEASE_SMOKE_TIMEOUT must be greater than zero"
(( shutdown_timeout_seconds > 0 )) || fail "LO_RELEASE_SMOKE_SHUTDOWN_TIMEOUT must be greater than zero"
(( max_rss_bytes > 0 )) || fail "LO_RELEASE_MAX_RSS_BYTES must be greater than zero"
if [[ -n $smoke_output ]]; then
  case $smoke_output in /*) ;; *) smoke_output=$root/$smoke_output ;; esac
  smoke_output_dir=${smoke_output%/*}
  [[ $smoke_output_dir != "$smoke_output" ]] || smoke_output_dir=$root
  mkdir -p "$smoke_output_dir"
fi

binary_input=${LO_RELEASE_BINARY:-bin/lazy-observe}
case $binary_input in
  /*) binary_candidate=$binary_input ;;
  *) binary_candidate=$root/$binary_input ;;
esac
binary_dir=${binary_candidate%/*}
binary_name=${binary_candidate##*/}
[[ -d $binary_dir ]] || skip "release binary directory does not exist: $binary_dir"
binary_dir=$(CDPATH= cd -- "$binary_dir" && pwd -P)
binary=$binary_dir/$binary_name
[[ -f $binary ]] || skip "release binary does not exist: $binary (run mise run dist)"
[[ -x $binary ]] || fail "release artifact is not executable: $binary"

if ! "$python_command" - <<'PY'
import socket
import sys

listeners = [
    ("gateway", "0.0.0.0", 3100),
    ("metrics", "127.0.0.1", 18428),
    ("logs", "127.0.0.1", 19428),
    ("traces", "127.0.0.1", 20428),
    ("vmagent", "127.0.0.1", 22428),
]
busy = []
for name, host, port in listeners:
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        # Match Go's listener behavior so sockets in TIME_WAIT after a previous
        # successful smoke do not turn an immediate rerun into a false SKIP.
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind((host, port))
    except OSError as exc:
        busy.append(f"{name} {host}:{port}: {exc}")
    finally:
        sock.close()
if busy:
    print("required default ports are unavailable:\n  " + "\n  ".join(busy), file=sys.stderr)
    raise SystemExit(1)
PY
then
  skip "default gateway or managed-component port is already in use"
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-release-smoke.XXXXXX")
run_dir=$work/cwd
mkdir "$run_dir"
mkdir "$work/home"
log=$work/server.log
ready_json=$work/ready.json
health_json=$work/health.json
web_headers=$work/web.headers
web_body=$work/index.html
web_assets=$work/web-assets.txt
asset_headers=$work/asset.headers
asset_body=$work/asset.body
children_file=$work/children.tsv
server_pid=
failure_context=
log_dumped=0

dump_log() {
  if [[ $log_dumped -eq 0 && -s $log ]]; then
    log_dumped=1
    printf '%s\n' '--- lazy-observe smoke log (last 200 lines) ---' >&2
    tail -n 200 "$log" | "$python_command" -c '
import re
import sys

for line in sys.stdin:
    line = re.sub(r"(password|project_api_token)=\S+", r"\1=[REDACTED]", line)
    sys.stderr.write(line)
' || true
    printf '%s\n' '--- end lazy-observe smoke log ---' >&2
  fi
}

stop_managed_processes() {
  # This is only a last-resort cleanup. Match the unique temporary extraction
  # directory before signalling a PID, so an unrelated Victoria process cannot
  # be affected even if a recorded PID has already been reused.
  "$python_command" - "$run_dir" <<'PY' >/dev/null 2>&1 || true
import os
import signal
import subprocess
import sys
import time

work = os.path.realpath(sys.argv[1])
names = {
    "victoria-metrics-prod",
    "victoria-logs-prod",
    "victoria-traces-prod",
    "vmagent-prod",
}
expected = {os.path.join(work, "data", "bin", name) for name in names}

def matches():
    result = subprocess.run(
        ["ps", "-eo", "pid=,args="],
        check=False,
        capture_output=True,
        text=True,
    )
    found = []
    for line in result.stdout.splitlines():
        fields = line.strip().split(None, 1)
        if len(fields) != 2:
            continue
        try:
            pid = int(fields[0])
        except ValueError:
            continue
        command = fields[1]
        if any(path in command for path in expected):
            found.append(pid)
    return found

for pid in matches():
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
for _ in range(20):
    if not matches():
        break
    time.sleep(0.1)
for pid in matches():
    try:
        os.kill(pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
PY
}

cleanup() {
  rc=$?
  trap - EXIT HUP INT TERM
  if [[ -n ${server_pid:-} ]]; then
    if kill -0 "$server_pid" 2>/dev/null; then
      kill -TERM "$server_pid" 2>/dev/null || true
      deadline=$((SECONDS + shutdown_timeout_seconds))
      while kill -0 "$server_pid" 2>/dev/null && (( SECONDS < deadline )); do
        sleep 0.2
      done
      if kill -0 "$server_pid" 2>/dev/null; then
        kill -KILL "$server_pid" 2>/dev/null || true
      fi
    fi
    wait "$server_pid" 2>/dev/null || true
    server_pid=
  fi
  stop_managed_processes
  if (( rc != 0 )); then
    [[ -z $failure_context ]] || printf 'smoke failure context: %s\n' "$failure_context" >&2
    dump_log
  fi
  rm -rf -- "$work"
  exit "$rc"
}

trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

CGO_ENABLED=0 go build -o "$work/e2e-otlp-payload" "$root/scripts/e2e-otlp-payload"

# Deliberately start from an empty cwd and an allow-listed environment. This
# prevents release, registry, cloud, and CI credentials from reaching either
# the gateway or its four managed children. The release contract being tested
# is exactly: <absolute-binary> server.
(
  cd "$run_dir"
  exec env -i HOME="$work/home" PATH="$PATH" TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-C.UTF-8}" \
    "$binary" server
) >"$log" 2>&1 &
server_pid=$!

ready=0
deadline=$((SECONDS + timeout_seconds))
while (( SECONDS < deadline )); do
  if ! kill -0 "$server_pid" 2>/dev/null; then
    failure_context="server exited before /readyz became ready"
    fail "$failure_context"
  fi
  if curl --fail --silent --show-error --max-time 2 \
      http://127.0.0.1:3100/readyz >"$ready_json" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.2
done
(( ready == 1 )) || fail "timed out after ${timeout_seconds}s waiting for http://127.0.0.1:3100/readyz"

curl --fail --silent --show-error --max-time 5 \
  http://127.0.0.1:3100/healthz >"$health_json" || fail "default /healthz did not return HTTP 200"
curl --fail --silent --show-error --max-time 5 \
  --dump-header "$web_headers" --output "$web_body" \
  http://127.0.0.1:3100/ || fail "embedded Web homepage did not return HTTP 200"

if ! "$python_command" - "$ready_json" "$health_json" "$web_headers" "$web_body" "$web_assets" <<'PY'
import html.parser
import json
import pathlib
import sys
import urllib.parse

ready_path, health_path, headers_path, body_path, assets_path = map(pathlib.Path, sys.argv[1:])
required = {"metrics", "logs", "traces", "vmagent"}

def check_health(path, expected_status):
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"invalid {path.name}: {exc}")
    if payload.get("status") != expected_status:
        raise SystemExit(f"{path.name} status={payload.get('status')!r}, want {expected_status!r}")
    components = payload.get("components")
    if not isinstance(components, dict):
        raise SystemExit(f"{path.name} has no component health map")
    missing = sorted(required - components.keys())
    unhealthy = {name: components.get(name) for name in required if components.get(name) != "ok"}
    if missing or unhealthy:
        raise SystemExit(f"{path.name} missing={missing} unhealthy={unhealthy}")

check_health(ready_path, "ready")
check_health(health_path, "ok")
headers = headers_path.read_text(encoding="iso-8859-1").lower()
if "content-type: text/html" not in headers:
    raise SystemExit("homepage response is not text/html")
body = body_path.read_text(encoding="utf-8")
if "<title>lazy-observe</title>" not in body or 'id="root"' not in body:
    raise SystemExit("homepage is not the embedded lazy-observe Web entrypoint")

class EntryAssets(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.assets = []
        self.scripts = 0
        self.styles = 0

    def handle_starttag(self, tag, attrs):
        values = dict(attrs)
        candidate = None
        if tag == "script" and values.get("src"):
            candidate = values["src"]
            self.scripts += 1
        elif tag == "link" and values.get("href"):
            rel = set(values.get("rel", "").lower().split())
            if rel.intersection({"stylesheet", "modulepreload"}):
                candidate = values["href"]
                if "stylesheet" in rel:
                    self.styles += 1
        if candidate is not None:
            parsed = urllib.parse.urlsplit(candidate)
            if parsed.scheme or parsed.netloc or parsed.fragment or not parsed.path.startswith("/assets/"):
                raise SystemExit(f"unexpected external or non-asset Web entry reference: {candidate!r}")
            self.assets.append(urllib.parse.urlunsplit(("", "", parsed.path, parsed.query, "")))

parser = EntryAssets()
parser.feed(body)
assets = list(dict.fromkeys(parser.assets))
if parser.scripts == 0 or parser.styles == 0 or not assets:
    raise SystemExit("homepage does not reference embedded JavaScript and CSS assets")
assets_path.write_text("".join(asset + "\n" for asset in assets), encoding="utf-8")
PY
then
  fail "health or embedded Web response validation failed"
fi

while IFS= read -r asset; do
  [[ -n $asset ]] || continue
  curl --fail --silent --show-error --max-time 10 \
    --dump-header "$asset_headers" --output "$asset_body" \
    "http://127.0.0.1:3100$asset" || fail "embedded Web asset did not return HTTP 200: $asset"
  if ! "$python_command" - "$asset" "$asset_headers" "$asset_body" <<'PY'
import pathlib
import sys
import urllib.parse

asset = urllib.parse.urlsplit(sys.argv[1]).path.lower()
headers = pathlib.Path(sys.argv[2]).read_text(encoding="iso-8859-1").lower()
body = pathlib.Path(sys.argv[3]).read_bytes()
if not body:
    raise SystemExit("asset body is empty")
if asset.endswith(".css") and "content-type: text/css" not in headers:
    raise SystemExit("CSS asset has the wrong Content-Type")
if asset.endswith((".js", ".mjs")) and not any(
    value in headers for value in ("content-type: text/javascript", "content-type: application/javascript")
):
    raise SystemExit("JavaScript asset has the wrong Content-Type")
PY
  then
    fail "embedded Web asset validation failed: $asset"
  fi
done <"$web_assets"

# Exercise the exact embedded artifact's bootstrap, authentication, ingest,
# tenant routing, storage writes, and query paths. A healthy homepage alone is
# not sufficient evidence that the zero-configuration data plane works.
# Zero-config bootstrap writes the generated admin password to
# <data_dir>/initial-admin-password (0600) and logs only that path. The
# password goes straight from that file into a 0600 login body, never argv.
credentials=$("$python_command" - "$log" "$run_dir" "$work/login.json" <<'PY'
import json
import pathlib
import re
import stat
import sys

log = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8", errors="replace")
run_dir = pathlib.Path(sys.argv[2])
login_path = pathlib.Path(sys.argv[3])
written = re.findall(r"initial admin password written[^\n]*?path=(\S+)", log)
if len(written) != 1:
    raise SystemExit("expected exactly one initial admin password path in the log")
password_path = (run_dir / written[0]).resolve()
if password_path != (run_dir / "data" / "initial-admin-password").resolve():
    raise SystemExit("initial admin password was not written under the default data_dir")
mode = stat.S_IMODE(password_path.stat().st_mode)
if mode != 0o600:
    raise SystemExit(f"initial admin password file mode is {mode:o}, want 600")
password = password_path.read_text(encoding="utf-8").strip()
if len(password) < 8:
    raise SystemExit("initial admin password file is empty or too short")
if password in log or re.search(r"(?<![\w])password=", log):
    raise SystemExit("server log contains the bootstrap admin password")
tokens = re.findall(r"created default project[^\n]*?project_api_token=(\S+)", log)
if len(tokens) != 1 or tokens[0] == password:
    raise SystemExit("expected exactly one default project API token")
login_path.write_text(json.dumps({"username": "admin", "password": password}, separators=(",", ":")), encoding="utf-8")
login_path.chmod(0o600)
print(tokens[0])
PY
) || fail "could not read the one-time bootstrap credentials"
project_api_token=$credentials
[[ -n $project_api_token ]] || fail "invalid bootstrap credentials"
printf '%s' "$project_api_token" | "$python_command" -c '
import pathlib
import sys
token = sys.stdin.read()
if not token or "\n" in token or "\r" in token:
    raise SystemExit("invalid token")
path = pathlib.Path(sys.argv[1])
path.write_text("Authorization: Bearer " + token + "\n", encoding="utf-8")
path.chmod(0o600)
' "$work/token.header"
curl --fail --silent --show-error --max-time 5 -c "$work/cookies" \
  -H 'Content-Type: application/json' --data-binary @"$work/login.json" \
  http://127.0.0.1:3100/api/auth/login >"$work/login-response.json" || fail "bootstrap admin login failed"
curl --fail --silent --show-error --max-time 5 -b "$work/cookies" \
  http://127.0.0.1:3100/api/auth/me >"$work/auth-me.json" || fail "bootstrap session validation failed"
"$python_command" - "$work/auth-me.json" <<'PY' || fail "bootstrap session returned the wrong identity"
import json
import pathlib
import sys

payload = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
if payload.get("user", {}).get("username") != "admin" and payload.get("username") != "admin":
    raise SystemExit("authenticated user is not admin")
if payload.get("must_change_password") is not True:
    raise SystemExit("generated bootstrap admin is not forced to change its password")
PY

# Project API tokens are ingest-only; every read below uses a user API token
# issued to the logged-in admin plus an explicit X-LO-Project selector. The CSRF
# and user token values stay in 0600 header files rather than argv.
"$python_command" - "$work/cookies" "$work/csrf.header" <<'PY' || fail "bootstrap login did not set a CSRF cookie"
import pathlib
import sys

value = ""
for line in pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    fields = line.split("\t")
    if len(fields) == 7 and fields[5] == "lo_csrf":
        value = fields[6]
if not value or "\n" in value or "\r" in value:
    raise SystemExit("missing lo_csrf cookie")
path = pathlib.Path(sys.argv[2])
path.write_text("X-CSRF-Token: " + value + "\n", encoding="utf-8")
path.chmod(0o600)
PY

# The generated password must be replaced before any other API works.
blocked_status=$(curl --silent --show-error --max-time 5 -b "$work/cookies" \
  --output "$work/blocked.json" --write-out '%{http_code}' \
  http://127.0.0.1:3100/api/auth/tokens) || fail "password-change gate request failed"
[[ $blocked_status == 403 ]] && grep -Fq '"password_change_required"' "$work/blocked.json" || \
  fail "flagged bootstrap admin reached /api/auth/tokens (HTTP $blocked_status)"
"$python_command" - "$work/login.json" "$work/password-change.json" <<'PY' || fail "could not prepare the password change"
import json
import pathlib
import secrets
import sys

current = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))["password"]
path = pathlib.Path(sys.argv[2])
body = {"current_password": current, "new_password": secrets.token_urlsafe(24)}
path.write_text(json.dumps(body, separators=(",", ":")), encoding="utf-8")
path.chmod(0o600)
PY
curl --fail --silent --show-error --max-time 10 -b "$work/cookies" -H @"$work/csrf.header" \
  -H 'Content-Type: application/json' --data-binary @"$work/password-change.json" \
  http://127.0.0.1:3100/api/auth/password >/dev/null || fail "bootstrap admin could not change the initial password"
curl --fail --silent --show-error --max-time 5 -b "$work/cookies" \
  http://127.0.0.1:3100/api/auth/me >"$work/auth-me.json" || fail "session lost after the password change"
"$python_command" - "$work/auth-me.json" <<'PY' || fail "password change did not clear must_change_password"
import json
import pathlib
import sys

if json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")).get("must_change_password") is not False:
    raise SystemExit("must_change_password still set")
PY

curl --fail --silent --show-error --max-time 5 -b "$work/cookies" -H @"$work/csrf.header" \
  -H 'Content-Type: application/json' --data-binary '{"name":"release-smoke-read"}' \
  http://127.0.0.1:3100/api/auth/tokens | "$python_command" -c '
import json
import pathlib
import sys
token = json.load(sys.stdin).get("value", "")
if not token.startswith("lou_") or "\n" in token or "\r" in token:
    raise SystemExit("invalid user API token")
path = pathlib.Path(sys.argv[1])
path.write_text("Authorization: Bearer " + token + "\n", encoding="utf-8")
path.chmod(0o600)
' "$work/read.header" || fail "could not create a user API token for reads"
read_project_header='X-LO-Project: default'

marker="release-smoke-$server_pid"
metric_name="lazy_observe_release_smoke_metric"
trace_id=55555555555555555555555555555555
"$work/e2e-otlp-payload" -kind metrics -name "$metric_name" -value 42 >"$work/metric.pb"
"$work/e2e-otlp-payload" -kind logs -name "$marker" >"$work/log.pb"
"$python_command" - "$trace_id" >"$work/trace.json" <<'PY'
import json
import sys
import time

trace_id = sys.argv[1]
start = time.time_ns()
payload = {"resourceSpans": [{
    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "release-smoke"}}]},
    "scopeSpans": [{"scope": {"name": "release-smoke"}, "spans": [{
        "traceId": trace_id,
        "spanId": "5555555555555555",
        "name": "release-smoke",
        "kind": 1,
        "startTimeUnixNano": str(start),
        "endTimeUnixNano": str(start + 1_000_000),
        "status": {},
    }]}],
}]}
json.dump(payload, sys.stdout, separators=(",", ":"))
PY
for request in \
  "metrics|application/x-protobuf|$work/metric.pb" \
  "logs|application/x-protobuf|$work/log.pb" \
  "traces|application/json|$work/trace.json"; do
  signal=${request%%|*}
  rest=${request#*|}
  content_type=${rest%%|*}
  payload=${rest#*|}
  curl --fail --silent --show-error --max-time 10 \
    -H @"$work/token.header" -H "Content-Type: $content_type" \
    --data-binary @"$payload" "http://127.0.0.1:3100/v1/$signal" >"$work/$signal-response" \
    || fail "embedded $signal ingest failed"
done

data_ready=0
deadline=$((SECONDS + 60))
while (( SECONDS < deadline )); do
  metric_ok=0 log_ok=0 trace_ok=0
  if curl --fail --silent --max-time 5 -G -H @"$work/read.header" -H "$read_project_header" \
      --data-urlencode "query=$metric_name" --data-urlencode 'nocache=1' \
      http://127.0.0.1:3100/api/v1/query >"$work/metric-query.json" 2>/dev/null; then
    "$python_command" - "$work/metric-query.json" <<'PY' && metric_ok=1 || true
import json
import pathlib
import sys

payload = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
rows = payload.get("data", {}).get("result", [])
raise SystemExit(0 if any(float(row.get("value", [0, 0])[1]) == 42 for row in rows) else 1)
PY
  fi
  if curl --fail --silent --max-time 5 -G -H @"$work/read.header" -H "$read_project_header" \
      --data-urlencode "query=_msg:=\"$marker\"" --data-urlencode 'limit=10' \
      http://127.0.0.1:3100/api/v1/logs/query >"$work/log-query.jsonl" 2>/dev/null; then
    "$python_command" - "$work/log-query.jsonl" "$marker" <<'PY' && log_ok=1 || true
import json
import pathlib
import sys

rows = [json.loads(line) for line in pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").splitlines() if line.strip()]
marker = sys.argv[2]
raise SystemExit(0 if any(row.get("_msg") == marker and row.get("_time") for row in rows) else 1)
PY
  fi
  if curl --fail --silent --max-time 5 -H @"$work/read.header" -H "$read_project_header" \
      "http://127.0.0.1:3100/api/traces/$trace_id" >"$work/trace-query.json" 2>/dev/null; then
    "$python_command" - "$work/trace-query.json" "$trace_id" <<'PY' && trace_ok=1 || true
import json
import pathlib
import sys

payload = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
trace_id = sys.argv[2]
found = any(
    trace.get("traceID") == trace_id
    and any(
        span.get("traceID") == trace_id and span.get("spanID") and span.get("operationName")
        for span in trace.get("spans", [])
    )
    for trace in payload.get("data", [])
)
raise SystemExit(0 if found else 1)
PY
  fi
  if (( metric_ok == 1 && log_ok == 1 && trace_ok == 1 )); then
    data_ready=1
    break
  fi
  sleep 0.5
done
(( data_ready == 1 )) || fail "embedded metrics, logs, and traces did not all round-trip within 60 seconds"
unset project_api_token credentials

if ! "$python_command" - "$log" "$run_dir" "$server_pid" "$children_file" <<'PY'
import os
import pathlib
import re
import stat
import subprocess
import sys

log_path = pathlib.Path(sys.argv[1])
work = pathlib.Path(sys.argv[2]).resolve()
server_pid = int(sys.argv[3])
children_path = pathlib.Path(sys.argv[4])
log = log_path.read_text(encoding="utf-8", errors="replace")
binary_by_component = {
    "metrics": "victoria-metrics-prod",
    "logs": "victoria-logs-prod",
    "traces": "victoria-traces-prod",
    "vmagent": "vmagent-prod",
}

if "using embedded storage binaries" not in log:
    raise SystemExit("server did not report the embedded storage extraction path")
if "lazy-observe: 4 managed components healthy" not in log:
    raise SystemExit("server did not report four healthy managed components")
if "gateway listening" not in log or "address=:3100" not in log:
    raise SystemExit("server did not bind the zero-configuration gateway address :3100")

matches = re.findall(r"lazy-observe: (metrics|logs|traces|vmagent) ready pid=(\d+)", log)
seen = {}
for component, raw_pid in matches:
    if component in seen:
        raise SystemExit(f"duplicate ready record for {component}")
    seen[component] = int(raw_pid)
missing = sorted(set(binary_by_component) - set(seen))
if missing:
    raise SystemExit(f"missing managed-component ready records: {missing}")

bin_dir = work / "data" / "bin"
actual_files = {entry.name for entry in bin_dir.iterdir() if entry.is_file()}
expected_files = set(binary_by_component.values())
if actual_files != expected_files:
    raise SystemExit(f"extracted storage executables={sorted(actual_files)}, want={sorted(expected_files)}")

records = []
expected_processes = {}
for component, binary_name in binary_by_component.items():
    executable = (bin_dir / binary_name).resolve()
    info = executable.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_size <= 0 or info.st_mode & 0o111 == 0:
        raise SystemExit(f"invalid extracted executable: {executable}")
    pid = seen[component]
    ppid_result = subprocess.run(
        ["ps", "-o", "ppid=", "-p", str(pid)],
        check=False,
        capture_output=True,
        text=True,
    )
    try:
        ppid = int(ppid_result.stdout.strip())
    except ValueError:
        raise SystemExit(f"managed {component} pid {pid} is not running")
    if ppid != server_pid:
        raise SystemExit(f"managed {component} pid {pid} has ppid {ppid}, want supervisor {server_pid}")
    command_result = subprocess.run(
        ["ps", "-o", "args=", "-p", str(pid)],
        check=False,
        capture_output=True,
        text=True,
    )
    if str(executable) not in command_result.stdout:
        raise SystemExit(f"managed {component} pid {pid} is not the freshly extracted executable")
    records.append(f"{component}\t{pid}\t{executable}\n")
    expected_processes[pid] = str(executable)

process_result = subprocess.run(
    ["ps", "-eo", "pid=,args="],
    check=False,
    capture_output=True,
    text=True,
)
actual_processes = {}
expected_paths = set(expected_processes.values())
for line in process_result.stdout.splitlines():
    fields = line.strip().split(None, 1)
    if len(fields) != 2:
        continue
    try:
        pid = int(fields[0])
    except ValueError:
        continue
    matches = [path for path in expected_paths if path in fields[1]]
    if matches:
        actual_processes[pid] = matches[0]
if actual_processes != expected_processes:
    raise SystemExit(
        f"processes from the fresh extraction path={sorted(actual_processes)}, "
        f"want exactly the four supervised pids={sorted(expected_processes)}"
    )
children_path.write_text("".join(records), encoding="utf-8")
PY
then
  fail "embedded storage extraction or process supervision validation failed"
fi

smoke_commit=${LO_PERF_COMMIT:-}
if [[ -z $smoke_commit ]]; then
  smoke_commit=$(git -C "$root" rev-parse HEAD 2>/dev/null || true)
fi
if ! "$python_command" - "$children_file" "$server_pid" "$max_rss_bytes" "$smoke_output" "$smoke_commit" <<'PY'
from datetime import datetime, timezone
import json
import pathlib
import subprocess
import sys

children_path = pathlib.Path(sys.argv[1])
server_pid = int(sys.argv[2])
maximum = int(sys.argv[3])
output = sys.argv[4]
git_commit = sys.argv[5]
processes = [("gateway", server_pid)]
for line in children_path.read_text(encoding="utf-8").splitlines():
    component, raw_pid, _ = line.split("\t", 2)
    processes.append((component, int(raw_pid)))

measurements = []
for component, pid in processes:
    result = subprocess.run(
        ["ps", "-o", "rss=", "-p", str(pid)],
        check=False,
        capture_output=True,
        text=True,
    )
    try:
        rss_bytes = int(result.stdout.strip()) * 1024
    except ValueError:
        raise SystemExit(f"could not measure RSS for {component} pid {pid}")
    if rss_bytes <= 0:
        raise SystemExit(f"non-positive RSS for {component} pid {pid}")
    measurements.append({"component": component, "rss_bytes": rss_bytes})

total = sum(item["rss_bytes"] for item in measurements)
payload = {
    "version": 1,
    "git_commit": git_commit,
    "generated_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "aggregate_idle_rss_bytes": total,
    "max_idle_rss_bytes": maximum,
    "processes": measurements,
    "gate_passed": total < maximum,
}
if output:
    pathlib.Path(output).write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
if total >= maximum:
    raise SystemExit(f"aggregate idle RSS {total} bytes is not below {maximum} bytes")
print(f"PASS: aggregate idle RSS {total} bytes (< {maximum}) across gateway and four managed components")
PY
then
  fail "aggregate idle RSS gate failed"
fi

kill -TERM "$server_pid" || fail "could not send SIGTERM to release server pid $server_pid"
shutdown_deadline=$((SECONDS + shutdown_timeout_seconds))
while kill -0 "$server_pid" 2>/dev/null && (( SECONDS < shutdown_deadline )); do
  sleep 0.2
done
if kill -0 "$server_pid" 2>/dev/null; then
  fail "server did not exit within ${shutdown_timeout_seconds}s after SIGTERM"
fi
shutdown_rc=0
if wait "$server_pid"; then
  shutdown_rc=0
else
  shutdown_rc=$?
fi
server_pid=
(( shutdown_rc == 0 )) || fail "server exited with status $shutdown_rc after SIGTERM, want 0"

if ! "$python_command" - "$children_file" <<'PY'
import pathlib
import subprocess
import sys

expected = set()
for line in pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    component, raw_pid, executable = line.split("\t", 2)
    expected.add(executable)
    result = subprocess.run(
        ["ps", "-o", "args=", "-p", raw_pid],
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode == 0 and executable in result.stdout:
        raise SystemExit(f"managed {component} pid {raw_pid} survived supervisor shutdown")

result = subprocess.run(
    ["ps", "-eo", "pid=,args="],
    check=False,
    capture_output=True,
    text=True,
)
survivors = []
for line in result.stdout.splitlines():
    fields = line.strip().split(None, 1)
    if len(fields) == 2 and any(executable in fields[1] for executable in expected):
        survivors.append(fields[0])
if survivors:
    raise SystemExit(f"processes from the fresh extraction path survived supervisor shutdown: {survivors}")
PY
then
  fail "one or more embedded storage processes survived graceful shutdown"
fi

printf 'PASS: zero-configuration release smoke succeeded for %s\n' "$binary"
