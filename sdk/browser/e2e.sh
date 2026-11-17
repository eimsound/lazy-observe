#!/bin/sh
set -eu

SDK_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-browser-e2e.XXXXXX")
VANILLA_PID=
REACT_PID=
GO_PID=
NODE_PID=
CPU_LOAD_PID=
VANILLA_PORT=${LO_E2E_VANILLA_PORT:-43520}
REACT_PORT=${LO_E2E_REACT_PORT:-43530}
GO_PORT=${LO_E2E_GO_PORT:-43540}
NODE_PORT=${LO_E2E_NODE_PORT:-43550}
VANILLA_URL="http://127.0.0.1:$VANILLA_PORT"
REACT_URL="http://127.0.0.1:$REACT_PORT"
GO_URL="http://127.0.0.1:$GO_PORT"
NODE_URL="http://127.0.0.1:$NODE_PORT"
ERROR_JOURNEY_RELEASE=browser-error-journey-v1

# SDK e2e 需要 LO_E2E_PLATFORM_URL 与 LO_E2E_TOKEN 指向的 lazy-observe 服务端
if [ -z "${LO_E2E_PLATFORM_URL:-}" ] || [ -z "${LO_E2E_TOKEN:-}" ]; then
	printf 'SDK e2e 需要 LO_E2E_PLATFORM_URL 与 LO_E2E_TOKEN 指向的 lazy-observe 服务端\n' >&2
	exit 1
fi
PLATFORM_URL=${LO_E2E_PLATFORM_URL}
TOKEN=${LO_E2E_TOKEN}
PASSWORD=${LO_E2E_ADMIN_PASSWORD:-}

