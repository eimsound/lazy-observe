#!/bin/sh
set -eu

SDK_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-browser-e2e.XXXXXX")
VANILLA_PID=
REACT_PID=
GO_PID=
NODE_PID=
CPU_LOAD_PID=
# 示例端口取 16520–16550：低于 Linux 临时端口范围（32768–60999），不会和本机出站连接抢；低于平台组件端口
# （19428+ 再加 scripts/sdk-journeys.sh 的随机偏移），高于网关端口段 14000–14999，也避开 HA 脚本等其它固定端口。
# go/node/python journey 用同一段里的 16320–16330、16420–16430、16620，四个 journey 互不重叠。
# 两个 vite 命令带 --strictPort：端口被占就直接失败，不会悄悄换端口让后面的请求打错地方。
VANILLA_PORT=${LO_E2E_VANILLA_PORT:-16520}
REACT_PORT=${LO_E2E_REACT_PORT:-16530}
GO_PORT=${LO_E2E_GO_PORT:-16540}
NODE_PORT=${LO_E2E_NODE_PORT:-16550}
VANILLA_URL="http://127.0.0.1:$VANILLA_PORT"
REACT_URL="http://127.0.0.1:$REACT_PORT"
GO_URL="http://127.0.0.1:$GO_PORT"
NODE_URL="http://127.0.0.1:$NODE_PORT"
ERROR_JOURNEY_RELEASE=browser-error-journey-v1

# 标准入口是 `mise run sdk-journeys`（scripts/sdk-journeys.sh 起平台、准备下面三个变量并依次跑四个 journey，
# CI 的 SDK journeys job 也跑它）。也可以手动对着一个运行中的 lazy-observe 平台单独执行本脚本，需要：
#   LO_E2E_PLATFORM_URL    平台地址
#   LO_E2E_TOKEN           默认项目的项目 API Token（lo_…），只用来拼 DSN 做摄取
#   LO_E2E_ADMIN_PASSWORD  admin 密码；所有读请求都用登录得到的会话 Cookie 加
#                          X-LO-Project: default（ADR 0010：项目 Token 只能摄取，读路由返回 403）。
#                          上传 Source Map 用的 los_ Token 也由脚本用这个会话通过管理 API 创建（ADR 0018）
if [ -z "${LO_E2E_PLATFORM_URL:-}" ] || [ -z "${LO_E2E_TOKEN:-}" ] || [ -z "${LO_E2E_ADMIN_PASSWORD:-}" ]; then
	printf 'SDK e2e 需要 LO_E2E_PLATFORM_URL、LO_E2E_TOKEN（lo_ 摄取 Token）与 LO_E2E_ADMIN_PASSWORD（读请求用 admin 会话）指向运行中的 lazy-observe 平台\n' >&2
	exit 1
fi
PLATFORM_URL=${LO_E2E_PLATFORM_URL}
TOKEN=${LO_E2E_TOKEN}
PASSWORD=${LO_E2E_ADMIN_PASSWORD}

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
need jq

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


pnpm_install_frozen() {
	store_dir=$1
	shift
	if [ -n "$store_dir" ]; then
		pnpm install --frozen-lockfile --store-dir "$store_dir" "$@"
	else
		pnpm install --frozen-lockfile "$@"
	fi
}

