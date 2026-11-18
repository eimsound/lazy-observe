#!/usr/bin/env bash
set -euo pipefail

# SDK release driver for the monorepo, called by .github/workflows/sdk-release.yml.
#
# usage: sdk-publish.sh <tag> [--publish]
#
# Each SDK is released by its own tag prefix:
#   sdk/go/vX.Y.Z        Go module github.com/eimsound/lazy-observe/sdk/go. Go
#                        modules are published by the tag itself; --publish only
#                        asks proxy.golang.org to fetch the version.
#   sdk-node-vX.Y.Z      npm @lazy-observe/node (sdk/node/packages/node)
#   sdk-browser-vX.Y.Z   npm @lazy-observe/browser, @lazy-observe/react,
#                        @lazy-observe/bundler-plugin (sdk/browser/packages/*)
#   sdk-python-vX.Y.Z    PyPI lazyobserve (sdk/python); the workflow uploads the
#                        built dist/ with PyPI trusted publishing.
#
# Without --publish (the default) the script only verifies that the tag matches
# the package versions and builds the artifacts: nothing leaves the runner.
# npm publishing reads NODE_AUTH_TOKEN (set by actions/setup-node from NPM_TOKEN).

usage() {
  printf 'usage: %s <tag> [--publish]\n' "${0##*/}" >&2
}

fail() {
  printf 'sdk-publish: %s\n' "$*" >&2
  exit 1
}

[[ $# == 1 || $# == 2 ]] || { usage; exit 2; }
tag=$1
publish=0
if [[ $# == 2 ]]; then
  [[ $2 == --publish ]] || { usage; exit 2; }
  publish=1
fi

root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
cd "$root"

semver='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?'
case $tag in
  sdk/go/v*) sdk=go version=${tag#sdk/go/v} ;;
  sdk-node-v*) sdk=node version=${tag#sdk-node-v} ;;
  sdk-browser-v*) sdk=browser version=${tag#sdk-browser-v} ;;
  sdk-python-v*) sdk=python version=${tag#sdk-python-v} ;;
  *) fail "unsupported SDK tag: $tag (want sdk/go/v*, sdk-node-v*, sdk-browser-v*, or sdk-python-v*)" ;;
esac
[[ $version =~ ^${semver}$ ]] || fail "tag version is not SemVer: $version"

json_version() { # package.json
  node -p "require(process.argv[1]).version" "$root/$1"
}

expect_version() { # file actual
  [[ $2 == "$version" ]] || fail "$1 has version $2, tag $tag wants $version"
}

printf '== SDK release ==\ntag: %s\nsdk: %s\nversion: %s\npublish: %s\n' \
  "$tag" "$sdk" "$version" "$([[ $publish == 1 ]] && echo yes || echo 'no (dry run)')"

case $sdk in
  go)
    # 先收齐再取第一条：pipefail 下 sed | head 会在 head 提前退出时让 sed 收到 SIGPIPE（141）。
    module_lines=$(sed -n 's/^module[[:space:]]*//p' sdk/go/go.mod)
    module_path=${module_lines%%$'\n'*}
    [[ $module_path == github.com/eimsound/lazy-observe/sdk/go ]] || \
      fail "unexpected Go SDK module path: $module_path"
    (cd sdk/go && CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test ./...)
    if [[ $publish == 1 ]]; then
      (cd "$(mktemp -d)" && GOPROXY=https://proxy.golang.org go list -m "$module_path@v$version")
      printf 'DONE: %s@v%s is available from proxy.golang.org\n' "$module_path" "$version"
    else
      printf 'DRY RUN: Go module %s@v%s is delivered by the tag\n' "$module_path" "$version"
    fi
    ;;
  node | browser)
    workspace=sdk/$sdk
    packages=()
    for manifest in "$workspace"/packages/*/package.json; do
      expect_version "$manifest" "$(json_version "$manifest")"
      packages+=("$(dirname -- "$manifest")")
    done
    ((${#packages[@]} > 0)) || fail "no packages under $workspace/packages"
    (cd "$workspace" && pnpm install --frozen-lockfile && pnpm build && pnpm typecheck && pnpm test)
    for package in "${packages[@]}"; do
      [[ -f $package/LICENSE ]] || fail "$package/LICENSE is missing from the published files"
      if [[ $publish == 1 ]]; then
        (cd "$package" && pnpm publish --access public --no-git-checks)
      else
        (cd "$package" && pnpm publish --access public --no-git-checks --dry-run)
      fi
    done
    ;;
  python)
    # 同上：pyproject 里可能有多个 version 键，先收齐再取首行。
    py_versions=$(sed -n 's/^version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' sdk/python/pyproject.toml)
    py_version=${py_versions%%$'\n'*}
    expect_version sdk/python/pyproject.toml "$py_version"
    python=${PYTHON:-python3}
    (
      cd sdk/python
      "$python" -m pip install --quiet build
      rm -rf dist
      "$python" -m build
    )
    printf 'built: %s\n' "$(ls sdk/python/dist)"
    if [[ $publish == 1 ]]; then
      printf 'DONE: sdk/python/dist is ready for the PyPI trusted-publishing upload step\n'
    else
      printf 'DRY RUN: sdk/python/dist is not uploaded\n'
    fi
    ;;
esac
