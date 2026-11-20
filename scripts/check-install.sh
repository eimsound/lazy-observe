#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-install-check.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

for spec in Linux:x86_64:linux:amd64 Linux:aarch64:linux:arm64; do
  IFS=: read -r input_os input_arch want_os want_arch <<<"$spec"
  output=$(LO_INSTALL_DRY_RUN=1 LO_INSTALL_OS="$input_os" LO_INSTALL_ARCH="$input_arch" \
    VERSION=1.2.3 PREFIX="$work/prefix" sh "$root/scripts/install.sh")
  grep -Fx "platform=$want_os/$want_arch" <<<"$output" >/dev/null
  grep -Fx "archive=https://github.com/eimsound/lazy-observe/releases/download/v1.2.3/lazy-observe_${want_os}_${want_arch}.tar.gz" <<<"$output" >/dev/null
  grep -Fx 'checksums=https://github.com/eimsound/lazy-observe/releases/download/v1.2.3/checksums.txt' <<<"$output" >/dev/null
done

latest=$(LO_INSTALL_DRY_RUN=1 LO_INSTALL_OS=Linux LO_INSTALL_ARCH=x86_64 \
  PREFIX="$work/prefix" sh "$root/scripts/install.sh")
grep -Fx 'archive=https://github.com/eimsound/lazy-observe/releases/latest/download/lazy-observe_linux_amd64.tar.gz' \
  <<<"$latest" >/dev/null

fixture="$work/fixture"
mkdir -p "$fixture/content" "$work/fake-bin" "$work/prefix"
printf '#!/bin/sh\nprintf "fixture binary\\n"\n' >"$fixture/content/lazy-observe"
chmod 0755 "$fixture/content/lazy-observe"
tar -czf "$fixture/lazy-observe_linux_amd64.tar.gz" -C "$fixture/content" lazy-observe
archive_sha=$(sha256sum "$fixture/lazy-observe_linux_amd64.tar.gz" | awk '{print $1}')
printf '%s  lazy-observe_linux_amd64.tar.gz\n' "$archive_sha" >"$fixture/checksums.txt"

cat >"$work/fake-bin/curl" <<'EOF'
#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    http://*|https://*) url=$1; shift ;;
    *) shift ;;
  esac
done
[ -n "$output" ] && [ -n "$url" ]
cp "$FIXTURE_DIR/${url##*/}" "$output"
EOF
chmod 0755 "$work/fake-bin/curl"

FIXTURE_DIR="$fixture" PATH="$work/fake-bin:$PATH" PREFIX="$work/prefix" \
  LO_INSTALL_OS=linux LO_INSTALL_ARCH=amd64 VERSION=v1.2.3 \
  sh "$root/scripts/install.sh" >/dev/null
[[ -x $work/prefix/bin/lazy-observe ]]
[[ $("$work/prefix/bin/lazy-observe") == 'fixture binary' ]]

# 尚不存在但祖先可写的自定义 PREFIX（例如 $HOME/.local/lazy-observe）不得误触发 sudo。
cat >"$work/fake-bin/sudo" <<'EOF'
#!/bin/sh
printf 'unexpected sudo invocation: %s\n' "$*" >&2
exit 99
EOF
chmod 0755 "$work/fake-bin/sudo"
FIXTURE_DIR="$fixture" PATH="$work/fake-bin:$PATH" PREFIX="$work/new/nested/prefix" \
  LO_INSTALL_OS=linux LO_INSTALL_ARCH=amd64 VERSION=v1.2.3 \
  sh "$root/scripts/install.sh" >/dev/null
[[ -x $work/new/nested/prefix/bin/lazy-observe ]]
[[ $("$work/new/nested/prefix/bin/lazy-observe") == 'fixture binary' ]]

printf '%064d  lazy-observe_linux_amd64.tar.gz\n' 0 >"$fixture/checksums.txt"
if FIXTURE_DIR="$fixture" PATH="$work/fake-bin:$PATH" PREFIX="$work/bad-prefix" \
  LO_INSTALL_OS=linux LO_INSTALL_ARCH=amd64 VERSION=v1.2.3 \
  sh "$root/scripts/install.sh" >"$work/bad.out" 2>"$work/bad.err"; then
  printf 'installer accepted a mismatched checksum\n' >&2
  exit 1
fi
grep -F 'checksum mismatch' "$work/bad.err" >/dev/null

printf 'PASS: install script platform mapping, checksum, extraction, and mode contracts\n'
