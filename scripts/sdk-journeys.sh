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
#   LO_SDK_JOURNEYS_ONLY              逗号或空格分隔的子集，例如 go,browser 或 "go browser"（CI 传的是 ci-changes 的
#                                     sdk_journeys_only，空格分隔）；空 = 默认 go,node,python,browser（按此顺序）
#   LO_SDK_JOURNEYS_READY_TIMEOUT     等 /readyz 的秒数，默认 120
#   LO_E2E_KEEP=1                     保留临时目录（journey 自己的临时目录也由它控制）
# 顺序不能换成并行：四个 journey 的示例端口虽已互不重叠（16320–16620），但共用同一个平台，没有为并行设计。
#
# 中断清理：平台和每个 journey 各占一个独立进程组（`set -m` 下起的后台任务，组 ID 即其 PID）。
# 收到 TERM/INT/HUP 或正常退出时，cleanup 对整个组先 TERM，到期（平台 45s、journey 15s）再 KILL。
# 不能只对 journey 的 sh 发信号：dash 等前台命令（例如 `sleep 70`）结束才跑 trap，示例进程会被孤立；
# 非交互 bash 里 `&` 起的子进程还会忽略 SIGINT。journey 的临时目录（TMPDIR）放在 $work/journeys 下，
# 即使 journey 被 KILL 来不及自己清理，也会随 $work 一起删掉。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"

all_journeys=(go node python browser)
ready_timeout=${LO_SDK_JOURNEYS_READY_TIMEOUT:-120}
shutdown_timeout=45 # 平台：等它自己优雅停掉受管的 Victoria 组件
journey_stop_timeout=15 # journey：只有示例进程和临时目录要收拾

fail() {
  printf 'sdk-journeys: %s\n' "$*" >&2
  exit 1
}

case $ready_timeout in
'' | *[!0-9]*) fail "LO_SDK_JOURNEYS_READY_TIMEOUT must be a positive integer" ;;
esac

journeys=()
if [[ -n ${LO_SDK_JOURNEYS_ONLY:-} ]]; then
  IFS=', ' read -r -a requested <<<"$LO_SDK_JOURNEYS_ONLY"
  for name in "${requested[@]}"; do
    [[ " ${all_journeys[*]} " == *" $name "* ]] ||
      fail "unknown journey '$name' in LO_SDK_JOURNEYS_ONLY (known: ${all_journeys[*]})"
  done
  # 按固定顺序跑，与请求里的顺序无关。
  for name in "${all_journeys[@]}"; do
    [[ " ${requested[*]} " == *" $name "* ]] && journeys+=("$name")
  done
else
  journeys=("${all_journeys[@]}")
