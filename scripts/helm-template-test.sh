#!/bin/sh
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
CHART="$ROOT/deploy/helm/lazy-observe"
HELM=${HELM:-helm}

if ! command -v "$HELM" >/dev/null 2>&1; then
  echo "helm not found; install Helm 3 or set HELM=/path/to/helm" >&2
  exit 1
fi

TMPDIR_ROOT=${TMPDIR:-/tmp}
WORK=$(mktemp -d "$TMPDIR_ROOT/lazy-observe-helm.XXXXXX")
trap 'rm -rf "$WORK"' EXIT HUP INT TERM

"$HELM" template test "$CHART" --set secret.create=true --set hpa.enabled=true >"$WORK/standalone.yaml"
"$HELM" template test "$CHART" --set mode=allinone >"$WORK/allinone.yaml"
"$HELM" template test "$CHART" -f "$CHART/values-standalone-ha-cluster.yaml" --set secret.create=true >"$WORK/cluster.yaml"

assert_contains() {
  file=$1
  pattern=$2
  message=$3
  if ! grep -Eq "$pattern" "$file"; then
    echo "FAIL: $message" >&2
    exit 1
  fi
}

assert_not_contains() {
  file=$1
  pattern=$2
  message=$3
  if grep -Eq "$pattern" "$file"; then
    echo "FAIL: $message" >&2
    exit 1
  fi
}

assert_contains "$WORK/standalone.yaml" 'replicas: 2' 'standalone-ha must render two replicas by default'
assert_contains "$WORK/standalone.yaml" 'path: /livez' 'liveness probe must use /livez'
assert_contains "$WORK/standalone.yaml" 'path: /readyz' 'readiness probe must use /readyz'
assert_contains "$WORK/standalone.yaml" 'kind: HorizontalPodAutoscaler' 'standalone-ha must support HPA'
assert_contains "$WORK/standalone.yaml" 'standalone: true' 'standalone-ha config must use external storage'
assert_contains "$WORK/standalone.yaml" 'migration_lock_timeout: 60s' 'metadata migration lock wait must be bounded'
assert_contains "$WORK/standalone.yaml" 'kind: PodDisruptionBudget' 'PDB must render'
assert_contains "$WORK/standalone.yaml" 'terminationGracePeriodSeconds: 45' 'termination grace must cover drain and child shutdown'

assert_contains "$WORK/allinone.yaml" 'replicas: 1' 'allinone must force a single replica'
assert_contains "$WORK/allinone.yaml" 'type: Recreate' 'allinone must avoid concurrent PVC users'
assert_contains "$WORK/allinone.yaml" 'kind: PersistentVolumeClaim' 'allinone must render a PVC'
assert_contains "$WORK/allinone.yaml" 'standalone: false' 'allinone config must enable embedded storage'
assert_not_contains "$WORK/allinone.yaml" 'kind: HorizontalPodAutoscaler' 'allinone must not render HPA'

for rendered in "$WORK/standalone.yaml" "$WORK/allinone.yaml" "$WORK/cluster.yaml"; do
  assert_not_contains "$rendered" '^[[:space:]]+ai:$' 'Helm must not render the removed ai configuration section'
  assert_not_contains "$rendered" 'LO_AI_' 'Helm must not render removed AI environment variables'
  assert_not_contains "$rendered" 'ai-api-key' 'Helm must not render the removed AI secret key'
done

assert_contains "$WORK/cluster.yaml" 'replicas: 3' 'cluster profile must render three gateway replicas'
assert_contains "$WORK/cluster.yaml" 'cluster: true' 'cluster profile must enable VictoriaMetrics cluster paths'
assert_contains "$WORK/cluster.yaml" 'tenant_account_id: "42"' 'cluster profile must render the VM platform account'
assert_contains "$WORK/cluster.yaml" 'query_url: "http://vmselect\.monitoring\.svc:8481"' 'cluster profile must query vmselect'
assert_contains "$WORK/cluster.yaml" 'ingest_url: "http://vminsert\.monitoring\.svc:8480"' 'cluster profile must ingest through vminsert'
assert_contains "$WORK/cluster.yaml" 'url: "http://vlogs-gateway\.monitoring\.svc:9428"' 'cluster profile must render the VLogs path router'
assert_contains "$WORK/cluster.yaml" 'url: "http://vtraces-gateway\.monitoring\.svc:10428"' 'cluster profile must render the VTraces path router'
assert_contains "$WORK/cluster.yaml" 'backend: s3' 'cluster profile must use shared S3 blobs'
assert_contains "$WORK/cluster.yaml" 'bucket: "lazy-observe"' 'cluster profile must render the S3 bucket'
assert_contains "$WORK/cluster.yaml" 'kind: HorizontalPodAutoscaler' 'cluster profile must render HPA'
assert_contains "$WORK/cluster.yaml" 'minAvailable: 2' 'cluster profile must keep two replicas available during disruption'
assert_contains "$WORK/cluster.yaml" 'podAntiAffinity:' 'cluster profile must spread gateway replicas when possible'
assert_contains "$WORK/cluster.yaml" 'terminationGracePeriodSeconds: 60' 'cluster profile grace period must cover drain and child shutdown'

