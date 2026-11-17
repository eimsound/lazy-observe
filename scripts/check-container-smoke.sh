#!/usr/bin/env bash
set -euo pipefail

image=${1:?usage: check-container-smoke.sh IMAGE PLATFORM}
platform=${2:?usage: check-container-smoke.sh IMAGE PLATFORM}
case $platform in
  linux/amd64) evidence_marker='PASS artifact: linux/amd64 zero-config' ;;
  linux/arm64) evidence_marker='PASS artifact: linux/arm64 zero-config' ;;
  *) printf 'unsupported container smoke platform: %s\n' "$platform" >&2; exit 1 ;;
esac

# 冒烟容器把 3100 发布到主机 127.0.0.1 的随机端口，脚本在主机上直接访问。
# PID（$$）可能复用；加随机因子避免撞上残留容器名。
container="lazy-observe-container-smoke-${platform##*/}-$$-$RANDOM"
cid=

cleanup() {
  if [[ -n $cid ]]; then
    docker rm -f "$cid" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT HUP INT TERM

cid=$(docker run -d --name "$container" --publish 127.0.0.1::3100 --platform "$platform" \
  "$image" --listen 0.0.0.0:3100)

host_port=$(docker port "$cid" 3100/tcp | sed -n 's/^127\.0\.0\.1://p' | head -n1)
if [[ -z $host_port ]]; then
  printf 'could not resolve the published port for %s\n' "$container" >&2
  exit 1
fi
# 本机地址必须直连，避免环境里的 HTTP_PROXY 截走请求。
address="http://127.0.0.1:$host_port"

for _ in $(seq 1 120); do
  if curl -fsS --noproxy '*' "$address/readyz" >/dev/null; then
    curl -fsS --noproxy '*' "$address/" >/dev/null
    printf '%s (readiness and embedded Web served on %s)\n' "$evidence_marker" "$address"
    exit 0
  fi
  if [[ $(docker inspect -f '{{.State.Running}}' "$cid" 2>/dev/null || true) != true ]]; then
    docker logs "$cid" >&2 || true
    printf 'container exited before readiness: %s\n' "$platform" >&2
    exit 1
  fi
  sleep 1
done

docker logs "$cid" >&2 || true
printf 'container readiness timed out: %s\n' "$platform" >&2
exit 1