fi
((${#journeys[@]} > 0)) || fail "no journey selected"

for command in curl jq go node pnpm rg od tr sed pgrep ps awk; do
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
mkdir "$work/cwd" "$work/home" "$work/journeys" "$work/data"
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

# 进程组里是否还有活着的成员。已退出但还没被 wait 的僵尸（Z）不算：组长是本脚本的子进程，
# 没 wait 之前它一直是僵尸，`kill -0 -- -PGID` 会因此一直成功。
group_alive() {
  local alive
  alive=$(ps -A -o pgid=,stat= 2>/dev/null | awk -v g="$1" '$1 == g && $2 !~ /^Z/ { print "alive" }' || true)
  [[ -n $alive ]]
}

# stop_group PGID TIMEOUT NAME：先对整个进程组 TERM，到期还在就 KILL 整个组，最后回收组长。
# PGID 就是 `set -m` 下起的后台任务的 PID；组里还有成员时这个 ID 不会被复用。
stop_group() {
  local pgid=$1 timeout=$2 name=$3 deadline
  [[ -n $pgid ]] || return 0
  if group_alive "$pgid"; then
    kill -TERM -- "-$pgid" 2>/dev/null || true
    deadline=$((SECONDS + timeout))
    while group_alive "$pgid" && ((SECONDS < deadline)); do
      sleep 0.2
    done
    if group_alive "$pgid"; then
      printf 'sdk-journeys: %s did not exit within %ss of SIGTERM, sending SIGKILL to its process group\n' \
        "$name" "$timeout" >&2
      kill -KILL -- "-$pgid" 2>/dev/null || true
    fi
  fi
  wait "$pgid" 2>/dev/null || true
}

cleanup() {
  local rc=$? leftovers
  # 清理期间再来的信号不能打断清理（子进程会继承忽略，但它们此刻只是 sleep/ps/kill）。
  trap '' HUP INT TERM
  trap - EXIT
  # 先停正在跑的 journey 的整个进程组（示例、构建、浏览器），再停平台的整个进程组。
  stop_group "$journey_pid" "$journey_stop_timeout" "journey"
  stop_group "$server_pid" "$shutdown_timeout" "platform"
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
# 网关端口取 14000–14999：低于 Linux 临时端口范围（32768–60999），不会和本机出站连接抢；
# 低于组件端口的下限（18428+1000），所以不会与同一次运行的组件撞车；仓库其它脚本的固定端口
# （HA 的 23101/23102、29090–29492、30428–30492，release smoke 的 42880，SDK journey 示例的 16320–16620，
# 以及组件默认端口 18428–22428）都不在这个区间里。
gateway_port=
port_offset=
platform_url=

pick_free_ports() {
  gateway_port=
  port_offset=
  local _attempt candidate_gateway candidate_offset busy port
  for _attempt in $(seq 50); do
    candidate_gateway=$((14000 + RANDOM % 1000))
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
}

# start_platform DATA_DIR LOG_FILE ENV_KV...
start_platform() {
  local data_dir=$1 log_file=$2
  shift 2
  local ready=0 deadline
  log=$log_file
  log_dumped=0
  pick_free_ports
  mkdir -p "$data_dir"
  printf 'starting lazy-observe at %s (component port offset %s)...\n' "$platform_url" "$port_offset"
  # 平台独占一个进程组（set -m 只包住这一次 fork，组 ID = server_pid），cleanup 按组停掉它和受管的 Victoria 组件。
  set -m
  (
    cd "$work/cwd"
    # 空 cwd + 白名单环境：宿主机上的 LO_* 配置、云凭据都不会流进平台。
    exec env -i HOME="$work/home" PATH="$PATH" TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-C.UTF-8}" \
      LO_SECURITY_ADMIN_PASSWORD_FILE="$work/admin-password" \
      "$@" \
      "$binary" server --listen "127.0.0.1:$gateway_port" --data-dir "$data_dir" \
      --storage-bin-dir "$storage" --component-port-offset "$port_offset"
  ) >"$log" 2>&1 </dev/null &
  server_pid=$!
  set +m

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
}

# 32 个十六进制字符，满足密码最短长度；od 自己读够 16 字节就结束，不会因管道提前关闭而 SIGPIPE。
admin_password=$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')
(umask 077 && printf '%s\n' "$admin_password" >"$work/admin-password")

start_platform "$work/data/main" "$work/server.log" LO_SELF_MONITORING_ENABLED=false

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
  # 放到后台再 wait：收到 TERM/INT 时 trap 能立刻跑。`set -m` 让它独占一个进程组（组 ID = journey_pid），
  # cleanup 对整个组发信号，示例、构建和浏览器都跟着停；TMPDIR 指到 $work/journeys，journey 被 KILL 时
  # 它自己的临时目录也会随 $work 删掉。stdin 接 /dev/null，后台组读终端会被 SIGTTIN 停住。
  set -m
  TMPDIR=$work/journeys sh "$root/sdk/$name/e2e.sh" </dev/null &
  journey_pid=$!
  set +m
  status=0
  wait "$journey_pid" || status=$?
  # journey 正常退出后，组里若还留着示例进程，也一并收掉（并留一条警告，说明 journey 自己没清干净）。
  if group_alive "$journey_pid"; then
    printf 'sdk-journeys: journey %s left processes behind, stopping its process group\n' "$name" >&2
    stop_group "$journey_pid" "$journey_stop_timeout" "journey $name leftovers"
  fi
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

run_web_self_monitoring_e2e() {
  local mode=$1 data_dir=$2 log_file=$3 web_flag=$4 started status elapsed
  start_platform "$data_dir" "$log_file" \
    LO_SELF_MONITORING_ENABLED=true \
    "LO_SELF_MONITORING_WEB=$web_flag" \
    LO_SELF_MONITORING_WEB_REPLAY=false
  printf '\n=== Web self-monitoring E2E (%s) ===\n' "$mode"
  started=$SECONDS
  set -m
  LO_SELF_MON_MODE=$mode \
    LO_SELF_MON_URL=$platform_url \
    LO_SELF_MON_ADMIN_PASSWORD=$admin_password \
    node "$root/scripts/e2e-web-self-monitoring.mjs" </dev/null &
  journey_pid=$!
  set +m
  status=0
  wait "$journey_pid" || status=$?
  if group_alive "$journey_pid"; then
    printf 'sdk-journeys: web self-monitoring (%s) left processes behind, stopping its process group\n' "$mode" >&2
    stop_group "$journey_pid" "$journey_stop_timeout" "web self-monitoring ($mode) leftovers"
  fi
  journey_pid=
  elapsed=$((SECONDS - started))
  if ((status != 0)); then
    fail "web self-monitoring E2E ($mode) failed (exit $status, ${elapsed}s)"
  fi
  printf '=== PASS Web self-monitoring E2E: %s (%ss) ===\n' "$mode" "$elapsed"
  stop_group "$server_pid" "$shutdown_timeout" "platform (web self-monitoring $mode)"
  server_pid=
}

if [[ " ${journeys[*]} " == *" browser "* ]]; then
  stop_group "$server_pid" "$shutdown_timeout" "platform"
  server_pid=
  run_web_self_monitoring_e2e enabled "$work/data/self-mon-enabled" "$work/self-mon-enabled.log" true
  run_web_self_monitoring_e2e disabled "$work/data/self-mon-disabled" "$work/self-mon-disabled.log" false
fi

printf 'PASS: all SDK journeys (%s)\n' "${journeys[*]}"
