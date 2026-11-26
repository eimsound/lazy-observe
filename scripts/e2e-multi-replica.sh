#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
BINARY=${LO_E2E_BINARY:-"$ROOT/bin/lazy-observe"}
BIN_DIR=${LO_E2E_STORAGE_BIN_DIR:-"$ROOT/.cache/storage-bin"}
# Every default image is name:tag@sha256:<multi-platform index digest>; the tag
# is for readers, the digest is what Docker pulls. Bump tag and digest together.
# Overrides (LO_E2E_*_IMAGE) should be pinned the same way.
#
# VictoriaMetrics ships no Debian variant; the -scratch builds carry only the
# static binary and CA bundle (same entrypoint and ports as the Alpine default).
# They are pulled from quay.io, VictoriaMetrics' second official registry, whose
# -cluster digests are identical to Docker Hub's.
VMSTORAGE_IMAGE=${LO_E2E_VMSTORAGE_IMAGE:-quay.io/victoriametrics/vmstorage:v1.157.0-cluster-scratch@sha256:6e43af80162367bd54526e988fe7c1d03891cef1f93aec36ba19e22f4ac6c422}
VMINSERT_IMAGE=${LO_E2E_VMINSERT_IMAGE:-quay.io/victoriametrics/vminsert:v1.157.0-cluster-scratch@sha256:72c0b0041ca31f67c99083f127d2cba2e1cb9f26a942981c3d60e637230dea00}
VMSELECT_IMAGE=${LO_E2E_VMSELECT_IMAGE:-quay.io/victoriametrics/vmselect:v1.157.0-cluster-scratch@sha256:c2bab3ad0ac24b0d8ddfa19b34e140143e792cee3fd226d36fed9856f7ca640d}
# SeaweedFS is Apache-2.0 and provides a real S3-compatible API without adding
# a copyleft test dependency. Upstream only publishes Alpine-based images, so
# this one stays Alpine; the digest pin keeps the gate repeatable.
SEAWEED_IMAGE=${LO_E2E_SEAWEED_IMAGE:-chrislusf/seaweedfs:4.54@sha256:4b42c212543a0acd4c0e675215ecdcaf62a186ef98def76c7e12701871222d15}
# Valkey is Redis-protocol compatible and BSD-3-Clause. Keep both the version
# and multi-platform manifest digest fixed so this gate cannot drift to a
# differently licensed Redis release through a floating image tag.
VALKEY_IMAGE=${LO_E2E_VALKEY_IMAGE:-valkey/valkey:8.1.12-trixie@sha256:5032c4dd7abe08d6fd9382054608183e2bf1f7e3895ef619b8784698454325f7}
POSTGRES_IMAGE=${LO_E2E_POSTGRES_IMAGE:-postgres:16.16-trixie@sha256:dd2bf3ce2dc83a81674cf7f217fcfd8a5009a4b325dda5b843bfee82a06cff6f}

# 网络：脚本自建一个 Docker bridge 网络，依赖容器之间用容器名互访；lazy-observe replica
# 是主机上的本地进程，经容器在该网络上的 IP 访问它们（Linux 主机可直接路由到 bridge
# 网络），不占用宿主端口。GitHub 托管 runner 与本机 Linux 都按这种方式运行。

skip() {
  echo "SKIP: $*" >&2
  # H2/H3 is a critical acceptance test. Exit 77 keeps a missing Docker
  # prerequisite visibly distinct from PASS and makes mise fail the gate.
  exit 77
}

command -v docker >/dev/null 2>&1 || skip "Docker is unavailable; cluster HA e2e was not run"
docker info >/dev/null 2>&1 || skip "Docker daemon is unavailable; cluster HA e2e was not run"
for command in curl gzip go; do
  command -v "$command" >/dev/null 2>&1 || skip "$command is required; cluster HA e2e was not run"
done
# 内联的 JSON/随机数/HTTP 辅助只用 Python 标准库。解释器由 uv 受管（scripts/lib/uv-python.sh），不用系统 python3；
# uv 缺失是失败而不是 SKIP（由 mise.toml [tools] 的 uv 提供，e2e-ha / perf-ha 经 mise 运行）。
# shellcheck source=lib/uv-python.sh
. "$ROOT/scripts/lib/uv-python.sh"
PY=$(lo_uv_python) || exit 1
GATEWAY_GOMAXPROCS=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["ha_scaling"]["gomaxprocs_per_replica"])' "$ROOT/bench/performance-gates.json")
case "$GATEWAY_GOMAXPROCS" in '' | *[!0-9]* | 0)
  echo "FAIL: invalid per-replica GOMAXPROCS performance budget" >&2
  exit 1
  ;;
esac
TRACE_SEARCH_MAX=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["query"]["medium_fixture"]["traces"])' "$ROOT/bench/performance-gates.json")
case "$TRACE_SEARCH_MAX" in '' | *[!0-9]*)
  echo "FAIL: invalid medium trace fixture size" >&2
  exit 1
  ;;
esac
if [ "$TRACE_SEARCH_MAX" -lt 1000 ] || [ "$TRACE_SEARCH_MAX" -gt 10000 ]; then
  echo "FAIL: medium trace fixture must stay between 1000 and the controlled 10000-trace search maximum" >&2
  exit 1
fi
for executable in "$BINARY" "$BIN_DIR/victoria-logs-prod" "$BIN_DIR/victoria-traces-prod" "$BIN_DIR/vmagent-prod"; do
  if [ ! -x "$executable" ]; then
    echo "FAIL: missing executable: $executable" >&2
    exit 1
  fi
done

WORK=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-ha.XXXXXX")
RUN_ID=$$
LOCK_DIR="${TMPDIR:-/tmp}/lazy-observe-e2e-ha.lock"
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  holder=$(cat "$LOCK_DIR/pid" 2>/dev/null || true)
  echo "FAIL: cluster HA e2e lock is held or stale (owner=${holder:-unknown}); remove $LOCK_DIR only after confirming no gate is running" >&2
  rm -rf "$WORK"
  exit 1
fi
printf '%s\n' "$$" >"$LOCK_DIR/pid"
PREFIX="lazy-observe-e2e-$RUN_ID"
# 本次运行专用的 bridge 网络，结束时拆除。
NETWORK="$PREFIX"
PG_NAME="$PREFIX-pg"
REDIS_NAME="$PREFIX-redis"
S3_NAME="$PREFIX-seaweedfs"
VMSTORAGE_1="$PREFIX-vmstorage-1"
VMSTORAGE_2="$PREFIX-vmstorage-2"
VMSTORAGE_3="$PREFIX-vmstorage-3"
VMINSERT_1="$PREFIX-vminsert-1"
VMINSERT_2="$PREFIX-vminsert-2"
VMSELECT_NAME="$PREFIX-vmselect"
S3_BUCKET=lazy-observe-e2e
S3_ACCESS_KEY=lazyobservee2e
S3_SECRET_KEY=lazyobservee2e-secret
DOCKER_NAMES="$PG_NAME $REDIS_NAME $S3_NAME $VMSTORAGE_1 $VMSTORAGE_2 $VMSTORAGE_3 $VMINSERT_1 $VMINSERT_2 $VMSELECT_NAME"
PIDS=""

cleanup() {
  exit_code=$?
  set +e
  for pid in $PIDS; do kill -TERM "$pid" 2>/dev/null; done
  sleep 1
  for pid in $PIDS; do
    kill -KILL "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
  done
  docker rm -f $DOCKER_NAMES >/dev/null 2>&1
  docker network rm "$NETWORK" >/dev/null 2>&1
  if [ "$exit_code" -ne 0 ]; then
    for log_name in perf-sink.log perf-a.log perf-b.log; do
      log_file="$WORK/$log_name"
      if [ -s "$log_file" ]; then
        printf '=== %s (tail -n 200) ===\n' "$log_name" >&2
        tail -n 200 "$log_file" >&2 || true
      fi
    done
  fi
  rm -rf "$WORK"
  if [ "$(cat "$LOCK_DIR/pid" 2>/dev/null || true)" = "$$" ]; then
    rm -rf "$LOCK_DIR"
  fi
}

