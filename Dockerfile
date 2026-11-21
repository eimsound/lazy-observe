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
RUN set -eu; \
    . ./mise-release.env; \
    build_arch=${BUILDARCH:-$(uname -m)}; \
    case "$build_arch" in \
    amd64 | x86_64) \
    mise_asset="mise-${MISE_BOOTSTRAP_VERSION}-linux-x64"; \
    mise_sha256=$MISE_BOOTSTRAP_SHA256_AMD64 \
    ;; \
    arm64 | aarch64) \
    mise_asset="mise-${MISE_BOOTSTRAP_VERSION}-linux-arm64"; \
    mise_sha256=$MISE_BOOTSTRAP_SHA256_ARM64 \
    ;; \
    *) \
    printf 'unsupported build architecture for the pinned mise: %s\n' "$build_arch" >&2; \
    exit 1 \
    ;; \
    esac; \
    curl --proto '=https' --tlsv1.2 -fsSL \
    --retry 3 --retry-delay 2 --retry-all-errors --connect-timeout 20 --max-time 120 \
    -o /tmp/mise \
    "https://github.com/jdx/mise/releases/download/${MISE_BOOTSTRAP_VERSION}/${mise_asset}"; \
    printf '%s  %s\n' "$mise_sha256" /tmp/mise | sha256sum --check --strict -; \
    install -m 0755 /tmp/mise /usr/local/bin/mise; \
    rm -f /tmp/mise
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
