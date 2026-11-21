# syntax=docker/dockerfile:1.29@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
# Every base image is pinned to its multi-platform index digest (the tag is kept
# for readers); bump tag and digest together.
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS builder
ARG TARGETOS
ARG TARGETARCH
# builder 阶段固定在 $BUILDPLATFORM 上执行；BUILDARCH 决定要安装哪个架构的 mise。
ARG BUILDARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
# Optional build args so `mise install` inside the build container can reach
# GitHub release downloads and the GitHub API through a local proxy. Empty by
# default (direct access); the release workflow does not set them.
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG NO_PROXY

ENV PATH=/root/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin
ENV HTTP_PROXY=$HTTP_PROXY HTTPS_PROXY=$HTTPS_PROXY NO_PROXY=$NO_PROXY
WORKDIR /src

# libatomic1: the official Node 26 binaries mise installs link libatomic.so.1,
# which trixie-slim does not ship.
# hadolint ignore=DL3008
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl git gzip libatomic1 tar && \
    rm -rf /var/lib/apt/lists/*
# mise 的版本与两个架构的 sha256 只在 scripts/mise-release.env 维护，CI 的 ci-setup-mise.sh 读同一份文件。
COPY mise.toml scripts/mise-release.env ./
# builder 阶段跑在 $BUILDPLATFORM 上，所以按 BUILDARCH（不是产物架构 TARGETARCH）选 mise，
# 多架构构建时两者可能不同。sha256 来自上游同一 release 的 SHASUMS256.txt。
RUN --mount=type=secret,id=github_token <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

. ./mise-release.env
build_arch=${BUILDARCH:-$(uname -m)}
case "$build_arch" in
amd64 | x86_64)
  mise_asset="mise-${MISE_BOOTSTRAP_VERSION}-linux-x64"
  mise_sha256=$MISE_BOOTSTRAP_SHA256_AMD64
  ;;
arm64 | aarch64)
  mise_asset="mise-${MISE_BOOTSTRAP_VERSION}-linux-arm64"
  mise_sha256=$MISE_BOOTSTRAP_SHA256_ARM64
  ;;
*)
  printf 'unsupported build architecture for the pinned mise: %s\n' "$build_arch" >&2
  exit 1
  ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

curl_common_args=(
  --proto '=https'
  --tlsv1.2
  --proto-redir '=https'
  -fsS
  --connect-timeout 20
  --max-time 120
  --retry 5
  --retry-all-errors
  --retry-max-time 90
)

download_via_github_api() {
  local token=$1 dest=$2
  local api_headers=$work/github-api.headers
  local asset_headers=$work/github-asset.headers
  local release_json=$work/release.json
  local asset_api_url redirect_meta http_code rest cdn_url

  (
    umask 077
    printf 'Authorization: Bearer %s\nAccept: application/vnd.github+json\nX-GitHub-Api-Version: 2022-11-28\n' \
      "$token" >"$api_headers"
    printf 'Authorization: Bearer %s\nAccept: application/octet-stream\nX-GitHub-Api-Version: 2022-11-28\n' \
      "$token" >"$asset_headers"
  )

  if ! curl "${curl_common_args[@]}" --max-redirs 0 \
    -H @"$api_headers" \
    -o "$release_json" \
    "https://api.github.com/repos/jdx/mise/releases/tags/${MISE_BOOTSTRAP_VERSION}"; then
    rm -f "$api_headers" "$asset_headers"
    return 1
  fi

  asset_api_url=$(awk -v target="$mise_asset" '
    { buf = buf $0 "\n" }
    END {
      n = length(buf)
      depth = 0
      assets_arr_depth = 0
      cur_name = ""
      cur_url = ""
      found = ""
      for (i = 1; i <= n; i++) {
        c = substr(buf, i, 1)
        if (c == "{") {
          depth++
          kind[depth] = "{"
          state[depth] = "key"
          last_key[depth] = ""
          if (assets_arr_depth > 0 && depth == assets_arr_depth + 1) {
            cur_name = ""
            cur_url = ""
          }
        } else if (c == "}") {
          if (assets_arr_depth > 0 && depth == assets_arr_depth + 1) {
            if (cur_name == target && cur_url ~ /^https:\/\/api\.github\.com\/repos\/jdx\/mise\/releases\/assets\/[0-9]+$/) {
              found = cur_url
              break
            }
          }
          depth--
        } else if (c == "[") {
          depth++
          kind[depth] = "["
          state[depth] = ""
          last_key[depth] = ""
          if (assets_arr_depth == 0 && depth == 2 && kind[1] == "{" && last_key[1] == "assets" && state[1] == "val") {
            assets_arr_depth = depth
          }
        } else if (c == "]") {
          if (depth == assets_arr_depth) {
            assets_arr_depth = 0
          }
          depth--
        } else if (c == ":") {
          if (depth > 0 && kind[depth] == "{" && state[depth] == "colon") {
            state[depth] = "val"
          }
        } else if (c == ",") {
          if (depth > 0 && kind[depth] == "{") {
            state[depth] = "key"
            last_key[depth] = ""
          }
        } else if (c == "\"") {
          str = ""
          i++
          while (i <= n) {
            ch = substr(buf, i, 1)
            if (ch == "\"") {
              break
            }
            if (ch == "\\") {
              i++
              if (i <= n) {
                esc = substr(buf, i, 1)
                if (esc == "\"" || esc == "\\" || esc == "/") {
                  str = str esc
                } else {
                  str = str "\\" esc
                }
              }
            } else {
              str = str ch
            }
            i++
          }
          if (depth > 0 && kind[depth] == "{") {
            if (state[depth] == "key") {
              last_key[depth] = str
              state[depth] = "colon"
            } else if (state[depth] == "val") {
              if (assets_arr_depth > 0 && depth == assets_arr_depth + 1) {
                if (last_key[depth] == "name") {
                  cur_name = str
                } else if (last_key[depth] == "url") {
                  cur_url = str
                }
              }
              state[depth] = "after_val"
            }
          }
        }
      }
      if (found != "") {
        print found
      }
    }
  ' "$release_json")

  if [[ ! $asset_api_url =~ ^https://api\.github\.com/repos/jdx/mise/releases/assets/[0-9]+$ ]]; then
    rm -f "$api_headers" "$asset_headers"
    return 1
  fi

  if ! redirect_meta=$(curl "${curl_common_args[@]}" --max-redirs 0 \
    -H @"$asset_headers" \
    -o /dev/null \
    -w '%{http_code}\n%{redirect_url}\n' \
    "$asset_api_url"); then
    rm -f "$api_headers" "$asset_headers"
    return 1
  fi
  rm -f "$api_headers" "$asset_headers"

  if [[ $redirect_meta != *$'\n'* ]]; then
    return 1
  fi
  http_code=${redirect_meta%%$'\n'*}
  rest=${redirect_meta#*$'\n'}
  cdn_url=${rest%%$'\n'*}
  if [[ $http_code != 302 || ! $cdn_url =~ ^https://[^[:space:]]+$ ]]; then
    return 1
  fi

  curl "${curl_common_args[@]}" -L -o "$dest" "$cdn_url"
}

token=$(tr -d '\r\n' </run/secrets/github_token 2>/dev/null || true)
downloaded=0
if [[ -n $token ]]; then
  if download_via_github_api "$token" "$work/$mise_asset"; then
    downloaded=1
  else
    rm -f "$work/$mise_asset" "$work/github-api.headers" "$work/github-asset.headers"
    printf 'warning: authenticated mise download via GitHub API failed; falling back to direct release download\n' >&2
  fi
fi

if ((!downloaded)); then
  curl "${curl_common_args[@]}" -L \
    -o "$work/$mise_asset" \
    "https://github.com/jdx/mise/releases/download/${MISE_BOOTSTRAP_VERSION}/${mise_asset}"
fi

printf '%s  %s\n' "$mise_sha256" "$work/$mise_asset" | sha256sum --check --strict -
install -m 0755 "$work/$mise_asset" /usr/local/bin/mise
EOF
# github_token is provided as a BuildKit secret (never an ARG/ENV, so it never
# lands in the image config). Authenticates aqua against the GitHub API to stay
# clear of the anonymous rate limit; tolerated as absent via `|| true`.
RUN --mount=type=secret,id=github_token bash -euo pipefail -c ' \
  GITHUB_TOKEN="$(cat /run/secrets/github_token 2>/dev/null || true)"; \
  export GITHUB_TOKEN; \
  mise trust /src/mise.toml && mise install go node pnpm \
'
COPY . .
# go.mod replaces github.com/eimsound/lazy-observe/sdk/go with the in-repo ./sdk/go
# module, which `COPY . .` already brought into the build context.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    VERSION="$VERSION" COMMIT="$COMMIT" DATE="$DATE" \
    BINARY=/out/lazy-observe MISE_TASK_RUN_AUTO_INSTALL=false \
    mise run --skip-tools dist && \
    mkdir -p /out/data

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=builder --chmod=0755 /out/lazy-observe /usr/local/bin/lazy-observe
COPY --from=builder --chown=65532:65532 /out/data /data
COPY LICENSE /usr/share/doc/lazy-observe/LICENSE
COPY third_party/ /usr/share/doc/lazy-observe/third_party/
COPY web/THIRD_PARTY_LICENSES.md /usr/share/doc/lazy-observe/third_party/web/THIRD_PARTY_LICENSES.md
EXPOSE 3100
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/lazy-observe", "server", "--data-dir", "/data"]