forget_pid() {
  forgotten=$1
  remaining=""
  for candidate in $PIDS; do
    [ "$candidate" = "$forgotten" ] || remaining="$remaining $candidate"
  done
  PIDS=$remaining
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

wait_http() {
  url=$1
  limit=${2:-90}
  count=0
  until curl -fsS "$url" >/dev/null 2>&1; do
    count=$((count + 1))
    if [ "$count" -ge "$limit" ]; then
      echo "FAIL: timed out waiting for $url" >&2
      return 1
    fi
    sleep 1
  done
}

wait_http_listener() {
  url=$1
  limit=${2:-90}
  count=0
  while :; do
    status=$(curl -sS -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)
    if [ -n "$status" ] && [ "$status" != 000 ]; then
      return 0
    fi
    count=$((count + 1))
    if [ "$count" -ge "$limit" ]; then
      echo "FAIL: timed out waiting for an HTTP response from $url" >&2
      return 1
    fi
    sleep 1
  done
}

wait_ready() {
  base=$1
  count=0
  until [ "$(curl -sS -o /dev/null -w '%{http_code}' "$base/readyz" 2>/dev/null || true)" = 200 ]; do
    count=$((count + 1))
    if [ "$count" -ge 90 ]; then
      echo "FAIL: timed out waiting for $base/readyz" >&2
      return 1
    fi
    sleep 1
  done
}

wait_postgres() {
  count=0
  until docker exec "$PG_NAME" pg_isready -U postgres -d lazy_observe >/dev/null 2>&1; do
    count=$((count + 1))
    if [ "$count" -ge 90 ]; then
      echo "FAIL: PostgreSQL container did not become ready" >&2
      docker logs "$PG_NAME" >&2 || true
      return 1
    fi
    sleep 1
  done
}

wait_redis() {
  count=0
  until docker exec "$REDIS_NAME" valkey-cli ping 2>/dev/null | grep -q PONG; do
    count=$((count + 1))
    if [ "$count" -ge 90 ]; then
      echo "FAIL: Redis container did not become ready" >&2
      docker logs "$REDIS_NAME" >&2 || true
      return 1
    fi
    sleep 1
  done
}

post_payload() {
  url=$1 token=$2 content_type=$3 payload=$4 label=$5
  count=0
  while [ "$count" -lt 30 ]; do
    status=$(curl -sS -o "$WORK/post-response" -w '%{http_code}' \
      -H "Content-Type: $content_type" -H "Authorization: Bearer $token" \
      --data-binary @"$payload" "$url" || true)
    case "$status" in
    2??) return 0 ;;
    esac
    count=$((count + 1))
    sleep 1
  done
  echo "FAIL: $label returned HTTP ${status:-transport-error}: $(cat "$WORK/post-response" 2>/dev/null || true)" >&2
  return 1
}

docker network create "$NETWORK" >/dev/null
docker run -d --name "$PG_NAME" --network "$NETWORK" -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=lazy_observe "$POSTGRES_IMAGE" >/dev/null
docker run -d --name "$REDIS_NAME" --network "$NETWORK" "$VALKEY_IMAGE" >/dev/null
docker run -d --name "$S3_NAME" --network "$NETWORK" \
  --tmpfs /data:rw,size=512m \
  -e "AWS_ACCESS_KEY_ID=$S3_ACCESS_KEY" -e "AWS_SECRET_ACCESS_KEY=$S3_SECRET_KEY" \
  -e "S3_BUCKET=$S3_BUCKET" "$SEAWEED_IMAGE" \
  mini -dir=/data -master.volumeSizeLimitMB=16 >/dev/null

# VictoriaMetrics cluster: three vmstorage nodes satisfy the documented
# 2*N-1 sizing recommendation for replicationFactor=2. vminsert and vmselect
# use the native cluster ports and account-scoped HTTP paths.
for node in "$VMSTORAGE_1" "$VMSTORAGE_2" "$VMSTORAGE_3"; do
  docker run -d --name "$node" --network "$NETWORK" "$VMSTORAGE_IMAGE" \
    -storageDataPath=/storage -retentionPeriod=1d >/dev/null
done
# vminsert/vmselect 不发布宿主端口，主机上的 replica 进程经容器 IP + 内部端口访问
for name in "$VMINSERT_1" "$VMINSERT_2"; do
  docker run -d --name "$name" --network "$NETWORK" "$VMINSERT_IMAGE" \
    -storageNode="$VMSTORAGE_1:8400,$VMSTORAGE_2:8400,$VMSTORAGE_3:8400" \
    -replicationFactor=2 >/dev/null
done
docker run -d --name "$VMSELECT_NAME" --network "$NETWORK" "$VMSELECT_IMAGE" \
  -storageNode="$VMSTORAGE_1:8401,$VMSTORAGE_2:8401,$VMSTORAGE_3:8401" \
  -replicationFactor=2 -dedup.minScrapeInterval=1ms -search.latencyOffset=0s >/dev/null

# 主机上的进程（replica、curl）经容器在本网络上的 IP 访问依赖；容器之间仍用容器名。
container_ip() {
  ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" "$1")
  [ -n "$ip" ] || { echo "FAIL: no IP for container $1 on $NETWORK" >&2; return 1; }
  printf '%s\n' "$ip"
}
PG_HOST=$(container_ip "$PG_NAME")
REDIS_HOST=$(container_ip "$REDIS_NAME")
S3_HOST=$(container_ip "$S3_NAME")
VMINSERT_1_HOST=$(container_ip "$VMINSERT_1")
VMINSERT_2_HOST=$(container_ip "$VMINSERT_2")
VMSELECT_HOST=$(container_ip "$VMSELECT_NAME")
# 配了出站代理的环境里，访问这些内网 IP 必须直连。
container_hosts="$PG_HOST,$REDIS_HOST,$S3_HOST,$VMINSERT_1_HOST,$VMINSERT_2_HOST,$VMSELECT_HOST"
NO_PROXY="${NO_PROXY:+$NO_PROXY,}$container_hosts"
no_proxy="${no_proxy:+$no_proxy,}$container_hosts"
export NO_PROXY no_proxy

wait_postgres
wait_redis
wait_http_listener http://$S3_HOST:8333/
wait_http http://$VMINSERT_1_HOST:8480/health
wait_http http://$VMINSERT_2_HOST:8480/health
wait_http http://$VMSELECT_HOST:8481/health

# VictoriaLogs and VictoriaTraces use their real split insert/select/storage
# roles. The tiny path router models the vmauth or internal load-balancer URL
# that standalone-ha config exposes to the gateway.
mkdir -p "$WORK/logs-1" "$WORK/logs-2" "$WORK/traces-1" "$WORK/traces-2"
"$BIN_DIR/victoria-logs-prod" -httpListenAddr=127.0.0.1:29491 -storageDataPath="$WORK/logs-1" >"$WORK/vlstorage-1.log" 2>&1 &
PIDS="$PIDS $!"
"$BIN_DIR/victoria-logs-prod" -httpListenAddr=127.0.0.1:29492 -storageDataPath="$WORK/logs-2" >"$WORK/vlstorage-2.log" 2>&1 &
PIDS="$PIDS $!"
"$BIN_DIR/victoria-traces-prod" -httpListenAddr=127.0.0.1:30491 -storageDataPath="$WORK/traces-1" >"$WORK/vtstorage-1.log" 2>&1 &
PIDS="$PIDS $!"
"$BIN_DIR/victoria-traces-prod" -httpListenAddr=127.0.0.1:30492 -storageDataPath="$WORK/traces-2" >"$WORK/vtstorage-2.log" 2>&1 &
PIDS="$PIDS $!"
wait_http http://127.0.0.1:29491/health
wait_http http://127.0.0.1:29492/health
wait_http http://127.0.0.1:30491/health
wait_http http://127.0.0.1:30492/health

"$BIN_DIR/victoria-logs-prod" -httpListenAddr=127.0.0.1:29480 \
  -storageNode=127.0.0.1:29491,127.0.0.1:29492 -select.disable >"$WORK/vlinsert.log" 2>&1 &
PIDS="$PIDS $!"
"$BIN_DIR/victoria-logs-prod" -httpListenAddr=127.0.0.1:29482 \
  -storageNode=127.0.0.1:29491,127.0.0.1:29492 -select.disable >"$WORK/vlinsert-2.log" 2>&1 &
PIDS="$PIDS $!"
"$BIN_DIR/victoria-logs-prod" -httpListenAddr=127.0.0.1:29481 \
  -storageNode=127.0.0.1:29491,127.0.0.1:29492 -insert.disable >"$WORK/vlselect.log" 2>&1 &
