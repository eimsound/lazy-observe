#!/usr/bin/env bash
set -euo pipefail

# 起一个真实的 lazy-observe 平台，依次跑四个客户端 SDK 的 journey（sdk/{go,node,python,browser}/e2e.sh）。
# 入口是 `mise run sdk-journeys`（先构建带前端的二进制、下载 storage 二进制、准备 Python venv）。
#
# 平台：临时 data_dir（新 SQLite 元数据库）、空 cwd、白名单环境变量、空闲的网关端口与组件端口偏移，
# 所以不会碰到宿主机上正在运行的平台或默认端口。
# 凭据：通过 security.admin_password_file 提供一个已知的 admin 密码（这样首次登录不强制改密），
# 默认项目的 token 只在首次创建时打一次到日志里，从日志读出来。token 只用于摄取，所有读请求
# 由 journey 用 admin 会话 Cookie 加 X-LO-Project 发出（ADR 0010）。
#
# 工具链缺失一律报错退出（不 skip）：CI 必须能发现 journey 没跑。
#
# 可选环境变量：
#   BINARY / STORAGE_BIN_DIR          平台二进制与 storage 二进制目录，默认 bin/lazy-observe、.cache/storage-bin
#   LO_SDK_JOURNEYS_ONLY              逗号分隔的子集，例如 go,browser；默认 go,node,python,browser（按此顺序）
#   LO_SDK_JOURNEYS_READY_TIMEOUT     等 /readyz 的秒数，默认 120
#   LO_E2E_KEEP=1                     保留临时目录（journey 自己的临时目录也由它控制）
# 顺序不能换成并行：python 与 browser 的示例默认都从 43520 起，串行才不冲突。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"

all_journeys=(go node python browser)
ready_timeout=${LO_SDK_JOURNEYS_READY_TIMEOUT:-120}
shutdown_timeout=45

fail() {
  printf 'sdk-journeys: %s\n' "$*" >&2
  exit 1
}

case $ready_timeout in
'' | *[!0-9]*) fail "LO_SDK_JOURNEYS_READY_TIMEOUT must be a positive integer" ;;
esac

journeys=()
if [[ -n ${LO_SDK_JOURNEYS_ONLY:-} ]]; then
  IFS=',' read -r -a requested <<<"$LO_SDK_JOURNEYS_ONLY"
  for name in "${requested[@]}"; do
    [[ " ${all_journeys[*]} " == *" $name "* ]] ||
      fail "unknown journey '$name' in LO_SDK_JOURNEYS_ONLY (known: ${all_journeys[*]})"
  done
  # 按固定顺序跑，与请求里的顺序无关。
  for name in "${all_journeys[@]}"; do
    [[ ",$LO_SDK_JOURNEYS_ONLY," == *",$name,"* ]] && journeys+=("$name")
  done
else
  journeys=("${all_journeys[@]}")
