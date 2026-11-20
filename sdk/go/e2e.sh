#!/bin/sh
set -eu

SDK_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-go-sdk-e2e.XXXXXX")
EXAMPLE_PID=
LLM_PID=
# 示例端口取 16320/16330，理由同 sdk/browser/e2e.sh 顶部：低于临时端口范围和平台组件端口，高于网关端口段。
EXAMPLE_PORT=${LO_E2E_EXAMPLE_PORT:-16320}
LLM_PORT=${LO_E2E_LLM_PORT:-16330}
EXAMPLE_URL="http://127.0.0.1:$EXAMPLE_PORT"
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
ADMIN_PASSWORD=${LO_E2E_ADMIN_PASSWORD}

cleanup() {
	if [ -n "$EXAMPLE_PID" ]; then
		kill -TERM "$EXAMPLE_PID" 2>/dev/null || true
		wait "$EXAMPLE_PID" 2>/dev/null || true
	fi
	if [ -n "$LLM_PID" ]; then
		kill -TERM "$LLM_PID" 2>/dev/null || true
		wait "$LLM_PID" 2>/dev/null || true
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
need go

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


printf 'building SDK e2e examples (fake-llm, fiber-example)...\n'
(cd "$SDK_DIR/examples/fiber" && CGO_ENABLED=0 go build -o "$WORK/fake-llm" ./fakellm)
jq -n --arg username admin --arg password "$ADMIN_PASSWORD" \
	'{username: $username, password: $password}' >"$WORK/login.json"
curl -fsS -c "$WORK/session.cookie" \
	-H "Content-Type: application/json" \
	--data-binary "@$WORK/login.json" "$PLATFORM_URL/api/auth/login" >/dev/null

printf 'starting instrumented Fiber example...\n'
(cd "$SDK_DIR/examples/fiber" && CGO_ENABLED=0 go build -o "$WORK/fiber-example" .)
env PORT="$LLM_PORT" "$WORK/fake-llm" >"$WORK/fake-llm.log" 2>&1 &
LLM_PID=$!
wait_http "$LLM_URL/healthz" || {
	cat "$WORK/fake-llm.log" >&2
	exit 1
}
env PORT="$EXAMPLE_PORT" LO_LLM_URL="$LLM_URL" LO_DSN="$PLATFORM_URL/p/default@$TOKEN" "$WORK/fiber-example" >"$WORK/example.log" 2>&1 &
EXAMPLE_PID=$!
wait_http "$EXAMPLE_URL/" || {
	cat "$WORK/example.log" >&2
	exit 1
}

# Keep named application work hot during the first CPU sampling window.
(
	i=0
	while [ "$i" -lt 48 ]; do
		curl -fsS "$EXAMPLE_URL/cpu" >/dev/null || exit 1
		i=$((i + 1))
	done
) &
CPU_LOAD_PID=$!

START=$(($(date +%s) - 5))
TRACE_ID=11111111111111111111111111111111
curl -fsS "$EXAMPLE_URL/db" >/dev/null
STATUS=$(curl -sS -o "$WORK/panic.body" -w '%{http_code}' \
	-H "traceparent: 00-$TRACE_ID-2222222222222222-01" \
	"$EXAMPLE_URL/panic")
test "$STATUS" = 500
ORDER_TRACE=55555555555555555555555555555555
curl -fsS -H "traceparent: 00-$ORDER_TRACE-5555555555555555-01" \
	"$EXAMPLE_URL/trace/order-e2e" -o "$WORK/order-trace-response.json"
jq -e --arg id "$ORDER_TRACE" \
	'.order_id == "order-e2e" and .items == 3 and .total == 126 and .trace_id == $id' \
	"$WORK/order-trace-response.json" >/dev/null
"$WORK/fiber-example" load --url "$EXAMPLE_URL" --duration 2s --concurrency 4 >"$WORK/load.log"
LLM_STREAM_TRACE=33333333333333333333333333333333
curl -fsS -H "traceparent: 00-$LLM_STREAM_TRACE-3333333333333333-01" "$EXAMPLE_URL/llm?stream=1" >/dev/null

wait "$CPU_LOAD_PID"
printf 'waiting for at least 70 seconds of continuous profiling...\n'
sleep 70

profile_ok=0
i=0
while [ "$i" -lt 30 ]; do
	curl -fsS -G "$PLATFORM_URL/api/profiles" -b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode 'service=lazy-observe-fiber-example' -o "$WORK/profiles.json" || true
	if jq -e '[.profiles[].type] | index("cpu") != null and index("heap") != null and index("goroutine") != null' "$WORK/profiles.json" >/dev/null 2>&1; then
		profile_ok=1
		break
	fi
	i=$((i + 1))
	sleep 1
done
test "$profile_ok" = 1 || {
	printf 'cpu+heap+goroutine profile assertion failed\n' >&2
	cat "$WORK/profiles.json" >&2
	exit 1
}

curl -fsS -G "$PLATFORM_URL/api/profiles/flamegraph" \
	-b "$WORK/session.cookie" -H "X-LO-Project: default" --data-urlencode 'service=lazy-observe-fiber-example' \
	--data-urlencode 'type=cpu' -o "$WORK/flamegraph.json"
jq -e '.. | objects | select(.name? == "main.cpuWork")' "$WORK/flamegraph.json" >/dev/null

PROFILE_ID=$(jq -r '[.profiles[] | select(.type == "cpu")] | sort_by(.id) | .[0].id' "$WORK/profiles.json")
curl -fsS "$PLATFORM_URL/api/profiles/$PROFILE_ID/raw" -b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/profile.pb.gz"
go tool pprof -top "$WORK/profile.pb.gz" >"$WORK/pprof-top.txt"
grep -q 'main.cpuWork' "$WORK/pprof-top.txt"

PROFILE_COUNT=$(jq '[.profiles[] | select(.service == "lazy-observe-fiber-example")] | length' "$WORK/profiles.json")

kill -TERM "$EXAMPLE_PID"
wait "$EXAMPLE_PID"
EXAMPLE_PID=
END=$(($(date +%s) + 5))

printf 'starting profiling-disabled example for reverse assertion...\n'
env PORT="$EXAMPLE_PORT" LO_LLM_URL="$LLM_URL" LO_DSN="$PLATFORM_URL/p/default@$TOKEN" LO_PROFILING=false LO_CAPTURE_LLM_CONTENT=true "$WORK/fiber-example" >"$WORK/example-disabled.log" 2>&1 &
EXAMPLE_PID=$!
wait_http "$EXAMPLE_URL/" || {
	cat "$WORK/example-disabled.log" >&2
	exit 1
}
LLM_CONTENT_TRACE=44444444444444444444444444444444
curl -fsS -H "traceparent: 00-$LLM_CONTENT_TRACE-4444444444444444-01" "$EXAMPLE_URL/llm" >/dev/null
sleep 12
kill -TERM "$EXAMPLE_PID"
wait "$EXAMPLE_PID"
EXAMPLE_PID=
curl -fsS -G "$PLATFORM_URL/api/profiles" -b "$WORK/session.cookie" -H "X-LO-Project: default" \
	--data-urlencode 'service=lazy-observe-fiber-example' -o "$WORK/profiles-disabled.json"
test "$(jq '[.profiles[] | select(.service == "lazy-observe-fiber-example")] | length' "$WORK/profiles-disabled.json")" = "$PROFILE_COUNT"

metric_ok=0
metric_contract_ok=0
log_ok=0
issue_ok=0
trace_ok=0
order_trace_ok=0
performance_ok=0
llm_stream_ok=0
llm_content_ok=0
i=0
while [ "$i" -lt 60 ]; do
	curl -fsS -G "$PLATFORM_URL/api/v1/query_range" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode 'query={__name__="http.server.request.duration_bucket"}' \
		--data-urlencode "start=$START" --data-urlencode "end=$END" --data-urlencode 'step=1s' \
		-o "$WORK/metrics.json" || true
	if jq -e '.data.result | length > 0' "$WORK/metrics.json" >/dev/null 2>&1; then metric_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/v1/series" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode 'match[]={__name__=~"http.*|process.runtime.*"}' \
		-o "$WORK/metric-series.json" || true
	if jq -e '
		def sdk_series($name): any(.data[]?; .__name__ == $name and .["service.name"] == "lazy-observe-fiber-example");
		sdk_series("http.server.request.duration_count") and
		sdk_series("http.server.request.duration_bucket") and
		sdk_series("http.server.active_requests") and
		sdk_series("process.runtime.go.goroutines") and
		sdk_series("process.runtime.go.memory.heap_alloc") and
		sdk_series("process.runtime.go.gc.count") and
		(all(.data[]? | select(.["service.name"] == "lazy-observe-fiber-example"); has("instance") | not))' \
		"$WORK/metric-series.json" >/dev/null 2>&1; then metric_contract_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/v1/logs/query" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" --data-urlencode 'query=_msg:="panic recovered"' \
		-o "$WORK/logs.jsonl" || true
	if grep -q "$TRACE_ID" "$WORK/logs.jsonl" 2>/dev/null; then log_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/issues" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode 'status=open' --data-urlencode 'query=example panic' \
		-o "$WORK/issues.json" || true
	if jq -e 'any(.issues[]?; .title == "panic: example panic" and .count >= 1)' \
		"$WORK/issues.json" >/dev/null 2>&1; then issue_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$TRACE_ID" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/trace.json" || true
	if jq -e --arg id "$TRACE_ID" '.data[0].traceID == $id' "$WORK/trace.json" >/dev/null 2>&1; then trace_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$ORDER_TRACE" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/order-trace.json" || true
	if jq -e --arg id "$ORDER_TRACE" '
		.data[0].traceID == $id and
		([.data[0].spans[]?.operationName] | index("GET /trace/:orderID") != null) and
		([.data[0].spans[]?.operationName] | index("validate order") != null) and
		([.data[0].spans[]?.operationName] | index("load order") != null) and
		([.data[0].spans[]?.operationName] | index("calculate order total") != null)' \
		"$WORK/order-trace.json" >/dev/null 2>&1; then order_trace_ok=1; fi

	curl -fsS -G "$PLATFORM_URL/api/v1/performance/endpoints" \
		-b "$WORK/session.cookie" -H "X-LO-Project: default" \
		--data-urlencode "start=$START" --data-urlencode "end=$END" --data-urlencode 'step=1' \
		-o "$WORK/performance.json" || true
	if jq -e '
		def endpoint($route): [.endpoints[]? | select(.service == "lazy-observe-fiber-example" and .route == $route)][0];
		(endpoint("/performance/fast").throughput > 0) and
		(endpoint("/performance/slow").p95 > endpoint("/performance/fast").p95) and
		(endpoint("/performance/error").error_rate == 1)' \
		"$WORK/performance.json" >/dev/null 2>&1; then performance_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$LLM_STREAM_TRACE" -b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/llm-stream-trace.json" || true
	if jq -e '
		def vals($key): [.data[0].spans[]? | select(.operationName == "chat openai") | .tags[]? | select(.key == $key) | .value];
		(vals("gen_ai.system")[0] == "openai") and
		(vals("gen_ai.provider.name")[0] == "openai") and
		(vals("gen_ai.request.model")[0] == "e2e-request-model") and
		(vals("gen_ai.response.model")[0] == "e2e-response-model") and
		((vals("gen_ai.usage.input_tokens")[0] | tostring) == "23") and
		((vals("gen_ai.usage.output_tokens")[0] | tostring) == "4") and
		(vals("gen_ai.prompt") | length == 0) and (vals("gen_ai.completion") | length == 0)' "$WORK/llm-stream-trace.json" >/dev/null 2>&1; then llm_stream_ok=1; fi

	curl -sS "$PLATFORM_URL/api/traces/$LLM_CONTENT_TRACE" -b "$WORK/session.cookie" -H "X-LO-Project: default" -o "$WORK/llm-content-trace.json" || true
	if jq -e '
		def vals($key): [.data[0].spans[]? | select(.operationName == "chat openai") | .tags[]? | select(.key == $key) | .value];
		(vals("gen_ai.system")[0] == "openai") and
		(vals("gen_ai.provider.name")[0] == "openai") and
		((vals("gen_ai.usage.input_tokens")[0] | tostring) == "23") and
		((vals("gen_ai.usage.output_tokens")[0] | tostring) == "4") and
		(vals("gen_ai.prompt")[0] | contains("e2e secret prompt")) and
		(vals("gen_ai.completion")[0] | contains("fixed completion"))' "$WORK/llm-content-trace.json" >/dev/null 2>&1; then llm_content_ok=1; fi

	if [ "$metric_ok" = 1 ] && [ "$metric_contract_ok" = 1 ] && [ "$log_ok" = 1 ] && [ "$issue_ok" = 1 ] && [ "$trace_ok" = 1 ] && [ "$order_trace_ok" = 1 ] && [ "$performance_ok" = 1 ] && [ "$llm_stream_ok" = 1 ] && [ "$llm_content_ok" = 1 ]; then break; fi
	i=$((i + 1))
	sleep 0.5
done

test "$metric_ok" = 1 || {
	printf 'metric assertion failed\n' >&2
	cat "$WORK/metrics.json" >&2
	exit 1
}
test "$metric_contract_ok" = 1 || {
	printf 'SDK metric name/label contract assertion failed\n' >&2
	cat "$WORK/metric-series.json" >&2
	exit 1
}
test "$log_ok" = 1 || {
	printf 'log trace_id assertion failed\n' >&2
	cat "$WORK/logs.jsonl" >&2
	exit 1
}
test "$issue_ok" = 1 || {
	printf 'panic Issue assertion failed\n' >&2
	cat "$WORK/issues.json" >&2
	exit 1
}
test "$trace_ok" = 1 || {
	printf 'trace assertion failed\n' >&2
	cat "$WORK/trace.json" >&2
	exit 1
}
test "$order_trace_ok" = 1 || {
	printf 'nested order trace assertion failed\n' >&2
	cat "$WORK/order-trace.json" >&2
	exit 1
}
test "$performance_ok" = 1 || {
	printf 'performance endpoint aggregation assertion failed\n' >&2
	cat "$WORK/performance.json" >&2
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

printf 'PASS metrics: http.server.request.duration histogram queried through gateway query_range\n'
printf 'PASS SDK metric contract: SDK metric names carry service.name and no instance label in VictoriaMetrics\n'
printf 'PASS logs: panic log and trace_id queried through gateway\n'
printf 'PASS issues: Fiber panic exported and aggregated through the real error-event gateway\n'
printf 'PASS traces: same trace_id queried through gateway /api/traces route\n'
printf 'PASS nested traces: application and database work share the incoming order trace\n'
printf 'PASS performance: fast, slow, and failed routes aggregate into throughput, percentiles, and error rate\n'
printf 'PASS profiles: cpu+heap+goroutine records, merged flamegraph main.* frame, and go tool pprof raw parsing\n'
printf 'PASS profiling disabled: no profile records added during reverse assertion window\n'
printf 'PASS LLM streaming: GenAI model/token attributes present and prompt/completion absent by default\n'
printf 'PASS LLM content opt-in: prompt/completion attributes present when explicitly enabled\n'
