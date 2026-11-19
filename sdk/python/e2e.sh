#!/bin/sh
set -eu

SDK_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-python-e2e.XXXXXX")
EXAMPLE_PID=
# 示例端口取 16620，理由同 sdk/browser/e2e.sh 顶部：低于临时端口范围和平台组件端口，高于网关端口段。
EXAMPLE_PORT=${LO_E2E_EXAMPLE_PORT:-16620}
EXAMPLE_URL="http://127.0.0.1:$EXAMPLE_PORT"
PYTHON=${LO_E2E_PYTHON:-"$SDK_DIR/.venv/bin/python"}

# 标准入口是 `mise run sdk-journeys`（scripts/sdk-journeys.sh 起平台、准备下面三个变量并依次跑四个 journey，
# CI 的 SDK journeys job 也跑它）。也可以手动对着一个运行中的 lazy-observe 平台单独执行本脚本，需要：
#   LO_E2E_PLATFORM_URL    平台地址
#   LO_E2E_TOKEN           默认项目的项目 API Token（lo_…），只用来拼 DSN 做摄取
#   LO_E2E_ADMIN_PASSWORD  admin 密码；所有读请求都用登录得到的会话 Cookie 加
#                          X-LO-Project: default（ADR 0010：项目 Token 只能摄取，读路由返回 403）
if [ -z "${LO_E2E_PLATFORM_URL:-}" ] || [ -z "${LO_E2E_TOKEN:-}" ] || [ -z "${LO_E2E_ADMIN_PASSWORD:-}" ]; then
	printf 'SDK e2e 需要 LO_E2E_PLATFORM_URL、LO_E2E_TOKEN（lo_ 摄取 Token）与 LO_E2E_ADMIN_PASSWORD（读请求用 admin 会话）指向运行中的 lazy-observe 平台\n' >&2
	exit 1
fi
PLATFORM_URL=${LO_E2E_PLATFORM_URL}
TOKEN=${LO_E2E_TOKEN}
ADMIN_PASSWORD=${LO_E2E_ADMIN_PASSWORD}

# The journey does not turn on continuous profiling: it is experimental and only samples on
# Linux + free-threaded CPython 3.15t (see docs/sdk/python.md#continuous-profiling).
cleanup() {
	if [ -n "$EXAMPLE_PID" ]; then
		kill -TERM "$EXAMPLE_PID" 2>/dev/null || true
		wait "$EXAMPLE_PID" 2>/dev/null || true
	fi
	if [ "${LO_E2E_KEEP:-0}" = 1 ]; then
		printf 'e2e artifacts kept at %s\n' "$WORK"
	else
		rm -rf "$WORK"
	fi
}
trap cleanup EXIT INT TERM

need() {
	command -v "$1" >/dev/null 2>&1 || {
		printf 'missing required command: %s\n' "$1" >&2
		exit 1
	}
}
need curl
need jq
need "$PYTHON"

assert_port_free() {
	port=$1
	name=$2
	curl_status=0
	curl -sS --connect-timeout 0.2 --max-time 0.2 "http://127.0.0.1:$port/" >/dev/null 2>&1 || curl_status=$?
	if [ "$curl_status" -ne 7 ]; then
		printf '%s port %s is already occupied; override its LO_E2E_*_PORT variable or stop the existing service\n' "$name" "$port" >&2
		exit 1
	fi
}

assert_port_free "$EXAMPLE_PORT" example

wait_http() {
	url=$1
	i=0
	while [ "$i" -lt 300 ]; do
		if curl -fsS "$url" >/dev/null 2>&1; then return 0; fi
		i=$((i + 1))
		sleep 0.1
	done
	return 1
}



jq -n --arg username admin --arg password "$ADMIN_PASSWORD" \
	'{username: $username, password: $password}' >"$WORK/login.json"
curl -fsS -c "$WORK/session.cookie" \
	-H "Content-Type: application/json" \
	--data-binary "@$WORK/login.json" "$PLATFORM_URL/api/auth/login" >/dev/null

printf 'starting instrumented Django/DRF example...\n'
PYTHONPATH="$SDK_DIR:$SDK_DIR/examples/django" \
	DJANGO_SETTINGS_MODULE=config.settings \
	LO_DSN="$PLATFORM_URL/p/default@$TOKEN" \
	LO_SERVICE_NAME="lazy-observe-django-example" \
	LO_EXPORT_INTERVAL_MS=1000 \
	PORT="$EXAMPLE_PORT" \
	"$PYTHON" "$SDK_DIR/examples/django/manage.py" runserver "127.0.0.1:$EXAMPLE_PORT" --noreload \
	>"$WORK/example.log" 2>&1 &
EXAMPLE_PID=$!
wait_http "$EXAMPLE_URL/ok" || {
	cat "$WORK/example.log" >&2
	exit 1
}

START=$(($(date +%s) - 5))
BOOM_TRACE=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DB_TRACE=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
LLM_TRACE=cccccccccccccccccccccccccccccccc