PIDS="$PIDS $!"
"$BIN_DIR/victoria-traces-prod" -httpListenAddr=127.0.0.1:30480 \
  -storageNode=127.0.0.1:30491,127.0.0.1:30492 -select.disable >"$WORK/vtinsert.log" 2>&1 &
PIDS="$PIDS $!"
"$BIN_DIR/victoria-traces-prod" -httpListenAddr=127.0.0.1:30481 \
  -storageNode=127.0.0.1:30491,127.0.0.1:30492 -insert.disable \
  -search.maxTraces="$TRACE_SEARCH_MAX" >"$WORK/vtselect.log" 2>&1 &
PIDS="$PIDS $!"
wait_http http://127.0.0.1:29480/health
wait_http http://127.0.0.1:29482/health
wait_http http://127.0.0.1:29481/health
wait_http http://127.0.0.1:30480/health
wait_http http://127.0.0.1:30481/health

start_path_router() {
  listen=$1 insert=$2 select=$3 delay=${4:-0} output=$5
  "$WORK/e2e-path-router" -listen="127.0.0.1:$listen" -insert-url="$insert" \
    -select-url="$select" -select-delay="${delay}s" >"$output" 2>&1 &
  PIDS="$PIDS $!"
}
CGO_ENABLED=0 go build -o "$WORK/e2e-path-router" ./scripts/e2e-path-router
start_path_router 29429 http://127.0.0.1:29482 http://127.0.0.1:29481 0 "$WORK/logs-router-2.log"
start_path_router 30428 http://127.0.0.1:30480 http://127.0.0.1:30481 0 "$WORK/traces-router.log"
# Both replicas use exactly one logs router and a dedicated vlinsert. Replica A
# delays only selects so the later in-flight drain assertion cannot skew ingest.
start_path_router 29091 http://127.0.0.1:29480 http://127.0.0.1:29481 3 "$WORK/logs-delay-router.log"
wait_http http://127.0.0.1:29429/health
wait_http http://127.0.0.1:30428/health
wait_http http://127.0.0.1:29091/health

: >"$WORK/webhook.count"
"$PY" -c 'import http.server,sys,threading
path=sys.argv[1]
lock=threading.Lock()
class H(http.server.BaseHTTPRequestHandler):
 def do_POST(self):
  with lock:
   n=int(open(path).read() or "0")+1
   open(path,"w").write(str(n))
  self.send_response(204); self.end_headers()
 def log_message(self,*args): pass
http.server.ThreadingHTTPServer(("127.0.0.1",29090),H).serve_forever()' "$WORK/webhook.count" >"$WORK/webhook.log" 2>&1 &
PIDS="$PIDS $!"

CGO_ENABLED=0 go build -o "$WORK/e2e-otlp-payload" ./scripts/e2e-otlp-payload

# HA deployments provide the first admin password: a generated one would only
# land in the bootstrap winner's data_dir. Both replicas read the same 0600 file.
ADMIN_PASSWORD_FILE="$WORK/admin-password"
(umask 077 && "$PY" -c 'import secrets; print(secrets.token_urlsafe(24))' >"$ADMIN_PASSWORD_FILE")
PASSWORD=$(cat "$ADMIN_PASSWORD_FILE")

# Replicas share one metadata database, so they must share one platform
# encryption key. Each replica has its own data_dir; without this file every
# replica would generate its own key and all but the concurrent-bootstrap
# winner would be refused for a fingerprint mismatch.
SECRET_KEY_FILE="$WORK/secret.key"
(umask 077 && "$PY" -c 'import base64, os; print(base64.b64encode(os.urandom(32)).decode())' >"$SECRET_KEY_FILE")

write_config() {
  file=$1 listen=$2 logs_url=$3 metrics_ingest_url=$4 offset=$5
  cat >"$file" <<EOF
listen: "$listen"
data_dir: "${file%.yaml}-data"
standalone: true
storage_bin_dir: "$BIN_DIR"
meta:
  db_url: "postgres://postgres:postgres@$PG_HOST:5432/lazy_observe?sslmode=disable"
redis:
  enabled: true
  addr: "$REDIS_HOST:6379"
  key_prefix: "lazy-observe-e2e"
storage:
  metrics:
    kind: vm
    cluster: true
    tenant_account_id: "42"
    query_url: "http://$VMSELECT_HOST:8481"
    ingest_url: "$metrics_ingest_url"
  logs:
    url: "$logs_url"
  traces:
    url: "http://127.0.0.1:30428"
blob:
  backend: s3
  s3:
    bucket: "$S3_BUCKET"
    region: "us-east-1"
    endpoint: "http://$S3_HOST:8333"
    access_key: "$S3_ACCESS_KEY"
    secret_key: "$S3_SECRET_KEY"
alert:
  group_wait: 100ms
  repeat_interval: 1h
process:
  startup_timeout: 20s
  shutdown_timeout: 10s
  drain_timeout: 10s
  component_port_offset: $offset
self_monitoring:
  enabled: false
security:
  admin_password_file: "$ADMIN_PASSWORD_FILE"
  secret_key_file: "$SECRET_KEY_FILE"
  outbound_allowlist:
    - "127.0.0.1:29090"
EOF
}
write_config "$WORK/replica-a.yaml" 127.0.0.1:23101 http://127.0.0.1:29091 "http://$VMINSERT_1_HOST:8480" 0
write_config "$WORK/replica-b.yaml" 127.0.0.1:23102 http://127.0.0.1:29429 "http://$VMINSERT_2_HOST:8480" 100

write_performance_config() {
  file=$1 listen=$2 offset=$3 sink_url=$4
  cat >"$file" <<EOF
listen: "$listen"
data_dir: "${file%.yaml}-data"
standalone: true
storage_bin_dir: "$BIN_DIR"
meta:
  db_url: "postgres://postgres:postgres@$PG_HOST:5432/lazy_observe?sslmode=disable"
redis:
  enabled: true
  addr: "$REDIS_HOST:6379"
  key_prefix: "lazy-observe-e2e"
storage:
  metrics:
    kind: vm
    cluster: true
    tenant_account_id: "42"
    query_url: "$sink_url"
    ingest_url: "$sink_url"
  logs:
    url: "$sink_url"
  traces:
    url: "$sink_url"
blob:
  backend: s3
  s3:
    bucket: "$S3_BUCKET"
    region: "us-east-1"
    endpoint: "http://$S3_HOST:8333"
    access_key: "$S3_ACCESS_KEY"
    secret_key: "$S3_SECRET_KEY"
scrape:
  mode: disabled
process:
  startup_timeout: 20s
  shutdown_timeout: 10s
  drain_timeout: 10s
  component_port_offset: $offset
self_monitoring:
  enabled: false
security:
  secret_key_file: "$SECRET_KEY_FILE"
EOF
}

# replica 只访问本机与内网容器 IP，unset 代理让其直连。
GOMAXPROCS="$GATEWAY_GOMAXPROCS" env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
  "$BINARY" server --config "$WORK/replica-a.yaml" >"$WORK/a.log" 2>&1 &
PID_A=$!
PIDS="$PIDS $PID_A"
GOMAXPROCS="$GATEWAY_GOMAXPROCS" env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
  "$BINARY" server --config "$WORK/replica-b.yaml" >"$WORK/b.log" 2>&1 &
PID_B=$!
PIDS="$PIDS $PID_B"
wait_ready http://127.0.0.1:23101
wait_ready http://127.0.0.1:23102
echo "PASS: both application replicas started concurrently against a blank PostgreSQL database"

TOKEN=$(sed -n 's/.*created default project.*project_api_token=\([^ ]*\).*/\1/p' "$WORK/a.log" "$WORK/b.log" | head -1)
if [ -z "$PASSWORD" ] || [ -z "$TOKEN" ]; then
  echo "FAIL: failed to read bootstrap credentials" >&2
  exit 1
fi
ADMIN_CREATIONS=$(cat "$WORK/a.log" "$WORK/b.log" | grep -c 'created bootstrap admin.*password_source=admin_password_file' || true)
if [ "$ADMIN_CREATIONS" != 1 ]; then
  echo "FAIL: exactly one replica must create the admin from admin_password_file (got $ADMIN_CREATIONS)" >&2
  exit 1
