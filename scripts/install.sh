#!/bin/sh
set -eu

# Release archives are GitHub Release assets; /releases/latest/download follows
# the latest non-prerelease without calling the rate-limited GitHub API.
base_url=${LO_INSTALL_BASE_URL:-https://github.com}
base_url=${base_url%/}
repo=${REPO:-eimsound/lazy-observe}
version=${VERSION:-latest}
prefix=${PREFIX:-/usr/local}
os_name=${LO_INSTALL_OS:-$(uname -s)}
arch_name=${LO_INSTALL_ARCH:-$(uname -m)}

case "$os_name" in
  Linux|linux) os=linux ;;
  *) printf 'unsupported operating system: %s\n' "$os_name" >&2; exit 1 ;;
esac

case "$arch_name" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) printf 'unsupported architecture: %s\n' "$arch_name" >&2; exit 1 ;;
esac

asset="lazy-observe_${os}_${arch}.tar.gz"
if [ "$version" = latest ]; then
  release_url="$base_url/$repo/releases/latest/download"
else
  case "$version" in v*) tag=$version ;; *) tag=v$version ;; esac
  release_url="$base_url/$repo/releases/download/$tag"
fi
archive_url="$release_url/$asset"
checksums_url="$release_url/checksums.txt"
destination="$prefix/bin/lazy-observe"

if [ "${LO_INSTALL_DRY_RUN:-0}" = 1 ]; then
  printf 'platform=%s/%s\narchive=%s\nchecksums=%s\ndestination=%s\n' \
    "$os" "$arch" "$archive_url" "$checksums_url" "$destination"
  exit 0
fi

for command_name in curl tar install awk; do
  command -v "$command_name" >/dev/null 2>&1 || {
    printf 'missing required command: %s\n' "$command_name" >&2
    exit 1
  }
done

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  printf 'missing required SHA-256 tool: sha256sum or shasum\n' >&2
  exit 1
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-install.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

download() {
  output=$1
  url=$2
  curl -fL --proto '=https' --tlsv1.2 --retry 3 --retry-all-errors -o "$output" "$url"
}

download "$tmp_dir/$asset" "$archive_url"
download "$tmp_dir/checksums.txt" "$checksums_url"

expected=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$tmp_dir/checksums.txt")
[ -n "$expected" ] || { printf 'checksum entry not found for %s\n' "$asset" >&2; exit 1; }
actual=$(sha256 "$tmp_dir/$asset")
[ "$actual" = "$expected" ] || {
  printf 'checksum mismatch for %s: got %s, want %s\n' "$asset" "$actual" "$expected" >&2
  exit 1
}

mkdir -p "$tmp_dir/extracted"
tar -xzf "$tmp_dir/$asset" -C "$tmp_dir/extracted"
[ -f "$tmp_dir/extracted/lazy-observe" ] || { printf 'archive is missing lazy-observe\n' >&2; exit 1; }

if [ -d "$prefix/bin" ] && [ -w "$prefix/bin" ]; then
  install -m 0755 "$tmp_dir/extracted/lazy-observe" "$destination"
elif [ ! -e "$prefix/bin" ] && [ -w "$prefix" ]; then
  mkdir -p "$prefix/bin"
  install -m 0755 "$tmp_dir/extracted/lazy-observe" "$destination"
elif command -v sudo >/dev/null 2>&1; then
  sudo install -d "$prefix/bin"
  sudo install -m 0755 "$tmp_dir/extracted/lazy-observe" "$destination"
else
  printf 'cannot write %s; set PREFIX to a writable directory\n' "$prefix/bin" >&2
  exit 1
fi

printf 'installed lazy-observe to %s\nrun: lazy-observe server\n' "$destination"
