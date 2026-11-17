#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'Docker CI setup failed: %s\n' "$*" >&2
  exit 1
}

if ! command -v docker >/dev/null 2>&1; then
  (( EUID == 0 )) || fail 'docker is unavailable and docker-cli installation requires root'
  command -v apt-get >/dev/null 2>&1 || fail 'docker is unavailable and apt-get is not installed'
  command -v dpkg >/dev/null 2>&1 || fail 'docker is unavailable and dpkg is not installed'

  if [[ ! -r /etc/os-release ]]; then
    fail 'docker is unavailable and /etc/os-release cannot be read'
  fi
  # shellcheck disable=SC1091
  . /etc/os-release
  [[ ${ID:-} == debian ]] || fail "docker-cli installation requires Debian (got ${ID:-unknown})"

  cleanup_apt_lists() {
    rm -rf -- /var/lib/apt/lists/*
  }
  trap cleanup_apt_lists EXIT

  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install --yes --no-install-recommends docker-cli

  cleanup_apt_lists
  trap - EXIT
  command -v docker >/dev/null 2>&1 || fail 'docker-cli was installed but docker is not on PATH'
else
  printf 'Docker CLI is already available; skipping installation\n'
fi

docker_bin=$(command -v docker) || fail 'docker CLI is unavailable after setup'
docker_host=${DOCKER_HOST:-unix:///var/run/docker.sock}
case "$docker_host" in
  unix://*) docker_socket=${docker_host#unix://} ;;
  *) fail "DOCKER_HOST must use a Unix socket (got $docker_host)" ;;
esac
[[ -n $docker_socket ]] || fail 'DOCKER_HOST contains an empty Unix socket path'
[[ -S $docker_socket ]] || fail "Docker socket is missing or is not a Unix socket: $docker_socket"

client_version=$(
  "$docker_bin" --host "$docker_host" version --format '{{.Client.Version}}'
) || fail "Docker client could not negotiate with the daemon through $docker_socket"
server_version=$(
  "$docker_bin" --host "$docker_host" version --format '{{.Server.Version}}'
) || fail "Docker server version is unavailable through $docker_socket"
daemon_version=$(
  "$docker_bin" --host "$docker_host" info --format '{{.ServerVersion}}'
) || fail "Docker daemon did not answer docker info through $docker_socket"

[[ -n $client_version ]] || fail 'Docker client reported an empty version'
[[ -n $server_version ]] || fail 'Docker server reported an empty version'
[[ -n $daemon_version ]] || fail 'Docker daemon reported an empty server version'
[[ $server_version == "$daemon_version" ]] || fail \
  "Docker server version mismatch: docker version=$server_version, docker info=$daemon_version"

printf 'Docker client %s connected to server %s through %s\n' \
  "$client_version" "$server_version" "$docker_socket"