fi
if grep -Fq -- "$PASSWORD" "$WORK/a.log" "$WORK/b.log" ||
  grep -q 'initial admin password written' "$WORK/a.log" "$WORK/b.log" ||
  [ -e "$WORK/replica-a-data/initial-admin-password" ] || [ -e "$WORK/replica-b-data/initial-admin-password" ]; then
  echo "FAIL: admin_password_file bootstrap logged the password or generated another one" >&2
  exit 1
fi
echo "PASS: admin bootstrapped once from admin_password_file without logging the password"

curl -fsS -c "$WORK/cookies" -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}" http://127.0.0.1:23101/api/auth/login >/dev/null
curl -fsS -b "$WORK/cookies" http://127.0.0.1:23102/api/auth/me >/dev/null
echo "PASS: session created on replica A is valid on replica B"

# 管理 API 的 mutating 请求（带 lo_session cookie）需要 double-submit CSRF：
# 从登录时写入的 lo_csrf cookie 提取值，作为 X-CSRF-Token 请求头。
CSRF=$(awk '$6=="lo_csrf" {print $7; exit}' "$WORK/cookies")
[ -n "$CSRF" ] || { echo "FAIL: no lo_csrf cookie after login" >&2; exit 1; }

PROJECT_TWO=$(curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"name":"HA Project Two","slug":"ha-p2"}' http://127.0.0.1:23101/api/projects)
PROJECT_TWO_ID=$(printf '%s' "$PROJECT_TWO" | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["id"])')
ISSUED_TOKEN=$(curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"name":"ha-e2e"}' "http://127.0.0.1:23101/api/projects/$PROJECT_TWO_ID/tokens")
TOKEN_TWO=$(printf '%s' "$ISSUED_TOKEN" | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["value"])')
# Project tokens ($TOKEN for slug "default", $TOKEN_TWO for slug "ha-p2") are
# ingest-only. Every read uses the admin's user API token plus X-LO-Project.
ISSUED_USER_TOKEN=$(curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"name":"ha-e2e-read"}' http://127.0.0.1:23101/api/auth/tokens)
READ_TOKEN=$(printf '%s' "$ISSUED_USER_TOKEN" | "$PY" -c 'import json,sys; v=json.load(sys.stdin)["value"]; assert v.startswith("lou_"); print(v)')
PROJECT_DEFAULT=default
PROJECT_TWO_SLUG=ha-p2

"$WORK/e2e-otlp-payload" -kind metrics -name ha_cluster_metric -value 11 >"$WORK/metric-default.pb"
"$WORK/e2e-otlp-payload" -kind metrics -name ha_cluster_metric -value 22 >"$WORK/metric-p2.pb"
# The client tries to forge project_id. The gateway must strip it before it
# writes to the platform VM account and inject its trusted project label.
post_payload 'http://127.0.0.1:23101/v1/metrics?extra_label=project_id%3Dha-p2' "$TOKEN" application/x-protobuf "$WORK/metric-default.pb" "default metric ingest"
post_payload http://127.0.0.1:23102/v1/metrics "$TOKEN_TWO" application/x-protobuf "$WORK/metric-p2.pb" "project-two metric ingest"

wait_metric_value() {
  base=$1 project=$2 expected=$3 output=$4
  count=0
  while [ "$count" -lt 30 ]; do
    curl -fsSG -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $project" "$base/api/v1/query" \
      --data-urlencode 'query=ha_cluster_metric' --data-urlencode 'nocache=1' >"$output"
    if "$PY" -c 'import json,sys
p=json.load(open(sys.argv[1])); rows=p.get("data",{}).get("result",[])
sys.exit(0 if len(rows)==1 and float(rows[0]["value"][1])==float(sys.argv[2]) else 1)' "$output" "$expected"; then
      return 0
    fi
    count=$((count + 1))
    sleep 1
  done
  echo "FAIL: metric value did not converge to $expected: $(cat "$output")" >&2
  return 1
}
wait_metric_value http://127.0.0.1:23102 "$PROJECT_DEFAULT" 11 "$WORK/metric-default.json"
wait_metric_value http://127.0.0.1:23101 "$PROJECT_TWO_SLUG" 22 "$WORK/metric-p2.json"
curl -fsSG -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_TWO_SLUG" http://127.0.0.1:23102/api/v1/query \
  --data-urlencode 'query=ha_cluster_metric' --data-urlencode 'extra_filters[]={project_id="default"}' >"$WORK/metric-hostile-query.json"
"$PY" -c 'import json,sys
p=json.load(open(sys.argv[1])); rows=p.get("data",{}).get("result",[])
assert len(rows)==1 and float(rows[0]["value"][1])==22' "$WORK/metric-hostile-query.json"
curl -fsSG http://$VMSELECT_HOST:8481/select/41/prometheus/api/v1/query \
  --data-urlencode 'query=ha_cluster_metric' >"$WORK/metric-other-account.json"
"$PY" -c 'import json,sys
p=json.load(open(sys.argv[1])); assert p.get("data",{}).get("result",[])==[]' "$WORK/metric-other-account.json"
echo "PASS: VM cluster account 42 preserves project_id label isolation across replicas"

# 跨副本吊销：revision 只是副本内的计数器（meta.Store.revision），没有跨副本广播。吊销后本副本立即清缓存，
# 其它副本靠 tokenCacheTTL（internal/gateway/gateway.go，30s，自写入起算，不滑动）过期。项目 API Token（lo_）
# 和 Source Map 上传 Token（los_）走这套缓存；用户 Token（lou_）每次请求都查库，不缓存。
# 每类 token：A 创建，先在 B、再在 A 各用一次（两边都缓存住），在 A 上吊销；A 必须立即 401，B 必须在
# TTL + 余量内开始 401，之后保持 401。三类在后台并行轮询 B，结果留到后段（doctor/drain 之前；
# 性能门禁模式下在测量开始前）再收，不占用主线程的等待时间。
REVOKE_TTL=30 # 与 tokenCacheTTL 保持一致
REVOKE_MARGIN=15
REVOKE_BOUND=$((REVOKE_TTL + REVOKE_MARGIN))
printf '{"version":3,"file":"ha.js","sources":["ha.ts"],"names":[],"mappings":"AAAA"}' >"$WORK/ha-revoke.js.map"
"$WORK/e2e-otlp-payload" -kind logs -name "ha-revoke-$RUN_ID" >"$WORK/revoke-log.pb"

# probe_token <kind> <base> <token>：用该类 token 访问一个只认它的端点，打印 HTTP 状态码（传输失败打印 000）。
# 末尾 exec curl，所以只能在子 shell（$(...) 或 &）里调用：后台轮询拿到的 $! 就是 curl 本身，清理时能直接结束它。
probe_token() {
  case "$1" in
  project)
    set -- -H 'Content-Type: application/x-protobuf' --data-binary @"$WORK/revoke-log.pb" \
      -H "Authorization: Bearer $3" "$2/v1/logs"
    ;;
  sourcemap)
    set -- -F "ha.js=@$WORK/ha-revoke.js.map;filename=ha.js.map" -H "Authorization: Bearer $3" \
      "$2/api/releases/ha-revoke-$RUN_ID/sourcemaps"
    ;;
  user)
    set -- -H "Authorization: Bearer $3" "$2/api/auth/me"
    ;;
  esac
  exec curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$@" 2>/dev/null
}
expect_accepted() {
  status=$(probe_token "$1" "$2" "$3") || :
  case "$status" in
  2??) ;;
  *)
    echo "FAIL: $1 token was not accepted by $2 before revocation (HTTP $status)" >&2
    exit 1
    ;;
  esac
}
json_field() {
  "$PY" -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"
}
revoke_token() {
  case "$1" in
  project) path="projects/$PROJECT_TWO_ID/tokens/$REVOKE_PROJECT_ID" ;;
  sourcemap) path="projects/$PROJECT_TWO_ID/source-map-upload-tokens/$REVOKE_SOURCEMAP_ID" ;;
  user) path="auth/tokens/$REVOKE_USER_ID" ;;
  esac
  curl -fsS -X DELETE -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" "http://127.0.0.1:23101/api/$path" >/dev/null
}
token_of() {
  case "$1" in
  project) printf '%s' "$REVOKE_PROJECT_TOKEN" ;;
  sourcemap) printf '%s' "$REVOKE_SOURCEMAP_TOKEN" ;;
  user) printf '%s' "$REVOKE_USER_TOKEN" ;;
  esac
}
expect_rejected_on_a() {
  status=$(probe_token "$1" http://127.0.0.1:23101 "$(token_of "$1")") || :
  [ "$status" = 401 ] || {
    echo "FAIL: revoked $1 token still got HTTP $status on the revoking replica A, want 401 immediately" >&2
    exit 1
  }
}

REVOKE_PROJECT_JSON=$(curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"name":"ha-revoke"}' "http://127.0.0.1:23101/api/projects/$PROJECT_TWO_ID/tokens")
REVOKE_PROJECT_TOKEN=$(printf '%s' "$REVOKE_PROJECT_JSON" | json_field value)
REVOKE_PROJECT_ID=$(printf '%s' "$REVOKE_PROJECT_JSON" | json_field id)
REVOKE_SOURCEMAP_JSON=$(curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"name":"ha-revoke"}' "http://127.0.0.1:23101/api/projects/$PROJECT_TWO_ID/source-map-upload-tokens")
REVOKE_SOURCEMAP_TOKEN=$(printf '%s' "$REVOKE_SOURCEMAP_JSON" | json_field value)
REVOKE_SOURCEMAP_ID=$(printf '%s' "$REVOKE_SOURCEMAP_JSON" | json_field id)
REVOKE_USER_JSON=$(curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' \
  -d '{"name":"ha-revoke"}' http://127.0.0.1:23101/api/auth/tokens)
REVOKE_USER_TOKEN=$(printf '%s' "$REVOKE_USER_JSON" | json_field value)
REVOKE_USER_ID=$(printf '%s' "$REVOKE_USER_JSON" | json_field id)
case "$REVOKE_PROJECT_TOKEN:$REVOKE_SOURCEMAP_TOKEN:$REVOKE_USER_TOKEN" in
lo_*:los_*:lou_*) ;;
*)
  echo "FAIL: unexpected token prefixes for the revocation test" >&2
  exit 1
  ;;
