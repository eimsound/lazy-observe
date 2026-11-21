#!/usr/bin/env bash
set -euo pipefail

# 启动一个带嵌入前端的真实 lazy-observe 平台（临时 SQLite data_dir、空闲端口 < 30000、
# 自动生成首次管理员密码触发强制改密），再运行 web/e2e-real 真实后端 Playwright 套件。
#
# 标准入口：`mise run web-e2e-real`（先执行 build 与 storage-bins）。
# 可选环境变量：
#   BINARY                        平台二进制路径，默认 bin/lazy-observe（不存在时兼容检查 dist/lazy-observe）
#   STORAGE_BIN_DIR               Victoria 存储二进制目录，默认 .cache/storage-bin
#   LO_WEB_REAL_E2E_READY_TIMEOUT 等 /readyz 就绪的秒数，默认 120
#   LO_E2E_KEEP=1                 保留临时目录（含 server.log）

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"

ready_timeout=${LO_WEB_REAL_E2E_READY_TIMEOUT:-120}
shutdown_timeout=45
runner_stop_timeout=15

fail() {
  printf 'e2e-web-real: %s\n' "$*" >&2
  exit 1
}

case $ready_timeout in
'' | *[!0-9]*) fail "LO_WEB_REAL_E2E_READY_TIMEOUT must be a positive integer" ;;
esac

for command in curl node pnpm sed pgrep ps awk; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable: $command"
done

binary_input=${BINARY:-}
if [[ -z $binary_input ]]; then
  if [[ -x $root/bin/lazy-observe ]]; then
    binary_input=bin/lazy-observe
  elif [[ -x $root/dist/lazy-observe ]]; then
    binary_input=dist/lazy-observe
  else
    binary_input=bin/lazy-observe
  fi
fi
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

work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-web-real-e2e.XXXXXX")
mkdir "$work/cwd" "$work/home" "$work/data"
log=$work/server.log
server_pid=
runner_pid=
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

group_alive() {
  local alive
  alive=$(ps -A -o pgid=,stat= 2>/dev/null | awk -v g="$1" '$1 == g && $2 !~ /^Z/ { print "alive" }' || true)
  [[ -n $alive ]]
}

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
      printf 'e2e-web-real: %s did not exit within %ss of SIGTERM, sending SIGKILL to its process group\n' \
        "$name" "$timeout" >&2
      kill -KILL -- "-$pgid" 2>/dev/null || true
    fi
  fi
  wait "$pgid" 2>/dev/null || true
}

cleanup() {
  local rc=$? leftovers
  trap '' HUP INT TERM
  trap - EXIT
  stop_group "$runner_pid" "$runner_stop_timeout" "playwright"
  stop_group "$server_pid" "$shutdown_timeout" "platform"
  leftovers=$(pgrep -f -- "$work/data" || true)
  if [[ -n $leftovers ]]; then
    # shellcheck disable=SC2086 # pgrep 输出以空白分隔的 PID
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
    printf 'e2e-web-real artifacts kept at %s\n' "$work" >&2
  else
    rm -rf -- "$work"
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

port_busy() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# 网关端口取 15000–15999，组件端口偏移取 1000–6999（最大组件端口 22428+6999=29427 < 30000），
# 均低于 Linux 临时端口段（32768–60999），并避开 sdk-journeys 的 14000–14999 及组件默认端口。
gateway_port=
port_offset=
for _ in $(seq 50); do
  candidate_gateway=$((15000 + RANDOM % 1000))
  candidate_offset=$((1000 + RANDOM % 6000))
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
[[ -n $gateway_port ]] || fail "could not find free gateway and component ports (< 30000) after 50 attempts"
platform_url=http://127.0.0.1:$gateway_port
admin_password_file=$work/data/initial-admin-password

printf 'preparing web Playwright dependencies...\n'
(
  cd "$root/web"
  CI=true pnpm install --frozen-lockfile
  if [[ $(id -u) == 0 ]] || sudo -n true >/dev/null 2>&1; then
    apt_cmd=(apt-get)
    if [[ $(id -u) != 0 ]]; then
      apt_cmd=(sudo -n apt-get)
    fi
    if command -v apt-get >/dev/null 2>&1; then
      "${apt_cmd[@]}" update -qq >/dev/null 2>&1 || true
    fi
    if ! pnpm exec playwright install --with-deps --only-shell chromium; then
      printf 'warning: playwright install --with-deps failed; retrying after apt-get update and falling back to browser-only install\n' >&2
      if command -v apt-get >/dev/null 2>&1; then
        "${apt_cmd[@]}" update -qq >/dev/null 2>&1 || true
      fi
      pnpm exec playwright install --with-deps --only-shell chromium ||
        pnpm exec playwright install --only-shell chromium
    fi
  else
    pnpm exec playwright install --only-shell chromium
  fi
)

printf 'starting lazy-observe at %s (component port offset %s)...\n' "$platform_url" "$port_offset"
# 不设置 LO_SECURITY_ADMIN_PASSWORD_FILE，使平台首次启动自动生成 initial-admin-password 并开启强制改密。
set -m
(
  cd "$work/cwd"
  exec env -i HOME="$work/home" PATH="$PATH" TMPDIR="${TMPDIR:-/tmp}" LANG="${LANG:-C.UTF-8}" \
    LO_SELF_MONITORING_ENABLED=false \
    "$binary" server --listen "127.0.0.1:$gateway_port" --data-dir "$work/data" \
    --storage-bin-dir "$storage" --component-port-offset "$port_offset"
) >"$log" 2>&1 </dev/null &
server_pid=$!
set +m

ready=0
deadline=$((SECONDS + ready_timeout))
while ((SECONDS < deadline)); do
  kill -0 "$server_pid" 2>/dev/null || fail "platform exited before becoming ready"
  if curl --fail --silent --show-error --max-time 2 "$platform_url/healthz" >/dev/null 2>&1 &&
    curl --fail --silent --show-error --max-time 2 "$platform_url/readyz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.2
done
((ready == 1)) || fail "timed out after ${ready_timeout}s waiting for $platform_url/healthz and /readyz"

[[ -s $admin_password_file ]] || fail "initial admin password file was not generated: $admin_password_file"

printf 'running real-backend Playwright E2E suite against %s...\n' "$platform_url"
started=$SECONDS
set -m
(
  cd "$root/web"
  exec env \
    LO_REAL_E2E_BASE_URL="$platform_url" \
    LO_REAL_E2E_ADMIN_PASSWORD_FILE="$admin_password_file" \
    pnpm test:e2e:real "$@"
) </dev/null &
runner_pid=$!
set +m

status=0
wait "$runner_pid" || status=$?
if group_alive "$runner_pid"; then
  stop_group "$runner_pid" "$runner_stop_timeout" "playwright leftovers"
fi
runner_pid=
elapsed=$((SECONDS - started))

if ((status != 0)); then
  fail "real-backend Playwright E2E failed (exit $status, ${elapsed}s)"
fi
printf 'PASS: web real-backend E2E suite (%ss)\n' "$elapsed"