cleanup() {
	if [ -n "$CPU_LOAD_PID" ]; then
		kill -TERM "$CPU_LOAD_PID" 2>/dev/null || true
		wait "$CPU_LOAD_PID" 2>/dev/null || true
	fi
	if [ -n "$REACT_PID" ]; then
		kill -TERM "$REACT_PID" 2>/dev/null || true
		wait "$REACT_PID" 2>/dev/null || true
	fi
	if [ -n "$VANILLA_PID" ]; then
		kill -TERM "$VANILLA_PID" 2>/dev/null || true
		wait "$VANILLA_PID" 2>/dev/null || true
	fi
	if [ -n "$NODE_PID" ]; then
		kill -TERM "$NODE_PID" 2>/dev/null || true
		wait "$NODE_PID" 2>/dev/null || true
	fi
	if [ -n "$GO_PID" ]; then
		kill -TERM "$GO_PID" 2>/dev/null || true
		wait "$GO_PID" 2>/dev/null || true
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
need go
need node
need pnpm
need rg

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
assert_port_free "$VANILLA_PORT" vanilla-example
assert_port_free "$REACT_PORT" react-example
assert_port_free "$GO_PORT" go-example
assert_port_free "$NODE_PORT" node-example

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


printf 'building SDK workspace and examples from this repo...\n'
(cd "$SDK_DIR/../go" && CGO_ENABLED=0 go build -o "$WORK/fiber-example" ./examples/fiber)
(cd "$SDK_DIR/../node" && pnpm install --frozen-lockfile && pnpm build)
(
	cd "$SDK_DIR"
	pnpm install --frozen-lockfile
	pnpm build
	pnpm exec playwright install --with-deps --only-shell chromium
)


printf 'starting instrumented Fiber error-journey endpoint...\n'
env PORT="$GO_PORT" LO_DSN="$PLATFORM_URL/p/default@$TOKEN" \
	"$WORK/fiber-example" >"$WORK/fiber-example.log" 2>&1 &
GO_PID=$!
wait_http "$GO_URL/" || {
	sed -n '1,200p' "$WORK/fiber-example.log" >&2
	exit 1
}

printf 'starting instrumented Express endpoint and concurrent profiling workloads...\n'
env PORT="$NODE_PORT" LO_DSN="$PLATFORM_URL/p/default@$TOKEN" \
	node "$SDK_DIR/../node/examples/express/dist/main.js" >"$WORK/node-example.log" 2>&1 &
NODE_PID=$!
wait_http "$NODE_URL/" || {
	sed -n '1,200p' "$WORK/node-example.log" >&2
	exit 1
}

(
	i=0
	while [ "$i" -lt 48 ]; do
		curl -fsS "$GO_URL/cpu" >/dev/null || exit 1
		i=$((i + 1))
	done
) &
CPU_LOAD_PID=$!

printf 'generating a bounded fast/slow/error performance workload...\n'
i=0
while [ "$i" -lt 6 ]; do
	curl -fsS "$GO_URL/performance/fast" >/dev/null
	curl -fsS "$GO_URL/performance/slow" >/dev/null
	curl -fsS "$NODE_URL/performance/fast" >/dev/null
	curl -fsS "$NODE_URL/performance/slow" >/dev/null
	test "$(curl -sS -o /dev/null -w '%{http_code}' "$GO_URL/performance/error")" = 503
	test "$(curl -sS -o /dev/null -w '%{http_code}' "$NODE_URL/performance/error")" = 503
	i=$((i + 1))
done
for delay in 400 700 950; do
	curl -fsS "$GO_URL/performance/slow?delay_ms=$delay" >/dev/null
	curl -fsS "$NODE_URL/performance/slow?delay_ms=$delay" >/dev/null
done
test "$(curl -sS -o /dev/null -w '%{http_code}' "$GO_URL/performance/error")" = 503
test "$(curl -sS -o /dev/null -w '%{http_code}' "$NODE_URL/performance/error")" = 503

printf 'building the production React fixture and uploading its source maps...\n'
(
	cd "$SDK_DIR"
	LO_E2E_DSN="$PLATFORM_URL/p/default@$TOKEN" \
		LO_E2E_RELEASE="$ERROR_JOURNEY_RELEASE" \
		LO_E2E_STRIP_SOURCEMAPS=1 \
		pnpm --filter @lazy-observe/example-react build
)
if find "$SDK_DIR/examples/react/dist" -name '*.map' -print -quit | rg -q .; then
	printf 'production React fixture still exposes source maps after upload\n' >&2
	exit 1
fi

printf 'starting vanilla dev and React production examples...\n'
(cd "$SDK_DIR" && pnpm --filter @lazy-observe/example-vanilla exec vite --host 127.0.0.1 --port "$VANILLA_PORT") >"$WORK/vanilla.log" 2>&1 &
VANILLA_PID=$!
(cd "$SDK_DIR" && LO_E2E_JOURNEY_TARGET="$GO_URL" pnpm --filter @lazy-observe/example-react exec vite preview --host 127.0.0.1 --port "$REACT_PORT") >"$WORK/react.log" 2>&1 &
REACT_PID=$!
wait_http "$VANILLA_URL/" || {
	sed -n '1,160p' "$WORK/vanilla.log" >&2
	exit 1
}
wait_http "$REACT_URL/" || {
	sed -n '1,160p' "$WORK/react.log" >&2
	exit 1
}

printf 'running real Chromium signal and persistence assertions...\n'
(
	cd "$SDK_DIR"
	LO_E2E_PLATFORM_URL="$PLATFORM_URL" \
		LO_E2E_VANILLA_URL="$VANILLA_URL" \
		LO_E2E_REACT_URL="$REACT_URL" \
		LO_E2E_GO_URL="$GO_URL" \
		LO_E2E_NODE_URL="$NODE_URL" \
		LO_E2E_TOKEN="$TOKEN" \
		LO_E2E_ADMIN_PASSWORD="$PASSWORD" \
		LO_E2E_ERROR_JOURNEY_RELEASE="$ERROR_JOURNEY_RELEASE" \
		pnpm test:e2e "$@"
)