esac

for kind in project sourcemap user; do
  expect_accepted "$kind" http://127.0.0.1:23102 "$(token_of "$kind")"
done
# A 的缓存条目写在这之后；下面 A 上的 401 必须在 TTL 过期之前拿到，才能证明是吊销清掉了缓存而不是条目碰巧过期。
REVOKE_A_CACHED_AT=$(date +%s)
for kind in project sourcemap user; do
  expect_accepted "$kind" http://127.0.0.1:23101 "$(token_of "$kind")"
done

# 逐类吊销。两个 Authenticator 共用同一个 revision，吊销 lo_ 也会清掉 A 上 los_ 的缓存；所以吊销 lo_ 之后
# 先在 A 上重新用一次 los_ 让它再缓存住，这样 los_ 的 401 只能来自它自己的吊销清缓存。
revoke_token project
REVOKED_AT_PROJECT=$(date +%s)
expect_rejected_on_a project
expect_accepted sourcemap http://127.0.0.1:23101 "$REVOKE_SOURCEMAP_TOKEN"
revoke_token sourcemap
REVOKED_AT_SOURCEMAP=$(date +%s)
expect_rejected_on_a sourcemap
revoke_token user
REVOKED_AT_USER=$(date +%s)
expect_rejected_on_a user
REVOKE_A_GAP=$(($(date +%s) - REVOKE_A_CACHED_AT))
# date +%s 只有秒精度，留 2 秒：实际间隔 <= REVOKE_A_GAP + 1 < TTL，A 上的条目还没过期。
[ "$REVOKE_A_GAP" -le $((REVOKE_TTL - 2)) ] || {
  echo "FAIL: replica A's revocation checks finished ${REVOKE_A_GAP}s after caching, too late to tell cache clearing from ${REVOKE_TTL}s TTL expiry" >&2
  exit 1
}
echo "PASS: revoked project (lo_), source map upload (los_) and user (lou_) tokens are rejected by the revoking replica A immediately (${REVOKE_A_GAP}s after caching on A, TTL ${REVOKE_TTL}s)"

# 副本 B：轮询到 401，截止时间从该 token 的吊销时刻起算；之后再确认保持 401，防止回弹。
# B 的缓存条目写于吊销之前，过期点早于「吊销时刻 + TTL」；吊销之后 B 只会查到已吊销，不会再写入新条目。
# 子进程（curl、sleep）都放后台再 wait：前台等待会推迟 TERM trap，清理时子进程会成为孤儿。
await_revoked_on_b() {
  kind=$1 token=$2 revoked_at=$3 result=$4
  child=""
  trap '[ -z "$child" ] || kill -TERM "$child" 2>/dev/null; exit 143' TERM
  rejected_after=""
  confirmed=0
  while [ "$confirmed" -lt 3 ]; do
    probe_token "$kind" http://127.0.0.1:23102 "$token" >"$result.status" &
    child=$!
    wait "$child" || :
    child=""
    status=$(cat "$result.status")
    if [ -n "$rejected_after" ]; then
      [ "$status" = 401 ] || {
        echo "FAIL: replica B accepted a revoked $kind token again (HTTP $status) after rejecting it" >"$result"
        return 1
      }
      confirmed=$((confirmed + 1))
    elif [ "$status" = 401 ]; then
      rejected_after=$(($(date +%s) - revoked_at))
    elif [ $(($(date +%s) - revoked_at)) -gt "$REVOKE_BOUND" ]; then
      echo "FAIL: replica B still answered HTTP $status for a revoked $kind token ${REVOKE_BOUND}s after revocation (TTL ${REVOKE_TTL}s)" >"$result"
      return 1
    fi
    sleep 1 &
    child=$!
    wait "$child" || :
    child=""
  done
  echo "PASS: replica B rejected the revoked $kind token ${rejected_after}s after revocation (bound ${REVOKE_BOUND}s)" >"$result"
}
REVOKE_PIDS=""
for kind in project sourcemap user; do
  case "$kind" in
  project) revoked_at=$REVOKED_AT_PROJECT ;;
  sourcemap) revoked_at=$REVOKED_AT_SOURCEMAP ;;
  user) revoked_at=$REVOKED_AT_USER ;;
  esac
  await_revoked_on_b "$kind" "$(token_of "$kind")" "$revoked_at" "$WORK/revoke-$kind.result" &
  PIDS="$PIDS $!"
  REVOKE_PIDS="$REVOKE_PIDS $kind:$!"