# Platform encryption key: every replica must read one shared Kubernetes Secret
# through security.secret_key_file instead of generating its own key in the
# per-pod emptyDir data directory.
KEY_PATH=/etc/lazy-observe-key/secret.key
"$HELM" template test "$CHART" --set encryptionKey.existingSecret=lo-platform-key \
  --set encryptionKey.key=platform.key >"$WORK/key-existing.yaml"
"$HELM" template test "$CHART" --set mode=allinone --set persistence.enabled=false >"$WORK/allinone-ephemeral.yaml"

assert_render_fails() {
  message=$1
  expected=$2
  shift 2
  if "$HELM" template test "$CHART" "$@" >"$WORK/fail.yaml" 2>"$WORK/fail.err"; then
    echo "FAIL: $message (render succeeded)" >&2
    exit 1
  fi
  if ! grep -Fq "$expected" "$WORK/fail.err"; then
    echo "FAIL: $message (missing error text: $expected)" >&2
    cat "$WORK/fail.err" >&2
    exit 1
  fi
}

for rendered in "$WORK/standalone.yaml" "$WORK/cluster.yaml" "$WORK/key-existing.yaml" "$WORK/allinone-ephemeral.yaml"; do
  assert_contains "$rendered" "secret_key_file: \"$KEY_PATH\"" 'secret_key_file must point at the mounted shared key'
  assert_contains "$rendered" 'mountPath: /etc/lazy-observe-key$' 'the shared key Secret must be mounted into the pod'
  assert_contains "$rendered" 'path: secret\.key$' 'the shared key must be projected as secret.key'
  assert_contains "$rendered" 'defaultMode: (0444|292)$' 'the key file must be read-only and readable by the nonroot uid'
  assert_not_contains "$rendered" 'LO_SECURITY_SECRET_KEY' 'the platform key must not be exposed through environment variables'
done

# Chart-generated key: one Secret shared by the whole Deployment, kept across
# upgrade/uninstall, encoded the way internal/secret expects (32 random bytes,
# unpadded standard base64, no trailing newline).
assert_contains "$WORK/standalone.yaml" 'secretName: test-lazy-observe-encryption-key$' 'replicas must mount the chart-managed key Secret'
assert_contains "$WORK/standalone.yaml" 'helm\.sh/resource-policy: keep' 'the generated key Secret must survive helm uninstall'
generated=$(awk '/^kind: Secret$/ { in_secret = 1 } /^---/ { in_secret = 0 } in_secret && /^  secret\.key: / { gsub(/"/, "", $2); print $2 }' "$WORK/standalone.yaml")
if [ -z "$generated" ]; then
  echo "FAIL: chart-generated key Secret must carry data.secret.key" >&2
  exit 1
fi
raw_key=$(printf '%s' "$generated" | base64 -d)
if ! printf '%s' "$raw_key" | grep -Eq '^[A-Za-z0-9+/]{43}$'; then
  echo "FAIL: generated key file must be 43 chars of unpadded base64, got '${raw_key}'" >&2
  exit 1
fi
if [ "$(printf '%s=' "$raw_key" | base64 -d | wc -c | tr -d ' ')" != 32 ]; then
  echo "FAIL: generated key must decode to 32 bytes" >&2
  exit 1
fi
mounted_secrets=$(grep -c 'secretName: test-lazy-observe-encryption-key$' "$WORK/standalone.yaml")
if [ "$mounted_secrets" != 1 ]; then
  echo "FAIL: the Deployment pod template must reference exactly one key Secret, got $mounted_secrets" >&2
  exit 1
fi

assert_contains "$WORK/key-existing.yaml" 'secretName: lo-platform-key$' 'existingSecret must be mounted as the key source'
assert_contains "$WORK/key-existing.yaml" 'key: platform\.key$' 'existingSecret key name must be configurable'
assert_not_contains "$WORK/key-existing.yaml" 'test-lazy-observe-encryption-key' 'existingSecret must suppress the chart-generated key Secret'

# allinone keeps its key on the PVC (data_dir) unless the data dir is ephemeral.
assert_not_contains "$WORK/allinone.yaml" 'secret_key_file' 'allinone with a PVC keeps the key in data_dir'
assert_not_contains "$WORK/allinone.yaml" 'test-lazy-observe-encryption-key' 'allinone with a PVC must not render a key Secret'
assert_contains "$WORK/allinone-ephemeral.yaml" 'secretName: test-lazy-observe-encryption-key$' 'allinone without a PVC must use the shared key Secret'

assert_render_fails 'multi-replica without a key source must fail rendering' 'encryptionKey' \
  --set encryptionKey.autoGenerate=false
assert_render_fails 'HPA without a key source must fail rendering' 'encryptionKey' \
  --set replicaCount=1 --set hpa.enabled=true --set encryptionKey.autoGenerate=false
"$HELM" template test "$CHART" --set replicaCount=1 --set encryptionKey.autoGenerate=false >"$WORK/single-no-key.yaml"
assert_not_contains "$WORK/single-no-key.yaml" 'secret_key_file' 'a single replica without a key source falls back to data_dir'

echo "PASS: Helm templates satisfy standalone-ha, clustered HA, and allinone assertions"
