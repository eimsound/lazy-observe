#!/usr/bin/env bash
set -euo pipefail

mode=all
case $# in
  0) ;;
  1)
    [[ $1 == --sdk-go-only ]] || {
      printf 'usage: %s [--sdk-go-only]\n' "${0##*/}" >&2
      exit 2
    }
    mode=sdk-go-only
    ;;
  *)
    printf 'usage: %s [--sdk-go-only]\n' "${0##*/}" >&2
    exit 2
    ;;
esac

main_module=$(go list -m -f '{{.Path}}')

reviewed_non_go_packages=$(printf '%s\n' \
  github.com/cespare/xxhash/v2 \
  github.com/gofiber/utils/v2/simd \
  github.com/golang/snappy \
  github.com/klauspost/compress/huff0 \
  github.com/klauspost/compress/internal/cpuinfo \
  github.com/klauspost/compress/zstd \
  github.com/klauspost/compress/zstd/internal/xxhash \
  github.com/molecule-man/go-brrr/internal/encoder \
  github.com/segmentio/asm/ascii \
  github.com/segmentio/asm/base64 \
  github.com/segmentio/asm/keyset \
  golang.org/x/crypto/argon2 \
  golang.org/x/crypto/blake2b \
  golang.org/x/sys/cpu \
  golang.org/x/sys/unix \
  modernc.org/libc | sort -u)

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

check_target() {
  local label=$1 dir=$2 pkg=$3 platform=$4
  local goos=${platform%/*} goarch=${platform#*/}
  local warning_log="$workdir/${label}-${goos}-${goarch}.warnings"

  # 同步落盘到文件再回显并解析，避免 `2> >(tee ...)` 进程替换尚未刷盘就读取日志的竞态。
  if ! (
    cd "$dir"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go-licenses check "$pkg" \
        --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause \
        --ignore="$main_module" \
        2>"$warning_log"
  ); then
    cat "$warning_log" >&2
    exit 1
  fi
  cat "$warning_log" >&2

  local actual_non_go_packages unexpected
  actual_non_go_packages=$(sed -n 's/.*"\([^"]*\)" contains non-Go code.*/\1/p' "$warning_log" | sort -u)
  unexpected=$(comm -23 \
    <(printf '%s\n' "$actual_non_go_packages" | sed '/^$/d') \
    <(printf '%s\n' "$reviewed_non_go_packages"))
  if [[ -n "$unexpected" ]]; then
    printf 'Unreviewed non-Go dependency packages for %s (%s):\n%s\n' "$label" "$platform" "$unexpected" >&2
    exit 1
  fi
  printf 'PASS: %s (%s) production Go dependencies use Apache-2.0, MIT, or BSD licenses\n' "$label" "$platform"
}

if [[ $mode == all ]]; then
  # The main module (MIT, including the in-repo sdk/go module that shares its path
  # prefix) is ignored; dependencies reached through it remain in scope. Check the
  # exact two release targets because OS-specific package graphs differ.
  for platform in linux/amd64 linux/arm64; do
    check_target server . ./cmd/lazy-observe "$platform"
  done
  printf 'PASS: both production Go dependency graphs satisfy the license allowlist\n'
fi

# The Go SDK is a separate module that applications import directly, so every
# package under sdk/go must satisfy the same allowlist across release targets.
for platform in linux/amd64 linux/arm64; do
  check_target sdk-go sdk/go ./... "$platform"
done
printf 'PASS: sdk/go production Go dependencies use Apache-2.0, MIT, or BSD licenses\n'

if [[ $mode == all ]]; then
  bash scripts/third-party-notices.sh --check
fi