done
# 收取 B 侧结果；只收一次，第二次调用直接返回。
collect_revoke_results() {
  [ -n "$REVOKE_PIDS" ] || return 0
  revoke_failed=0
  for entry in $REVOKE_PIDS; do
    kind=${entry%%:*}
    pid=${entry#*:}
    if wait "$pid"; then :; else revoke_failed=1; fi
    forget_pid "$pid"
    cat "$WORK/revoke-$kind.result" >&2 2>/dev/null || {
      echo "FAIL: the replica B revocation poller for the $kind token left no result" >&2
      revoke_failed=1
    }
  done
  REVOKE_PIDS=""
  [ "$revoke_failed" = 0 ] || {
    echo "FAIL: a revoked token stayed valid on replica B beyond the cache TTL plus margin" >&2
    exit 1
  }
  echo "PASS: revoking a project, source map upload or user token on replica A invalidates it on replica B within the ${REVOKE_TTL}s cache TTL plus ${REVOKE_MARGIN}s margin"
}

DEFAULT_LOG="ha-vlogs-default-$RUN_ID"
P2_LOG="ha-vlogs-p2-$RUN_ID"
"$WORK/e2e-otlp-payload" -kind logs -name "$DEFAULT_LOG" >"$WORK/log-default.pb"
"$WORK/e2e-otlp-payload" -kind logs -name "$P2_LOG" >"$WORK/log-p2.pb"
post_payload http://127.0.0.1:23101/v1/logs "$TOKEN" application/x-protobuf "$WORK/log-default.pb" "default log ingest"
post_payload http://127.0.0.1:23102/v1/logs "$TOKEN_TWO" application/x-protobuf "$WORK/log-p2.pb" "project-two log ingest"

wait_log_marker() {
  base=$1 project=$2 marker=$3 output=$4
  count=0
  while [ "$count" -lt 30 ]; do
    curl -fsSG -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $project" "$base/api/v1/logs/query" --data-urlencode "query=_msg:=\"$marker\"" >"$output"
    grep -F "$marker" "$output" >/dev/null && return 0
    count=$((count + 1))
    sleep 1
  done
  echo "FAIL: log marker $marker not found: $(cat "$output")" >&2
  return 1
}
wait_log_marker http://127.0.0.1:23102 "$PROJECT_DEFAULT" "$DEFAULT_LOG" "$WORK/log-default.jsonl"
wait_log_marker http://127.0.0.1:23101 "$PROJECT_TWO_SLUG" "$P2_LOG" "$WORK/log-p2.jsonl"
curl -fsSG -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_TWO_SLUG" http://127.0.0.1:23102/api/v1/logs/query --data-urlencode "query=_msg:=\"$DEFAULT_LOG\"" >"$WORK/log-isolation.jsonl"
if grep -F "$DEFAULT_LOG" "$WORK/log-isolation.jsonl" >/dev/null; then
  echo "FAIL: project two read the default project's VictoriaLogs row" >&2
  exit 1
fi
curl -fsSG -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_DEFAULT" http://127.0.0.1:23101/api/v1/logs/query --data-urlencode "query=_msg:=\"$P2_LOG\"" >"$WORK/log-isolation-reverse.jsonl"
if grep -F "$P2_LOG" "$WORK/log-isolation-reverse.jsonl" >/dev/null; then
  echo "FAIL: default project read project two's VictoriaLogs row" >&2
  exit 1
fi
echo "PASS: VLogs vlinsert/vlselect/two-vlstorage cluster enforces native ProjectID isolation in both directions"

DEFAULT_SERVICE="ha-traces-default-$RUN_ID"
P2_SERVICE="ha-traces-p2-$RUN_ID"
NOW_NANO=$("$PY" -c 'import time; print(time.time_ns())')
printf '{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"%s"}}]},"scopeSpans":[{"scope":{"name":"ha-e2e"},"spans":[{"traceId":"11111111111111111111111111111111","spanId":"1111111111111111","name":"default","kind":1,"startTimeUnixNano":"%s","endTimeUnixNano":"%s","status":{}}]}]}]}' "$DEFAULT_SERVICE" "$NOW_NANO" "$((NOW_NANO + 1000000))" >"$WORK/trace-default.json"
NOW_NANO=$("$PY" -c 'import time; print(time.time_ns())')
printf '{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"%s"}}]},"scopeSpans":[{"scope":{"name":"ha-e2e"},"spans":[{"traceId":"22222222222222222222222222222222","spanId":"2222222222222222","name":"p2","kind":1,"startTimeUnixNano":"%s","endTimeUnixNano":"%s","status":{}}]}]}]}' "$P2_SERVICE" "$NOW_NANO" "$((NOW_NANO + 1000000))" >"$WORK/trace-p2.json"
post_payload http://127.0.0.1:23101/v1/traces "$TOKEN" application/json "$WORK/trace-default.json" "default trace ingest"
post_payload http://127.0.0.1:23102/v1/traces "$TOKEN_TWO" application/json "$WORK/trace-p2.json" "project-two trace ingest"

wait_trace_service() {
  base=$1 project=$2 service=$3 output=$4
  count=0
  while [ "$count" -lt 30 ]; do
    curl -fsS -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $project" "$base/api/traces/services" >"$output"
    grep -F "$service" "$output" >/dev/null && return 0
    count=$((count + 1))
    sleep 1
  done
  echo "FAIL: trace service $service not found: $(cat "$output")" >&2
  return 1
}
wait_trace_detail() {
  base=$1 project=$2 trace_id=$3 output=$4
  count=0
  while [ "$count" -lt 60 ]; do
    status=$(curl -sS -o "$output" -w '%{http_code}' -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $project" \
      "$base/api/traces/$trace_id" 2>/dev/null || true)
    if [ "$status" = 200 ] && "$PY" - "$output" "$trace_id" <<'PY'; then
import json
import sys

payload = json.load(open(sys.argv[1]))
trace_id = sys.argv[2]
found = any(
    trace.get("traceID") == trace_id
    and any(span.get("traceID") == trace_id and span.get("spanID") and span.get("operationName") for span in trace.get("spans", []))
    for trace in payload.get("data", [])
)
raise SystemExit(0 if found else 1)
PY
      return 0
    fi
    count=$((count + 1))
    sleep 1
  done
  echo "FAIL: trace detail $trace_id did not become queryable (last HTTP ${status:-transport-error})" >&2
  return 1
}
wait_trace_service http://127.0.0.1:23102 "$PROJECT_DEFAULT" "$DEFAULT_SERVICE" "$WORK/trace-services-default.json"
wait_trace_service http://127.0.0.1:23101 "$PROJECT_TWO_SLUG" "$P2_SERVICE" "$WORK/trace-services-p2.json"
if grep -F "$DEFAULT_SERVICE" "$WORK/trace-services-p2.json" >/dev/null; then
  echo "FAIL: project two read the default project's VictoriaTraces service" >&2
  exit 1
fi
curl -fsS -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_DEFAULT" http://127.0.0.1:23101/api/traces/services >"$WORK/trace-isolation-reverse.json"
if grep -F "$P2_SERVICE" "$WORK/trace-isolation-reverse.json" >/dev/null; then
  echo "FAIL: default project read project two's VictoriaTraces service" >&2
  exit 1
fi
wait_trace_detail http://127.0.0.1:23102 "$PROJECT_TWO_SLUG" 22222222222222222222222222222222 "$WORK/trace-p2-detail.json"
echo "PASS: VTraces vtinsert/vtselect/two-vtstorage cluster enforces native ProjectID isolation in both directions"

if [ "${LO_E2E_PERF_GATES:-0}" = 1 ]; then
  # 后台吊销轮询会持续打副本 B（摄取、上传 Source Map），先收完再灌夹具、测查询 P95，免得干扰测量。
  collect_revoke_results
  PERF_COUNTS=$("$PY" -c 'import json,sys
p=json.load(open(sys.argv[1]))["query"]["medium_fixture"]
print(p["issues"], p["logs"], p["traces"])' "$ROOT/bench/performance-gates.json")
  set -- $PERF_COUNTS
  PERF_ISSUE_COUNT=$1
  PERF_LOG_COUNT=$2
  PERF_TRACE_COUNT=$3
  PERF_QUERY_START=$("$PY" -c 'import time; print(int(time.time())-3600)')
  PERF_QUERY_END=$("$PY" -c 'import time; print(int(time.time())+600)')

  PERF_LOG_MARKER="ha-perf-log-$RUN_ID"
  "$WORK/e2e-otlp-payload" -kind logs -name "$PERF_LOG_MARKER" -count "$PERF_LOG_COUNT" >"$WORK/perf-logs.pb"
  post_payload http://127.0.0.1:23102/v1/logs "$TOKEN_TWO" application/x-protobuf "$WORK/perf-logs.pb" "medium log fixture"

  mkdir -p "$WORK/perf-issue-batches"
  "$PY" - "$WORK/perf-issue-batches" "$RUN_ID" "$PERF_ISSUE_COUNT" <<'PY'
import datetime
import json
import pathlib
import sys

output, run_id, count = pathlib.Path(sys.argv[1]), sys.argv[2], int(sys.argv[3])
timestamp = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z")
for batch_start in range(0, count, 100):
    events = []
    for index in range(batch_start, min(batch_start + 100, count)):
        events.append({
            "event_id": f"perf-{run_id}-{index:05d}",
            "timestamp": timestamp,
            "platform": "go",
            "level": "error",
            "exception": {
                "type": f"PerfGateError{index:05d}",
                "value": f"controlled medium performance fixture {index:05d}",
                "stacktrace": {"frames": [{
                    "function": f"perffixture.Handler{index:05d}",
                    "filename": f"perf_fixture_{index:05d}.go",
                    "lineno": 100 + index,
                    "in_app": True,
                }]},
            },
        })
    path = output / f"batch-{batch_start // 100:05d}.json"
    path.write_text(json.dumps({"events": events}, separators=(",", ":")), encoding="utf-8")
PY
  for payload in "$WORK"/perf-issue-batches/*.json; do
    post_payload http://127.0.0.1:23102/api/events/errors "$TOKEN_TWO" application/json "$payload" "medium Issue fixture batch"
  done

  PERF_TRACE_SERVICE="ha-perf-traces-$RUN_ID"
  PERF_TRACE_ID=00000000000000000000000000000001
  "$PY" - "$PERF_TRACE_SERVICE" "$PERF_TRACE_COUNT" >"$WORK/perf-traces.json" <<'PY'
import json
import sys
import time

service, count = sys.argv[1], int(sys.argv[2])
now = time.time_ns()
spans = []
for index in range(count):
    sequence = index + 1
    spans.append({
        "traceId": f"{sequence:032x}",
        "spanId": f"{sequence:016x}",
        "name": "medium-fixture",
        "kind": 1,
        "startTimeUnixNano": str(now + index * 1000),
        "endTimeUnixNano": str(now + index * 1000 + 1000000),
        "status": {},
    })
payload = {"resourceSpans": [{
    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
    "scopeSpans": [{"scope": {"name": "ha-performance-gate"}, "spans": spans}],
}]}
json.dump(payload, sys.stdout, separators=(",", ":"))
PY
  post_payload http://127.0.0.1:23102/v1/traces "$TOKEN_TWO" application/json "$WORK/perf-traces.json" "medium trace fixture"

  PERF_ISSUE_ID=""
  count=0
  while [ "$count" -lt 40 ]; do
    curl -fsS -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_TWO_SLUG" \
      'http://127.0.0.1:23101/api/v1/issues?status=open&sort=last_seen&page=1' >"$WORK/perf-issue-list.json"
    PERF_ISSUE_ID=$(
      "$PY" - "$WORK/perf-issue-list.json" "$PERF_ISSUE_COUNT" <<'PY' || true
import json
import sys

payload = json.load(open(sys.argv[1]))
minimum = int(sys.argv[2])
issues = payload.get("issues", [])
if payload.get("total", 0) < minimum or len(issues) < 50:
    raise SystemExit(1)
print(issues[0]["id"])
PY
    )
    [ -n "$PERF_ISSUE_ID" ] && break
    count=$((count + 1))
    sleep 1
  done
  [ -n "$PERF_ISSUE_ID" ] || {
    echo "FAIL: medium Issue fixture did not flush" >&2
    exit 1
  }

  count=0
  while [ "$count" -lt 60 ]; do
    curl -fsSG -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_TWO_SLUG" http://127.0.0.1:23102/api/v1/logs/hits \
      --data-urlencode "query=_msg:=\"$PERF_LOG_MARKER\"" \
      --data-urlencode "start=$PERF_QUERY_START" --data-urlencode "end=$PERF_QUERY_END" \
      --data-urlencode 'step=1h' >"$WORK/perf-log-hits.json"
    if "$PY" - "$WORK/perf-log-hits.json" "$PERF_LOG_COUNT" <<'PY'; then
import json
import sys

payload = json.load(open(sys.argv[1]))
total = sum(int(hit.get("total", -1)) for hit in payload.get("hits", []))
raise SystemExit(0 if total >= int(sys.argv[2]) else 1)
PY
      break
    fi
    count=$((count + 1))
    sleep 1
  done
  [ "$count" -lt 60 ] || {
    echo "FAIL: medium log fixture did not become countable" >&2
    exit 1
  }

  count=0
  while [ "$count" -lt 60 ]; do
    curl -fsSG -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_TWO_SLUG" http://127.0.0.1:23102/api/traces/search \
      --data-urlencode "service=$PERF_TRACE_SERVICE" --data-urlencode "limit=$PERF_TRACE_COUNT" \
      --data-urlencode "start=$((PERF_QUERY_START * 1000000))" \
      --data-urlencode "end=$((PERF_QUERY_END * 1000000))" >"$WORK/perf-trace-search.json"
    if "$PY" - "$WORK/perf-trace-search.json" "$PERF_TRACE_COUNT" "$PERF_TRACE_ID" <<'PY'; then
import json
import sys

payload = json.load(open(sys.argv[1]))
trace_ids = {trace.get("traceID") for trace in payload.get("data", []) if trace.get("traceID")}
raise SystemExit(0 if len(trace_ids) >= int(sys.argv[2]) and sys.argv[3] in trace_ids else 1)
PY
      break
    fi
    count=$((count + 1))
    sleep 1
  done
  [ "$count" -lt 60 ] || {
    echo "FAIL: medium trace fixture did not become searchable" >&2
    exit 1
  }
  echo "PASS: medium query corpus has at least $PERF_ISSUE_COUNT Issues, $PERF_LOG_COUNT logs, and $PERF_TRACE_COUNT traces"

  CGO_ENABLED=0 go build -o "$WORK/perf-gate" ./cmd/perf-gate

  # Capacity scaling is a platform-gateway claim. Run two otherwise equivalent
  # one-core gateways against one shared, validating, non-persistent OTLP sink
  # so the tiny co-located VM/VLogs fixture cannot become the measured ceiling.
  # Every measured marker is separately mirrored through the real HA gateways
  # and queried back from real VM/VLogs. Direct-to-sink control rounds must also
  # prove at least the configured headroom over the scaled gateway result.
  CGO_ENABLED=0 go build -o "$WORK/e2e-otlp-sink" ./scripts/e2e-otlp-sink
  PERF_SINK_URL=http://127.0.0.1:31480
  PERF_BATCH_SIZE=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["ha_scaling"]["batch_size"])' "$ROOT/bench/performance-gates.json")
  case "$PERF_BATCH_SIZE" in '' | *[!0-9]* | 0)
    echo "FAIL: invalid performance batch size" >&2
    exit 1
    ;;
  esac
  # The sink is supporting test infrastructure, not a replica under test. Let
  # it use all available host CPUs while the two measured gateways stay pinned
  # to GATEWAY_GOMAXPROCS, and still prove >=2x headroom with direct controls.
  env -u GOMAXPROCS "$WORK/e2e-otlp-sink" -listen=127.0.0.1:31480 -expected-batch="$PERF_BATCH_SIZE" >"$WORK/perf-sink.log" 2>&1 &
  PERF_SINK_PID=$!
  PIDS="$PIDS $PERF_SINK_PID"
  wait_http "$PERF_SINK_URL/health"
  write_performance_config "$WORK/perf-replica-a.yaml" 127.0.0.1:23103 200 "$PERF_SINK_URL"
  write_performance_config "$WORK/perf-replica-b.yaml" 127.0.0.1:23104 300 "$PERF_SINK_URL"
  GOMAXPROCS="$GATEWAY_GOMAXPROCS" env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
    "$BINARY" server --config "$WORK/perf-replica-a.yaml" >"$WORK/perf-a.log" 2>&1 &
  PERF_PID_A=$!
  PIDS="$PIDS $PERF_PID_A"
  wait_ready http://127.0.0.1:23103
  GOMAXPROCS="$GATEWAY_GOMAXPROCS" env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
    "$BINARY" server --config "$WORK/perf-replica-b.yaml" >"$WORK/perf-b.log" 2>&1 &
  PERF_PID_B=$!
  PIDS="$PIDS $PERF_PID_B"
  wait_ready http://127.0.0.1:23104

  PERF_OUTPUT_DIR=${LO_PERF_OUTPUT_DIR:-}
  if [ -n "$PERF_OUTPUT_DIR" ]; then
    case "$PERF_OUTPUT_DIR" in /*) ;; *) PERF_OUTPUT_DIR="$ROOT/$PERF_OUTPUT_DIR" ;; esac
    mkdir -p "$PERF_OUTPUT_DIR"
    QUERY_OUTPUT="--output=$PERF_OUTPUT_DIR/query.json"
    SCALE_OUTPUT="--output=$PERF_OUTPUT_DIR/scale.json"
  else
    QUERY_OUTPUT=""
    SCALE_OUTPUT=""
  fi
  # perf-gate query only reads, so it takes the user API token from LO_PERF_QUERY_TOKEN.
  LO_PERF_QUERY_TOKEN="$READ_TOKEN" "$WORK/perf-gate" query \
    --config "$ROOT/bench/performance-gates.json" \
    --base-url http://127.0.0.1:23102 --project "$PROJECT_TWO_SLUG" \
    --issue-id "$PERF_ISSUE_ID" --log-marker "$PERF_LOG_MARKER" \
    --min-issues 50 --min-log-rows 1000 --log-limit 1000 \
    --log-start "$PERF_QUERY_START" --log-end "$PERF_QUERY_END" \
    --trace-id "$PERF_TRACE_ID" --trace-service "$PERF_TRACE_SERVICE" ${QUERY_OUTPUT:+"$QUERY_OUTPUT"}
  # perf-gate scale ingests with the project token and reads with the user token.
  LO_PERF_TOKEN="$TOKEN_TWO" LO_PERF_QUERY_TOKEN="$READ_TOKEN" "$WORK/perf-gate" scale \
    --config "$ROOT/bench/performance-gates.json" --project "$PROJECT_TWO_SLUG" \
    --targets http://127.0.0.1:23103,http://127.0.0.1:23104 \
    --readback-targets http://127.0.0.1:23101,http://127.0.0.1:23102 \
    --controlled-upstream "$PERF_SINK_URL" \
    ${SCALE_OUTPUT:+"$SCALE_OUTPUT"}
  kill -TERM "$PERF_PID_A" "$PERF_PID_B" "$PERF_SINK_PID"
  wait "$PERF_PID_A" "$PERF_PID_B" "$PERF_SINK_PID" || true
  forget_pid "$PERF_PID_A"
  forget_pid "$PERF_PID_B"
  forget_pid "$PERF_SINK_PID"
  echo "PASS: controlled query P95, real-storage payload readback, and capacity-controlled platform scaling gates"
fi

docker stop --time 5 "$VMSTORAGE_1" >/dev/null
"$WORK/e2e-otlp-payload" -kind metrics -name ha_cluster_metric -value 33 >"$WORK/metric-after-node-loss.pb"
post_payload http://127.0.0.1:23102/v1/metrics "$TOKEN" application/x-protobuf "$WORK/metric-after-node-loss.pb" "metric ingest after vmstorage loss"
wait_metric_value http://127.0.0.1:23101 "$PROJECT_DEFAULT" 33 "$WORK/metric-after-node-loss.json"
echo "PASS: VM replicationFactor=2 accepts and serves a fresh metric after one vmstorage node stops"

printf '[{"type":4,"timestamp":1773720000000,"data":{"href":"https://example.invalid/ha"}}]' | gzip -c >"$WORK/replay.json.gz"
curl -fsS -H 'Content-Type: application/octet-stream' -H "Authorization: Bearer $TOKEN" --data-binary @"$WORK/replay.json.gz" \
  'http://127.0.0.1:23101/api/replay/segments?session_id=ha-s3&seq=0' >/dev/null
curl -fsS -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_DEFAULT" \
  'http://127.0.0.1:23102/api/replay/sessions/ha-s3/segments/0' >"$WORK/replay-from-b.json.gz"
gzip -dc "$WORK/replay-from-b.json.gz" >"$WORK/replay-from-b.json"
"$PY" -c 'import json,sys
p=json.load(open(sys.argv[1])); assert p[0]["data"]["href"]=="https://example.invalid/ha"' "$WORK/replay-from-b.json"
echo "PASS: replica A wrote Replay to the real SeaweedFS S3 API and replica B read the same object"

CHANNEL=$(curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'X-LO-Project: default' -H 'Content-Type: application/json' -d '{"type":"webhook","name":"ha-e2e","config":{"url":"http://127.0.0.1:29090"}}' http://127.0.0.1:23101/api/channels)
CHANNEL_ID=$(printf '%s' "$CHANNEL" | "$PY" -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -fsS -b "$WORK/cookies" -X PUT -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json' -d "{\"new_issue\":true,\"regression\":false,\"error_rate_threshold_per_min\":0,\"channel_ids\":[$CHANNEL_ID]}" http://127.0.0.1:23101/api/projects/1/alert-config >/dev/null

EVENT='{"events":[{"event_id":"EVENT_ID","timestamp":"2026-07-17T03:00:00Z","platform":"go","level":"error","exception":{"type":"SharedReplicaError","value":"same fingerprint","stacktrace":{"frames":[{"function":"checkout.Handle","filename":"checkout.go","lineno":42,"in_app":true}]}}}]}'
printf '%s' "$EVENT" | sed 's/EVENT_ID/a-event/' | curl -fsS -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" --data-binary @- http://127.0.0.1:23101/api/events/errors >/dev/null &
POST_A=$!
printf '%s' "$EVENT" | sed 's/EVENT_ID/b-event/' | curl -fsS -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" --data-binary @- http://127.0.0.1:23102/api/events/errors >/dev/null &
POST_B=$!
wait "$POST_A" "$POST_B"

count=0
while [ "$count" -lt 30 ]; do
  ISSUES=$(curl -fsS -b "$WORK/cookies" -H 'X-LO-Project: default' http://127.0.0.1:23102/api/issues)
  ISSUE_COUNT=$(printf '%s' "$ISSUES" | "$PY" -c 'import json,sys; x=json.load(sys.stdin).get("issues",[]); print(x[0]["count"] if x else 0)')
  [ "$ISSUE_COUNT" = 2 ] && break
  count=$((count + 1))
  sleep 1
done
[ "${ISSUE_COUNT:-0}" = 2 ] || {
  echo "FAIL: Issue count=${ISSUE_COUNT:-0}, want 2" >&2
  exit 1
}
echo "PASS: identical Issue count is 2 across both replicas"

count=0
while [ "$count" -lt 30 ]; do
  DELIVERIES=$(cat "$WORK/webhook.count")
  [ "$DELIVERIES" = 1 ] && break
  count=$((count + 1))
  sleep 1
done
# Give a competing Outbox worker enough time to claim the same logical work;
# a second delivery arriving just after the first observation must fail the gate.
sleep 2
DELIVERIES=$(cat "$WORK/webhook.count")
[ "${DELIVERIES:-0}" = 1 ] || {
  echo "FAIL: webhook deliveries=${DELIVERIES:-0}, want 1" >&2
  exit 1
}
echo "PASS: database Outbox workers delivered the platform notification exactly once"

curl -fsS -b "$WORK/cookies" -H "X-CSRF-Token: $CSRF" -H 'X-LO-Project: default' -H 'Content-Type: application/json' \
  -d "{\"name\":\"HA native metric\",\"signal\":\"metrics\",\"expr\":\"ha_cluster_metric > 0\",\"for\":\"0s\",\"severity\":\"critical\",\"channel_ids\":[\"$CHANNEL_ID\"]}" \
  http://127.0.0.1:23102/api/alert-rules >/dev/null
count=0
while [ "$count" -lt 30 ]; do
  DELIVERIES=$(cat "$WORK/webhook.count")
  [ "$DELIVERIES" = 2 ] && break
  count=$((count + 1))
  sleep 1
done
sleep 2
DELIVERIES=$(cat "$WORK/webhook.count")
[ "${DELIVERIES:-0}" = 2 ] || {
  echo "FAIL: native rule webhook deliveries=${DELIVERIES:-0}, want total 2" >&2
  exit 1
}
echo "PASS: two evaluator candidates queried one native rule and committed exactly one notification"

collect_revoke_results

env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
  "$BINARY" doctor --ha --config "$WORK/replica-b.yaml" >"$WORK/doctor.log"
grep -F 'HA ready:' "$WORK/doctor.log" >/dev/null
echo "PASS: doctor --ha verified PostgreSQL, Redis, S3, and clustered signal endpoints"

curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $READ_TOKEN" -H "X-LO-Project: $PROJECT_DEFAULT" 'http://127.0.0.1:23101/api/v1/logs/query?query=*' >"$WORK/inflight.status" &
SLOW=$!
sleep 1
kill -TERM "$PID_A"
count=0
READY_STATUS=""
while [ "$count" -lt 20 ]; do
  READY_STATUS=$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:23101/readyz 2>/dev/null || true)
  [ "$READY_STATUS" = 503 ] && break
  count=$((count + 1))
  sleep 0.02
done
[ "$READY_STATUS" = 503 ] || {
  echo "FAIL: replica A readyz did not expose 503 during drain (last=$READY_STATUS)" >&2
  exit 1
}
wait "$SLOW"
[ "$(cat "$WORK/inflight.status")" = 200 ] || {
  echo "FAIL: in-flight request was interrupted" >&2
  exit 1
}
wait "$PID_A"
PIDS=$(printf '%s' "$PIDS" | sed "s/ $PID_A//")
curl -fsS http://127.0.0.1:23102/readyz >/dev/null
echo "PASS: replica A became unready, completed in-flight work, exited 0; replica B stayed ready"

echo "PASS: cluster and S3 multi-replica e2e"
