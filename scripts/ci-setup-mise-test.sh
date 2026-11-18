#!/usr/bin/env bash
set -euo pipefail

# Self-test for scripts/ci-setup-mise.sh. Runs the real script under `env -i` with
# a fake curl and a temporary HOME, so no tool is installed on the host and no large
# release asset is downloaded.
#
# Asserts:
#   - the download URL carries MISE_BOOTSTRAP_VERSION and the arch-specific asset;
#   - a matching sha256 installs an executable mise and updates GITHUB_PATH;
#   - a mismatched sha256 exits non-zero before trust/install run.

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
script=$root/scripts/ci-setup-mise.sh
release_env=$root/scripts/mise-release.env
bash_bin=$(command -v bash)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=scripts/mise-release.env
. "$release_env"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

# The fake mise binary is also the downloaded release asset, so its sha256 can be
# pinned in the fixture without fetching the real 75-81 MB binary.
payload=$work/fake-mise
cat >"$payload" <<'PAYLOAD'
#!/bin/sh
printf 'mise %s\n' "$*" >>"${MISE_CALL_LOG:?}"
PAYLOAD
chmod 0755 "$payload"
payload_sha=$(sha256sum "$payload" | awk '{print $1}')
zero_sha=0000000000000000000000000000000000000000000000000000000000000000

run_case() { # name arch sha_amd64 sha_arm64 expected_rc
  local name=$1 arch=$2 sha_amd64=$3 sha_arm64=$4 expected_rc=$5
  local dir=$work/case-$name asset_arch rc=0
  case $arch in
  x86_64) asset_arch=x64 ;;
  aarch64) asset_arch=arm64 ;;
  *)
    fail "$name: unsupported fixture arch $arch"
    return
    ;;
  esac
  mkdir -p "$dir/fakebin" "$dir/home"
  cp "$script" "$dir/ci-setup-mise.sh"
  printf 'MISE_BOOTSTRAP_VERSION=%s\nMISE_BOOTSTRAP_SHA256_AMD64=%s\nMISE_BOOTSTRAP_SHA256_ARM64=%s\n' \
    "$MISE_BOOTSTRAP_VERSION" "$sha_amd64" "$sha_arm64" >"$dir/mise-release.env"
  cat >"$dir/fakebin/uname" <<EOF
#!/bin/sh
printf '%s\\n' "$arch"
EOF
  cat >"$dir/fakebin/curl" <<'CURL'
#!/bin/sh
printf '%s\n' "$*" >>"$CURL_LOG"
out=
while [ $# -gt 0 ]; do
  case $1 in
  -o) out=$2; shift 2 ;;
  *) shift ;;
  esac
done
cp "$FAKE_MISE_PAYLOAD" "$out"
CURL
  chmod 0755 "$dir/fakebin/uname" "$dir/fakebin/curl"
  : >"$dir/curl.log"
  : >"$dir/mise.log"
  : >"$dir/github_path"

  env -i \
    HOME="$dir/home" \
    PATH="$dir/fakebin:/usr/bin:/bin" \
    GITHUB_PATH="$dir/github_path" \
    MISE_CI_TOOLS=go \
    CURL_LOG="$dir/curl.log" \
    MISE_CALL_LOG="$dir/mise.log" \
    FAKE_MISE_PAYLOAD="$payload" \
    "$bash_bin" "$dir/ci-setup-mise.sh" >"$dir/stdout" 2>"$dir/stderr" || rc=$?

  if [[ $rc -ne $expected_rc ]]; then
    fail "$name: exit $rc, want $expected_rc; stderr: $(cat "$dir/stderr")"
    return
  fi
  if [[ $expected_rc -ne 0 ]]; then
    if [[ -s $dir/mise.log ]]; then
      fail "$name: trust/install ran despite the failing checksum"
    fi
    if [[ -e $dir/home/.local/bin/mise ]]; then
      fail "$name: mise was installed despite the failing checksum"
    fi
    return
  fi

  [[ -x $dir/home/.local/bin/mise ]] || fail "$name: mise was not installed as an executable"
  grep -Fq "https://github.com/jdx/mise/releases/download/$MISE_BOOTSTRAP_VERSION/mise-$MISE_BOOTSTRAP_VERSION-linux-$asset_arch" \
    "$dir/curl.log" || fail "$name: download URL did not use the pinned version/asset: $(cat "$dir/curl.log")"
  grep -Fq 'trust' "$dir/mise.log" || fail "$name: trust was not invoked"
  grep -Fq 'install go' "$dir/mise.log" || fail "$name: install was not invoked"
  grep -Fq "$dir/home/.local/bin" "$dir/github_path" || fail "$name: GITHUB_PATH was not updated"
}

run_case amd64 x86_64 "$payload_sha" "$payload_sha" 0
run_case arm64 aarch64 "$payload_sha" "$payload_sha" 0
run_case wrong-sha x86_64 "$zero_sha" "$payload_sha" 1

if ((failures > 0)); then
  printf 'ci-setup-mise test: %d failure(s)\n' "$failures" >&2
  exit 1
fi
printf 'PASS: ci-setup-mise downloads the pinned mise and enforces its sha256\n'
