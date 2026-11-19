#!/bin/sh
set -eu

SDK_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-node-e2e.XXXXXX")
EXAMPLE_PID=
DISABLED_PID=
LLM_PID=
# 示例端口取 16420/16421/16430，理由同 sdk/browser/e2e.sh 顶部：低于临时端口范围和平台组件端口，高于网关端口段。
EXAMPLE_PORT=${LO_E2E_EXAMPLE_PORT:-16420}
DISABLED_PORT=${LO_E2E_DISABLED_PORT:-16421}
LLM_PORT=${LO_E2E_LLM_PORT:-16430}
EXAMPLE_URL="http://127.0.0.1:$EXAMPLE_PORT"
DISABLED_URL="http://127.0.0.1:$DISABLED_PORT"
LLM_URL="http://127.0.0.1:$LLM_PORT"

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
PASSWORD=${LO_E2E_ADMIN_PASSWORD}

cleanup() {
	if [ -n "$EXAMPLE_PID" ]; then
		kill -TERM "$EXAMPLE_PID" 2>/dev/null || true
		wait "$EXAMPLE_PID" 2>/dev/null || true
	fi
	if [ -n "$DISABLED_PID" ]; then
		kill -TERM "$DISABLED_PID" 2>/dev/null || true
		wait "$DISABLED_PID" 2>/dev/null || true
	fi
	if [ -n "$LLM_PID" ]; then
		kill -TERM "$LLM_PID" 2>/dev/null || true
		wait "$LLM_PID" 2>/dev/null || true
	fi
	if [ "${LO_E2E_KEEP:-0}" = 1 ]; then printf 'e2e artifacts kept at %s\n' "$WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT INT TERM

need() { command -v "$1" >/dev/null 2>&1 || {
	printf 'missing required command: %s\n' "$1" >&2
	exit 1
}; }
need curl
need jq
need go
need pnpm

assert_port_free() {
	port=$1
	name=$2
	if command -v nc >/dev/null 2>&1; then
		if nc -z 127.0.0.1 "$port" >/dev/null 2>&1; then
			printf '%s port %s is already occupied; override its LO_E2E_*_PORT variable or stop the existing service\n' "$name" "$port" >&2
			exit 1
		fi
	else
		curl_status=0
		curl -sS --connect-timeout 0.2 --max-time 0.2 "http://127.0.0.1:$port/" >/dev/null 2>&1 || curl_status=$?
		if [ "$curl_status" -ne 7 ]; then
			printf '%s port %s is already occupied; override its LO_E2E_*_PORT variable or stop the existing service\n' "$name" "$port" >&2
			exit 1
		fi
	fi
}
assert_port_free "$EXAMPLE_PORT" example
assert_port_free "$DISABLED_PORT" disabled-example
assert_port_free "$LLM_PORT" fake-llm

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

printf 'building Node SDK...\n'
(cd "$SDK_DIR" && pnpm install --frozen-lockfile && pnpm build)

# 读请求在 profile 断言就开始了，先登录拿会话 Cookie。
jq -n --arg username admin --arg password "$PASSWORD" \
	'{username: $username, password: $password}' >"$WORK/login.json"
curl -fsS -c "$WORK/cookies" \
	-H "Content-Type: application/json" \
	--data-binary "@$WORK/login.json" "$PLATFORM_URL/api/auth/login" >/dev/null

printf 'starting instrumented Express example...\n'
env PORT="$LLM_PORT" node "$SDK_DIR/examples/express/dist/fake-llm.js" >"$WORK/fake-llm.log" 2>&1 &
LLM_PID=$!
wait_http "$LLM_URL/healthz" || {
	cat "$WORK/fake-llm.log" >&2
	exit 1
}
env PORT="$EXAMPLE_PORT" LO_LLM_URL="$LLM_URL" LO_DSN="$PLATFORM_URL/p/default@$TOKEN" node "$SDK_DIR/examples/express/dist/main.js" >"$WORK/example.log" 2>&1 &
EXAMPLE_PID=$!
wait_http "$EXAMPLE_URL/" || {
	cat "$WORK/example.log" >&2
	exit 1
}

START=$(($(date +%s) - 5))
TRACE_ID=11111111111111111111111111111111
PARENT_ID=2222222222222222
curl -fsS "$EXAMPLE_URL/hello/e2e" >/dev/null
STATUS=$(curl -sS -o "$WORK/error.body" -w '%{http_code}' -H "traceparent: 00-$TRACE_ID-$PARENT_ID-01" "$EXAMPLE_URL/error")
test "$STATUS" = 500
LLM_STREAM_TRACE=55555555555555555555555555555555
curl -fsS -H "traceparent: 00-$LLM_STREAM_TRACE-5555555555555555-01" "$EXAMPLE_URL/llm?stream=1" >/dev/null

printf 'keeping example alive for two profiling cycles (at least 70s)...\n'
sleep 72

curl -fsS -b "$WORK/cookies" -H "X-LO-Project: default" -G "$PLATFORM_URL/api/profiles" --data-urlencode 'service=node-express-example' -o "$WORK/profiles.json"
CPU_ID=$(jq -r '.profiles | map(select(.service == "node-express-example" and .type == "cpu")) | first | .id // empty' "$WORK/profiles.json")
HEAP_ID=$(jq -r '.profiles | map(select(.service == "node-express-example" and .type == "heap")) | first | .id // empty' "$WORK/profiles.json")
test -n "$CPU_ID" || {
	printf 'node CPU profile assertion failed\n' >&2
	cat "$WORK/profiles.json" >&2
	exit 1
}
test -n "$HEAP_ID" || {
	printf 'node heap profile assertion failed\n' >&2
	cat "$WORK/profiles.json" >&2
	exit 1
}

curl -fsS -b "$WORK/cookies" -H "X-LO-Project: default" -G "$PLATFORM_URL/api/profiles/flamegraph" \
	--data-urlencode 'service=node-express-example' --data-urlencode 'type=cpu' -o "$WORK/flamegraph.json"
jq -e '.. | objects | select(.name? == "profileExampleWork")' "$WORK/flamegraph.json" >/dev/null || {
	printf 'CPU flamegraph example function assertion failed\n' >&2
	cat "$WORK/flamegraph.json" >&2
	exit 1
}
curl -fsS -b "$WORK/cookies" -H "X-LO-Project: default" "$PLATFORM_URL/api/profiles/$CPU_ID/raw" -o "$WORK/cpu.pb.gz"
go tool pprof -top "$WORK/cpu.pb.gz" >"$WORK/pprof-top.txt" 2>&1 || {
	cat "$WORK/pprof-top.txt" >&2
	exit 1
}

kill -TERM "$EXAMPLE_PID"
wait "$EXAMPLE_PID"
EXAMPLE_PID=
END=$(($(date +%s) + 5))

printf 'starting profiling-disabled example for the negative assertion...\n'
env LO_DSN="$PLATFORM_URL/p/default@$TOKEN" LO_LLM_URL="$LLM_URL" LO_CAPTURE_LLM_CONTENT=true LO_PROFILING=false LO_SERVICE_NAME=node-express-disabled PORT="$DISABLED_PORT" \
	node "$SDK_DIR/examples/express/dist/main.js" >"$WORK/disabled.log" 2>&1 &
DISABLED_PID=$!
wait_http "$DISABLED_URL/" || {
	cat "$WORK/disabled.log" >&2
	exit 1
}
LLM_CONTENT_TRACE=66666666666666666666666666666666
curl -fsS -H "traceparent: 00-$LLM_CONTENT_TRACE-6666666666666666-01" "$DISABLED_URL/llm" >/dev/null
sleep 12
curl -fsS -b "$WORK/cookies" -H "X-LO-Project: default" -G "$PLATFORM_URL/api/profiles" --data-urlencode 'service=node-express-disabled' -o "$WORK/disabled-profiles.json"
jq -e '.profiles | length == 0' "$WORK/disabled-profiles.json" >/dev/null || {
	printf 'profiling:false negative assertion failed\n' >&2
	cat "$WORK/disabled-profiles.json" >&2
	exit 1
}
kill -TERM "$DISABLED_PID"
wait "$DISABLED_PID"
DISABLED_PID=

metric_ok=0
log_ok=0
trace_ok=0
issue_ok=0
llm_stream_ok=0
llm_content_ok=0
i=0
while [ "$i" -lt 80 ]; do
	curl -fsS -G "$PLATFORM_URL/api/v1/query_range" -b "$WORK/cookies" -H "X-LO-Project: default" \
		--data-urlencode 'query={__name__="http.server.request.duration_bucket"}' --data-urlencode "start=$START" --data-urlencode "end=$END" --data-urlencode 'step=1s' -o "$WORK/metrics.json" || true
	if jq -e '.data.result | length > 0' "$WORK/metrics.json" >/dev/null 2>&1; then metric_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/v1/logs/query" -b "$WORK/cookies" -H "X-LO-Project: default" --data-urlencode 'query=_msg:="about to throw example error"' -o "$WORK/logs.jsonl" || true
	if grep -q "$TRACE_ID" "$WORK/logs.jsonl" 2>/dev/null; then log_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$TRACE_ID" -b "$WORK/cookies" -H "X-LO-Project: default" -o "$WORK/trace.json" || true
	if jq -e --arg trace "$TRACE_ID" --arg parent "$PARENT_ID" '.data[0].traceID == $trace and any(.data[0].spans[]?; any(.references[]?; .spanID == $parent and .traceID == $trace))' "$WORK/trace.json" >/dev/null 2>&1; then trace_ok=1; fi

	curl -fsS -b "$WORK/cookies" -H 'X-LO-Project: default' "$PLATFORM_URL/api/issues" -o "$WORK/issues.json" || true
	if jq -e '(.issues // .) | any(.[]?; .title == "Error: example route failed")' "$WORK/issues.json" >/dev/null 2>&1; then issue_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$LLM_STREAM_TRACE" -b "$WORK/cookies" -H "X-LO-Project: default" -o "$WORK/llm-stream-trace.json" || true
	if jq -e '
		def vals($key): [.data[0].spans[]? | select(.operationName == "chat openai") | .tags[]? | select(.key == $key) | .value];
		(vals("gen_ai.system")[0] == "openai") and
		(vals("gen_ai.provider.name")[0] == "openai") and
		(vals("gen_ai.request.model")[0] == "e2e-request-model") and
		(vals("gen_ai.response.model")[0] == "e2e-response-model") and
		((vals("gen_ai.usage.input_tokens")[0] | tostring) == "23") and
		((vals("gen_ai.usage.output_tokens")[0] | tostring) == "4") and
		(vals("gen_ai.prompt") | length == 0) and (vals("gen_ai.completion") | length == 0)' "$WORK/llm-stream-trace.json" >/dev/null 2>&1; then llm_stream_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$LLM_CONTENT_TRACE" -b "$WORK/cookies" -H "X-LO-Project: default" -o "$WORK/llm-content-trace.json" || true
	if jq -e '
		def vals($key): [.data[0].spans[]? | select(.operationName == "chat openai") | .tags[]? | select(.key == $key) | .value];
		(vals("gen_ai.system")[0] == "openai") and
		(vals("gen_ai.provider.name")[0] == "openai") and
		((vals("gen_ai.usage.input_tokens")[0] | tostring) == "23") and
		((vals("gen_ai.usage.output_tokens")[0] | tostring) == "4") and
		(vals("gen_ai.prompt")[0] | contains("e2e secret prompt")) and
		(vals("gen_ai.completion")[0] | contains("fixed completion"))' "$WORK/llm-content-trace.json" >/dev/null 2>&1; then llm_content_ok=1; fi

	if [ "$metric_ok" = 1 ] && [ "$log_ok" = 1 ] && [ "$trace_ok" = 1 ] && [ "$issue_ok" = 1 ] && [ "$llm_stream_ok" = 1 ] && [ "$llm_content_ok" = 1 ]; then break; fi
	i=$((i + 1))
	sleep 0.5
done

test "$metric_ok" = 1 || {
	printf 'metric assertion failed\n' >&2
	cat "$WORK/metrics.json" >&2
	exit 1
}
test "$log_ok" = 1 || {
	printf 'log trace_id assertion failed\n' >&2
	cat "$WORK/logs.jsonl" >&2
	exit 1
}
test "$trace_ok" = 1 || {
	printf 'traceparent assertion failed\n' >&2
	cat "$WORK/trace.json" >&2
	exit 1
}
test "$issue_ok" = 1 || {
	printf 'Issue assertion failed\n' >&2
	cat "$WORK/issues.json" >&2
	exit 1
}
test "$llm_stream_ok" = 1 || {
	printf 'streaming LLM span/default privacy assertion failed\n' >&2
	cat "$WORK/llm-stream-trace.json" >&2
	exit 1
}
test "$llm_content_ok" = 1 || {
	printf 'LLM content opt-in assertion failed\n' >&2
	cat "$WORK/llm-content-trace.json" >&2
	exit 1
}

printf 'PASS metrics: HTTP request histogram queried through gateway\n'
printf 'PASS logs: request log contains correlated trace_id\n'
printf 'PASS traces: server trace keeps browser trace ID and remote parent span ID\n'
printf 'PASS issues: Express error event generated an Issue\n'
printf 'PASS profiles: node service exposes CPU and heap records through the public profile contract\n'
printf 'PASS flamegraph: CPU tree contains profileExampleWork\n'
printf 'PASS raw profile: go tool pprof parsed downloaded CPU profile\n'
printf 'PASS profiling:false: disabled service uploaded no profiles\n'
printf 'PASS LLM streaming: GenAI model/token attributes present and prompt/completion absent by default\n'
printf 'PASS LLM content opt-in: prompt/completion attributes present when explicitly enabled\n'
