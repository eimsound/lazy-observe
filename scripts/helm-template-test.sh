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

echo "PASS: Helm templates satisfy standalone-ha, clustered HA, and allinone assertions"
