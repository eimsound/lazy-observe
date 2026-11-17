# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# Every base image is pinned to its multi-platform index digest (the tag is kept
# for readers); bump tag and digest together.
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS builder
ARG TARGETOS
ARG TARGETARCH
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

# hadolint ignore=DL3008
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl git gzip tar && \
    rm -rf /var/lib/apt/lists/* && \
    curl https://mise.run | MISE_VERSION=v2026.5.3 MISE_INSTALL_PATH=/usr/local/bin/mise sh
COPY mise.toml ./
# github_token is provided as a BuildKit secret (never an ARG/ENV, so it never
# lands in the image config). Authenticates aqua against the GitHub API to stay
# clear of the anonymous rate limit; tolerated as absent via `|| true`.
RUN --mount=type=secret,id=github_token bash -c ' \
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
COPY --from=builder --chown=65532:65532 /out/lazy-observe /usr/local/bin/lazy-observe
COPY --from=builder --chown=65532:65532 /out/data /data
COPY LICENSE /usr/share/doc/lazy-observe/LICENSE
COPY third_party/ /usr/share/doc/lazy-observe/third_party/
COPY web/THIRD_PARTY_LICENSES.md /usr/share/doc/lazy-observe/third_party/web/THIRD_PARTY_LICENSES.md
EXPOSE 3100
VOLUME ["/data"]
ENTRYPOINT ["/usr/local/bin/lazy-observe", "server", "--data-dir", "/data"]
