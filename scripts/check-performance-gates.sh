#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

mode=${1:-all}
go_command=${GO:-go}
config=${LO_PERF_GATE_CONFIG:-bench/performance-gates.json}
binary=${LO_PERF_BINARY:-bin/lazy-observe}
output_dir=${LO_PERF_OUTPUT_DIR:-}
skipped=0

if ! command -v "$go_command" >/dev/null 2>&1; then
  printf 'SKIP: Go command %s is unavailable; performance gates were not run\n' "$go_command" >&2
  exit 77
fi
platform=${LO_PERF_PLATFORM:-$($go_command env GOOS)-$($go_command env GOARCH)}

skip() {
  printf 'SKIP: %s\n' "$*" >&2
  skipped=1
}

OUTPUT_ARGS=()
prepare_output() {
  local name=$1
  OUTPUT_ARGS=()
  if [[ -n $output_dir ]]; then
    if ! mkdir -p "$output_dir"; then
      printf 'FAIL: could not create performance result directory: %s\n' "$output_dir" >&2
      return 1
    fi
    OUTPUT_ARGS=(--output "$output_dir/$name.json")
  fi
}

run_release_size() {
	prepare_output release-size
  CGO_ENABLED=0 "$go_command" run ./cmd/perf-gate release-size \
		--config "$config" --binary "$binary" --platform "$platform" "${OUTPUT_ARGS[@]}"
}

run_query() {
	# The query gate only reads, so it uses the user API token (LO_PERF_QUERY_TOKEN)
	# plus LO_PERF_PROJECT (default "default"); project tokens are ingest-only.
	if [[ -z ${LO_PERF_BASE_URL:-} || -z ${LO_PERF_QUERY_TOKEN:-} || -z ${LO_PERF_ISSUE_ID:-} || \
	      -z ${LO_PERF_LOG_MARKER:-} || -z ${LO_PERF_TRACE_ID:-} || -z ${LO_PERF_TRACE_SERVICE:-} ]]; then
		skip 'query gate requires LO_PERF_BASE_URL, LO_PERF_QUERY_TOKEN (user API token), LO_PERF_ISSUE_ID, LO_PERF_LOG_MARKER, LO_PERF_TRACE_ID, and LO_PERF_TRACE_SERVICE'
		return
	fi
	prepare_output query
	local range_args=()
	[[ -z ${LO_PERF_LOG_START:-} ]] || range_args+=(--log-start "$LO_PERF_LOG_START")
	[[ -z ${LO_PERF_LOG_END:-} ]] || range_args+=(--log-end "$LO_PERF_LOG_END")
  CGO_ENABLED=0 "$go_command" run ./cmd/perf-gate query \
		--config "$config" --base-url "$LO_PERF_BASE_URL" \
		--token-env LO_PERF_QUERY_TOKEN --project "${LO_PERF_PROJECT:-default}" --trace-id "$LO_PERF_TRACE_ID" --trace-service "$LO_PERF_TRACE_SERVICE" \
		--issue-id "$LO_PERF_ISSUE_ID" --log-marker "$LO_PERF_LOG_MARKER" \
		--min-issues "${LO_PERF_MIN_ISSUES:-50}" --min-log-rows "${LO_PERF_MIN_LOG_ROWS:-1000}" \
		--log-limit "${LO_PERF_LOG_LIMIT:-1000}" "${range_args[@]}" "${OUTPUT_ARGS[@]}"
}

run_scale() {
  # LO_PERF_TOKEN is the project ingest token (POSTs only); LO_PERF_QUERY_TOKEN is a
  # user API token used with LO_PERF_PROJECT for identity and readback GETs.
  if [[ -z ${LO_PERF_SCALE_TARGETS:-} || -z ${LO_PERF_SCALE_READBACK_TARGETS:-} || -z ${LO_PERF_CONTROLLED_UPSTREAM:-} || \
        -z ${LO_PERF_TOKEN:-} || -z ${LO_PERF_QUERY_TOKEN:-} ]]; then
    skip 'scale gate requires LO_PERF_SCALE_TARGETS, LO_PERF_SCALE_READBACK_TARGETS, LO_PERF_CONTROLLED_UPSTREAM, LO_PERF_TOKEN (project ingest token), and LO_PERF_QUERY_TOKEN (user API token)'
    return
  fi
	prepare_output scale
  CGO_ENABLED=0 "$go_command" run ./cmd/perf-gate scale \
		--config "$config" --targets "$LO_PERF_SCALE_TARGETS" \
		--readback-targets "$LO_PERF_SCALE_READBACK_TARGETS" \
		--token-env LO_PERF_TOKEN --query-token-env LO_PERF_QUERY_TOKEN --project "${LO_PERF_PROJECT:-default}" \
		--controlled-upstream "$LO_PERF_CONTROLLED_UPSTREAM" "${OUTPUT_ARGS[@]}"
}

case "$mode" in
  release-size) run_release_size ;;
  query)
    run_query
    (( skipped == 0 )) || exit 77
    ;;
  scale)
    run_scale
    (( skipped == 0 )) || exit 77
    ;;
  all)
    run_release_size
    run_query
    run_scale
    if (( skipped != 0 )); then
      printf 'SKIP: one or more performance gates were not executed; overall result is not PASS\n' >&2
      exit 77
    fi
    ;;
  *)
    printf 'usage: %s [all|release-size|query|scale]\n' "$0" >&2
    exit 2
    ;;
esac