printf 'building SDK workspace and examples from this repo...\n'
(cd "$SDK_DIR/../go/examples/fiber" && CGO_ENABLED=0 go build -o "$WORK/fiber-example" .)
(cd "$SDK_DIR/../node" && pnpm_install_frozen "${LO_PNPM_STORE_SDK_NODE:-}" && pnpm build)
# --with-deps 要用 apt 装系统库，需要 root 或免密 sudo（CI runner 满足）；
# 托管 runner 偶发 apt 索引过期或镜像半同步导致 fonts-unifont 等字体包找不到，先容错刷新索引并允许失败重试/降级到仅装浏览器，
# 系统库若真缺失，后续启动 Chromium 会如实报错。没有 root/免密 sudo 的开发机不调用 apt/sudo，只装浏览器本身。
install_playwright_chromium() {
	if [ "$(id -u)" = 0 ] || sudo -n true >/dev/null 2>&1; then
		apt_prefix=
		if [ "$(id -u)" != 0 ]; then
			apt_prefix="sudo -n"
		fi
		if command -v apt-get >/dev/null 2>&1; then
			# shellcheck disable=SC2086 # 有意按空白拆分 sudo -n 前缀
			$apt_prefix apt-get update -qq >/dev/null 2>&1 || true
		fi
		if ! pnpm exec playwright install --with-deps --only-shell chromium; then
			printf 'warning: playwright install --with-deps failed; retrying after apt-get update and falling back to browser-only install\n' >&2
			if command -v apt-get >/dev/null 2>&1; then
				# shellcheck disable=SC2086 # 有意按空白拆分 sudo -n 前缀
				$apt_prefix apt-get update -qq >/dev/null 2>&1 || true
			fi
			pnpm exec playwright install --with-deps --only-shell chromium ||
				pnpm exec playwright install --only-shell chromium
		fi
	else
		pnpm exec playwright install --only-shell chromium
	fi
}
(
	cd "$SDK_DIR"
	# bundler-plugin 的 bin（lazy-observe-sourcemaps）指向 dist/cli.js，examples/react 依赖它；
	# dist 还没构建就整体 install，pnpm 会报 "Failed to create bin"。先只装并构建 bundler-plugin，
	# 再整体 install，bin 链接就是完整的，发布产物（package.json 的 bin / files）不用动。
	pnpm_install_frozen "${LO_PNPM_STORE_SDK_BROWSER:-}" --filter @lazy-observe/bundler-plugin
	pnpm --filter @lazy-observe/bundler-plugin build
	pnpm_install_frozen "${LO_PNPM_STORE_SDK_BROWSER:-}"
	pnpm build
	install_playwright_chromium
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

# ADR 0018: 上传 Source Map 要用项目级的 los_ 上传 Token，项目 API Token（lo_）会得到 403。
# 登录 admin，通过管理 API 为默认项目创建上传 Token；值只在创建响应里出现一次。
printf 'creating a source map upload token through the management API...\n'
JAR="$WORK/admin-cookies"
jq -n --arg password "$PASSWORD" '{username: "admin", $password}' >"$WORK/login.json"
curl -fsS --max-time 10 -c "$JAR" -H 'Content-Type: application/json' \
	--data-binary @"$WORK/login.json" "$PLATFORM_URL/api/auth/login" >/dev/null
CSRF=$(sed -n 's/.*[[:space:]]lo_csrf[[:space:]]*//p' "$JAR" | head -n 1)
PROJECT_ID=$(curl -fsS --max-time 10 -b "$JAR" "$PLATFORM_URL/api/projects" |
	jq -r '.[] | select(.slug == "default") | .id')
test -n "$PROJECT_ID"
UPLOAD_TOKEN=$(curl -fsS --max-time 10 -b "$JAR" -H "X-CSRF-Token: $CSRF" \
	-H 'Content-Type: application/json' --data '{"name":"browser-e2e"}' \
	"$PLATFORM_URL/api/projects/$PROJECT_ID/source-map-upload-tokens" | jq -r '.value')
case "$UPLOAD_TOKEN" in
los_*) ;;
*)
	printf 'management API did not issue a los_ source map upload token\n' >&2
	exit 1
	;;
esac
printf '{"version":3,"sources":[],"names":[],"mappings":""}' >"$WORK/probe.js.map"
status=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
	-H "Authorization: Bearer $TOKEN" -F "probe.js=@$WORK/probe.js.map;filename=probe.js.map" \
	"$PLATFORM_URL/api/releases/rejected-by-project-token/sourcemaps")
if [ "$status" != 403 ]; then
	printf 'project API token upload returned %s, want 403 source_map_upload_token_required\n' "$status" >&2
	exit 1
fi

printf 'building the production React fixture and uploading its source maps...\n'
(
	cd "$SDK_DIR"
	LO_E2E_DSN="$PLATFORM_URL/p/default@$UPLOAD_TOKEN" \
		LO_E2E_RELEASE="$ERROR_JOURNEY_RELEASE" \
		LO_E2E_STRIP_SOURCEMAPS=1 \
		pnpm --filter @lazy-observe/example-react build
)
if find "$SDK_DIR/examples/react/dist" -name '*.map' -print -quit | rg -q .; then
	printf 'production React fixture still exposes source maps after upload\n' >&2
	exit 1
fi

printf 'starting vanilla dev and React production examples...\n'
(cd "$SDK_DIR" && pnpm --filter @lazy-observe/example-vanilla exec vite --host 127.0.0.1 --port "$VANILLA_PORT" --strictPort) >"$WORK/vanilla.log" 2>&1 &
VANILLA_PID=$!
(cd "$SDK_DIR" && LO_E2E_JOURNEY_TARGET="$GO_URL" pnpm --filter @lazy-observe/example-react exec vite preview --host 127.0.0.1 --port "$REACT_PORT" --strictPort) >"$WORK/react.log" 2>&1 &
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