fi
((${#journeys[@]} > 0)) || fail "no journey selected"

for command in curl jq go node pnpm rg od tr sed pgrep; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable: $command"
done

binary_input=${BINARY:-bin/lazy-observe}
storage_input=${STORAGE_BIN_DIR:-.cache/storage-bin}
case $binary_input in
/*) binary=$binary_input ;;
*) binary=$root/$binary_input ;;
esac
case $storage_input in
/*) storage=$storage_input ;;
*) storage=$root/$storage_input ;;
esac
[[ -x $binary ]] || fail "platform binary is missing or not executable: $binary (run mise run build)"
[[ -d $storage ]] || fail "storage binary directory is missing: $storage (run mise run storage-bins)"
if [[ " ${journeys[*]} " == *" python "* ]]; then
  [[ -x $root/sdk/python/.venv/bin/python ]] ||
    fail "sdk/python/.venv is missing (run mise run sdk-python)"
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-sdk-journeys.XXXXXX")
mkdir "$work/cwd" "$work/home"
log=$work/server.log
server_pid=
journey_pid=
log_dumped=0

dump_log() {
  if [[ $log_dumped -eq 0 && -s $log ]]; then
    log_dumped=1
    printf '%s\n' '--- lazy-observe platform log (last 200 lines) ---' >&2
    # tail 读完文件才结束，sed 读到 EOF，管道两端都不会提前退出。
    tail -n 200 "$log" | sed -E 's/(password|project_api_token)=[^ ]+/\1=[REDACTED]/g' >&2 || true
    printf '%s\n' '--- end lazy-observe platform log ---' >&2
  fi
}

stop_pid() {
  local pid=$1 deadline
  [[ -n $pid ]] || return 0
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    deadline=$((SECONDS + shutdown_timeout))
    while kill -0 "$pid" 2>/dev/null && ((SECONDS < deadline)); do
      sleep 0.2
    done
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

cleanup() {
  local rc=$? leftovers
  trap - EXIT HUP INT TERM
  # 先停正在跑的 journey（它自己的 trap 会清理示例进程），再停平台。
  stop_pid "$journey_pid"
  stop_pid "$server_pid"
  # 最后兜底：平台的受管子进程（VictoriaMetrics 等）的命令行都带着本次的临时 data_dir，
  # 按这个唯一路径匹配，不会误伤宿主机上别的 Victoria 进程。
  leftovers=$(pgrep -f -- "$work/data" || true)
  if [[ -n $leftovers ]]; then
    # shellcheck disable=SC2086 # pgrep 输出的是以空白分隔的 PID
    kill -TERM $leftovers 2>/dev/null || true
    sleep 1
    leftovers=$(pgrep -f -- "$work/data" || true)
    # shellcheck disable=SC2086
    [[ -z $leftovers ]] || kill -KILL $leftovers 2>/dev/null || true
  fi
  if ((rc != 0)); then
    dump_log
  fi
  if [[ ${LO_E2E_KEEP:-0} == 1 ]]; then
    printf 'sdk-journeys artifacts kept at %s\n' "$work" >&2
  else
    rm -rf -- "$work"
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# bash 的 /dev/tcp 能连上就说明端口已被占用。
port_busy() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# 网关端口与四个受管组件端口（默认 18428/19428/20428/22428，再加偏移）都要空闲。
gateway_port=
port_offset=
for _ in $(seq 50); do
  candidate_gateway=$((44000 + RANDOM % 1000))
  candidate_offset=$((1000 + RANDOM % 9000))
  busy=0
  for port in "$candidate_gateway" $((18428 + candidate_offset)) $((19428 + candidate_offset)) \
    $((20428 + candidate_offset)) $((22428 + candidate_offset)); do
    if port_busy "$port"; then
      busy=1
      break
    fi
  done
  if ((busy == 0)); then
    gateway_port=$candidate_gateway
    port_offset=$candidate_offset
    break
  fi
done
[[ -n $gateway_port ]] || fail "could not find free gateway and component ports after 50 attempts"
platform_url=http://127.0.0.1:$gateway_port

# 32 个十六进制字符，满足密码最短长度；od 自己读够 16 字节就结束，不会因管道提前关闭而 SIGPIPE。
admin_password=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
(umask 077 && printf '%s\n' "$admin_password" >"$work/admin-password")

printf 'starting lazy-observe at %s (component port offset %s)...\n' "$platform_url" "$port_offset"
(
  cd "$work/cwd"
  # 空 cwd + 白名单环境：宿主机上的 LO_* 配置、云凭据都不会流进平台。
  exec env -i HOME="$work/home" PATH="$PATH" TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-C.UTF-8}" \
    LO_SECURITY_ADMIN_PASSWORD_FILE="$work/admin-password" \
    LO_SELF_MONITORING_ENABLED=false \
    "$binary" server --listen "127.0.0.1:$gateway_port" --data-dir "$work/data" \
    --storage-bin-dir "$storage" --component-port-offset "$port_offset"
) >"$log" 2>&1 &
server_pid=$!

ready=0
deadline=$((SECONDS + ready_timeout))
while ((SECONDS < deadline)); do
  kill -0 "$server_pid" 2>/dev/null || fail "platform exited before /readyz became ready"
  if curl --fail --silent --show-error --max-time 2 "$platform_url/readyz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.2
done
((ready == 1)) || fail "timed out after ${ready_timeout}s waiting for $platform_url/readyz"

# 默认项目的 token 只在首次创建时打一次日志。
tokens=$(sed -n 's/.*created default project.*project_api_token=\([^ ]*\).*/\1/p' "$log")
project_token=${tokens%%$'\n'*}
[[ -n $project_token && $project_token == "$tokens" ]] ||
  fail "expected exactly one default project API token in the platform log"
[[ $project_token == lo_* ]] || fail "default project API token does not start with lo_"

# 先确认已知密码能登录且不强制改密：否则 journey 的读请求会被 403，失败原因会很难看。
login_body=$work/login.json
(umask 077 && jq -n --arg password "$admin_password" '{username: "admin", $password}' >"$login_body")
curl --fail --silent --show-error --max-time 10 -c "$work/cookies" \
  -H 'Content-Type: application/json' --data-binary @"$login_body" \
  "$platform_url/api/auth/login" >/dev/null || fail "admin login with the admin_password_file password failed"
curl --fail --silent --show-error --max-time 10 -b "$work/cookies" \
  "$platform_url/api/auth/me" >"$work/me.json" || fail "admin session validation failed"
jq -e '.must_change_password != true and ((.user.must_change_password // false) != true)' "$work/me.json" >/dev/null ||
  fail "admin is forced to change its password; journeys would be blocked"
rm -f -- "$login_body" "$work/cookies" "$work/me.json"

export LO_E2E_PLATFORM_URL=$platform_url
export LO_E2E_TOKEN=$project_token
export LO_E2E_ADMIN_PASSWORD=$admin_password

passed=()
failed=()
for name in "${journeys[@]}"; do
  printf '\n=== SDK journey: %s ===\n' "$name"
  started=$SECONDS
  # 放到后台再 wait：收到 TERM/INT 时 trap 能立刻跑，并转发给 journey 脚本。
  sh "$root/sdk/$name/e2e.sh" &
  journey_pid=$!
  status=0
  wait "$journey_pid" || status=$?
  journey_pid=
  elapsed=$((SECONDS - started))
  if ((status == 0)); then
    passed+=("$name")
    printf '=== PASS SDK journey: %s (%ss) ===\n' "$name" "$elapsed"
  else
    failed+=("$name")
    printf '=== FAIL SDK journey: %s (exit %s, %ss) ===\n' "$name" "$status" "$elapsed" >&2
  fi
done

printf '\nSDK journeys passed: %s\n' "${passed[*]:-none}"
if ((${#failed[@]} > 0)); then
  fail "SDK journeys failed: ${failed[*]}"
fi
printf 'PASS: all SDK journeys (%s)\n' "${journeys[*]}"
