#!/usr/bin/env bash
set -euo pipefail

# 验证 scripts/sdk-journeys.sh 被中断时不留进程、端口和临时目录（本地手动跑，不进 CI：要真实平台，约 2 分钟/次）。
#
#   mise run build && mise run storage-bins && bash scripts/sdk-journeys-interrupt-test.sh
#   bash scripts/sdk-journeys-interrupt-test.sh [TERM] [INT] [HUP]   # 默认 TERM INT
#
# 做法：TMPDIR 指到一个全新的目录，只跑 go journey（`LO_SDK_JOURNEYS_ONLY=go`），等它进入 sdk/go/e2e.sh 的
# `sleep 70`，然后**只对 sdk-journeys.sh 的 bash 进程**发信号（复现审查里的 `kill -TERM <bash pid>`，不发给整个组），
# 再检查：
#   1. sdk-journeys.sh 在 90s 内退出，退出码 = 128 + 信号；
#   2. 命令行里带这个 TMPDIR 的进程（平台、Victoria 组件、fake-llm、fiber-example……）一个都不剩；
#   3. 网关端口和 journey 的示例/LLM 端口（43320/43330）都不再监听；
#   4. TMPDIR 目录是空的（平台的 data_dir、journey 的 /tmp/lazy-observe-go-sdk-e2e.* 都被删了）。
# 任何一条不满足就非零退出，并把残留列出来。需要的 journey 前置：bin/lazy-observe、.cache/storage-bin。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"

journeys_script=${LO_SDK_JOURNEYS_SCRIPT:-scripts/sdk-journeys.sh} # 可换成别的版本做对照
signals=("$@")
((${#signals[@]} > 0)) || signals=(TERM INT)

fail() {
  printf 'sdk-journeys-interrupt-test: %s\n' "$*" >&2
  exit 1
}

for command in pgrep ps awk sed; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable: $command"
done

port_busy() {
  (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

run_case() {
  local busy_ports signal=$1 base pid out started elapsed status=0 want gateway leftovers port problems=0
  base=$(mktemp -d "${TMPDIR:-/tmp}/sdk-journeys-interrupt.XXXXXX")
  out=$base/out.log
  mkdir "$base/tmp"
  printf '\n=== case: kill -%s <sdk-journeys bash pid> ===\n' "$signal"

  # `set -m`：被测脚本在独立进程组里，且不会像非交互 bash 的普通 `&` 子进程那样忽略 SIGINT。
  set -m
  TMPDIR=$base/tmp LO_SDK_JOURNEYS_ONLY=go bash "$journeys_script" >"$out" 2>&1 </dev/null &
  pid=$!
  set +m

  started=$SECONDS
  while [[ -z $(sed -n '/waiting for at least 70 seconds/p' "$out") ]]; do
    kill -0 "$pid" 2>/dev/null || {
      cat "$out" >&2
      fail "sdk-journeys.sh exited before the go journey reached its 70s sleep"
    }
    ((SECONDS - started < 300)) || {
      cat "$out" >&2
      kill -KILL -- "-$pid" 2>/dev/null || true
      fail "timed out waiting for the go journey to reach its 70s sleep"
    }
    sleep 1
  done
  gateway=$(sed -n 's|^starting lazy-observe at http://127.0.0.1:\([0-9]*\) .*|\1|p' "$out")
  printf 'journey is in its sleep; platform gateway port %s; processes using this TMPDIR before the signal: %s\n' \
    "$gateway" "$(pgrep -f -- "$base/tmp" | wc -l)"

  kill -s "$signal" "$pid"
  started=$SECONDS
  while kill -0 "$pid" 2>/dev/null; do
    ((SECONDS - started < 90)) || {
      kill -KILL -- "-$pid" 2>/dev/null || true
      problems=$((problems + 1))
      printf 'PROBLEM: sdk-journeys.sh still running 90s after SIG%s (killed the group by hand)\n' "$signal" >&2
      break
    }
    sleep 0.5
  done
  elapsed=$((SECONDS - started))
  wait "$pid" 2>/dev/null || status=$?
  case $signal in
  TERM) want=143 ;;
  INT) want=130 ;;
  HUP) want=129 ;;
  *) want=-1 ;;
  esac
  printf 'sdk-journeys.sh exited with %s (want %s) %ss after SIG%s\n' "$status" "$want" "$elapsed" "$signal"
  ((want < 0 || status == want)) || problems=$((problems + 1))

  # 给内核一点时间回收刚被 KILL 的进程。
  sleep 1
  leftovers=$(pgrep -af -- "$base/tmp" || true)
  if [[ -n $leftovers ]]; then
    printf 'PROBLEM: leftover processes:\n%s\n' "$leftovers" >&2
    problems=$((problems + 1))
  else
    printf 'leftover processes: none\n'
  fi
  busy_ports=
  for port in "$gateway" 43320 43330; do
    if [[ -n $port ]] && port_busy "$port"; then
      busy_ports+="${busy_ports:+ }$port"
    fi
  done
  if [[ -n $busy_ports ]]; then
    printf 'PROBLEM: still listening: %s\n' "$busy_ports" >&2
    problems=$((problems + 1))
  else
    printf 'listening on %s 43320 43330: none\n' "$gateway"
  fi
  leftovers=$(cd "$base/tmp" && ls -A)
  if [[ -n $leftovers ]]; then
    printf 'PROBLEM: leftover temp entries under %s/tmp:\n%s\n' "$base" "$leftovers" >&2
    problems=$((problems + 1))
  else
    printf 'leftover temp entries: none\n'
  fi

  if ((problems > 0)); then
    printf -- '--- sdk-journeys.sh output (tail) ---\n' >&2
    tail -n 40 "$out" >&2
    # 收拾残局，免得影响下一个 case 和宿主机。
    pgrep -f -- "$base/tmp" | xargs -r kill -KILL 2>/dev/null || true
  fi
  rm -rf -- "$base"
  return "$problems"
}

total=0
for signal in "${signals[@]}"; do
  case $signal in
  TERM | INT | HUP) ;;
  *) fail "unsupported signal '$signal' (TERM, INT, HUP)" ;;
  esac
  run_case "$signal" || total=$((total + 1))
done

if ((total > 0)); then
  fail "$total case(s) left problems behind"
fi
printf '\nPASS: sdk-journeys cleans up after %s\n' "${signals[*]}"