curl -fsS "$EXAMPLE_URL/ok" >/dev/null
curl -fsS "$EXAMPLE_URL/fast" >/dev/null
curl -fsS "$EXAMPLE_URL/slow" >/dev/null
curl -fsS -H "traceparent: 00-$DB_TRACE-2222222222222222-01" "$EXAMPLE_URL/db" >/dev/null
STATUS=$(curl -sS -o "$WORK/boom.body" -w '%{http_code}' \
	-H "traceparent: 00-$BOOM_TRACE-3333333333333333-01" \
	"$EXAMPLE_URL/boom")
test "$STATUS" = 500
test "$(cat "$WORK/boom.body")" = "Internal Server Error"
curl -fsS -H "traceparent: 00-$LLM_TRACE-4444444444444444-01" "$EXAMPLE_URL/chat" -o "$WORK/chat.json"
jq -e '.model == "example-model" and .choices[0].message.content == "hello from fake llm"' "$WORK/chat.json" >/dev/null

metric_ok=0
metric_contract_ok=0
log_ok=0
issue_ok=0
trace_ok=0
db_span_ok=0
llm_span_ok=0
i=0
while [ "$i" -lt 60 ]; do
	curl -fsS -G "$PLATFORM_URL/api/v1/query_range" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode 'query={__name__="http.server.request.duration_bucket"}' \
		--data-urlencode "start=$START" --data-urlencode "end=$((START + 600))" --data-urlencode 'step=1s' \
		-o "$WORK/metrics.json" || true
	if jq -e '.data.result | length > 0' "$WORK/metrics.json" >/dev/null 2>&1; then metric_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/v1/series" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode 'match[]={__name__=~"http.*|process.runtime.*"}' \
		-o "$WORK/metric-series.json" || true
	if jq -e '
		def sdk_series($name): any(.data[]?; .__name__ == $name and .["service.name"] == "lazy-observe-django-example");
		sdk_series("http.server.request.duration_count") and
		sdk_series("http.server.request.duration_bucket") and
		sdk_series("http.server.active_requests") and
		sdk_series("process.runtime.python.threads") and
		sdk_series("process.runtime.python.memory.rss") and
		sdk_series("process.runtime.python.gc.count")' \
		"$WORK/metric-series.json" >/dev/null 2>&1; then metric_contract_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/v1/logs/query" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" --data-urlencode 'query=_msg:="fast request handled"' \
		-o "$WORK/logs.jsonl" || true
	if grep -q 'service.name' "$WORK/logs.jsonl" 2>/dev/null; then log_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/issues" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode 'status=open' --data-urlencode 'query=example boom' \
		-o "$WORK/issues.json" || true
	if jq -e 'any(.issues[]?; .title == "panic: example boom: not a real outage" and .count >= 1)' \
		"$WORK/issues.json" >/dev/null 2>&1; then issue_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$BOOM_TRACE" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/boom-trace.json" || true
	if jq -e --arg id "$BOOM_TRACE" '.data[0].traceID == $id' "$WORK/boom-trace.json" >/dev/null 2>&1; then trace_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$DB_TRACE" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/db-trace.json" || true
	if jq -e --arg id "$DB_TRACE" '
		.data[0].traceID == $id and
		([.data[0].spans[]?.operationName] | index("DB query") != null)' \
		"$WORK/db-trace.json" >/dev/null 2>&1; then db_span_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$LLM_TRACE" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/llm-trace.json" || true
	if jq -e --arg id "$LLM_TRACE" '
		.data[0].traceID == $id and
		([.data[0].spans[]?.operationName] | index("chat openai") != null) and
		([.data[0].spans[]?.tags[]? | select(.key == "gen_ai.usage.input_tokens" and .value == "12")] | length == 1)' \
		"$WORK/llm-trace.json" >/dev/null 2>&1; then llm_span_ok=1; fi

	if [ "$metric_ok" = 1 ] && [ "$metric_contract_ok" = 1 ] && [ "$log_ok" = 1 ] &&
		[ "$issue_ok" = 1 ] && [ "$trace_ok" = 1 ] && [ "$db_span_ok" = 1 ] && [ "$llm_span_ok" = 1 ]; then
		break
	fi
	i=$((i + 1))
	sleep 1
done

assert_ok() {
	name=$1
	value=$2
	if [ "$value" = 1 ]; then
		printf 'ok: %s\n' "$name"
	else
		printf 'FAIL: %s\n' "$name" >&2
		return 1
	fi
}

assert_ok "metrics" "$metric_ok"
assert_ok "metric-contract" "$metric_contract_ok"
assert_ok "logs" "$log_ok"
assert_ok "issue" "$issue_ok"
assert_ok "trace" "$trace_ok"
assert_ok "db-span" "$db_span_ok"
assert_ok "llm-span" "$llm_span_ok"

printf 'e2e passed\n'
