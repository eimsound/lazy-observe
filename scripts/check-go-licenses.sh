#!/usr/bin/env bash
set -euo pipefail

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

# The main module (MIT, including the in-repo sdk/go module that shares its path
# prefix) is ignored; dependencies reached through it remain in scope. Check the
# exact two release targets because OS-specific package graphs differ.
for platform in linux/amd64 linux/arm64; do
  goos=${platform%/*}
  goarch=${platform#*/}
  warning_log="$workdir/${goos}-${goarch}.warnings"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go-licenses check ./cmd/lazy-observe \
      --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause \
      --ignore="$main_module" \
      2> >(tee "$warning_log" >&2)

  actual_non_go_packages=$(sed -n 's/.*"\([^"]*\)" contains non-Go code.*/\1/p' "$warning_log" | sort -u)
  unexpected=$(comm -23 \
    <(printf '%s\n' "$actual_non_go_packages" | sed '/^$/d') \
    <(printf '%s\n' "$reviewed_non_go_packages"))
  if [[ -n "$unexpected" ]]; then
    printf 'Unreviewed non-Go dependency packages for %s:\n%s\n' "$platform" "$unexpected" >&2
    exit 1
  fi
  printf 'PASS: %s production Go dependencies use Apache-2.0, MIT, or BSD licenses\n' "$platform"
done

printf 'PASS: both production Go dependency graphs satisfy the license allowlist\n'

# The Go SDK is a separate module that applications import directly, so its own
# production dependency graph must satisfy the same allowlist.
(
  cd sdk/go
  CGO_ENABLED=0 go-licenses check . \
    --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause \
    --ignore="$main_module" 2>"$workdir/sdk-go.warnings" || {
    cat "$workdir/sdk-go.warnings" >&2
    exit 1
  }
)
printf 'PASS: sdk/go production Go dependencies use Apache-2.0, MIT, or BSD licenses\n'

bash scripts/third-party-notices.sh --check
