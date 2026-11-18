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
  # `--` so a pattern may start with a dash (e.g. the flag name it asserts on).
  if ! grep -Eq -- "$pattern" "$file"; then
    echo "FAIL: $message" >&2
    exit 1
  fi
}

assert_not_contains() {
  file=$1
  pattern=$2
  message=$3
  if grep -Eq -- "$pattern" "$file"; then
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
  # 把变量名当完整 token 匹配：后面不能再紧跟标识符字符，所以不会误中合法的
  # LO_SECURITY_SECRET_KEY_FILE（值是文件路径），同时能拦住单引号、flow 风格、行尾注释、
  # JSON、data + envFrom 等所有会把密钥名字当成整体词出现的写法。
  assert_not_contains "$rendered" 'LO_SECURITY_SECRET_KEY([^A-Za-z0-9_]|$)' \
    'the platform key must not be exposed through environment variables'
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
# Even a single replica must not fall back to data_dir: standalone-ha runs its
# metadata in an external database while /data is an emptyDir, so a key that
# lives only in the pod is lost on recreation and the next pod is refused.
assert_render_fails 'a single standalone-ha replica without a key source must fail rendering' \
  'set encryptionKey.existingSecret' --set replicaCount=1 --set encryptionKey.autoGenerate=false
assert_render_fails 'the standalone-ha key error must name the fingerprint consequence' \
  'no longer matches the fingerprint recorded in metadata' \
  --set replicaCount=1 --set encryptionKey.autoGenerate=false

# First admin password: replicas must read one Secret through
# security.admin_password_file. A generated password would otherwise land only
# in the bootstrap winner's emptyDir data_dir and be lost with that pod.
ADMIN_PATH=/etc/lazy-observe-admin/admin-password
"$HELM" template test "$CHART" --set adminPassword.existingSecret=lo-admin \
  --set adminPassword.key=password >"$WORK/admin-existing.yaml"
for rendered in "$WORK/standalone.yaml" "$WORK/cluster.yaml" "$WORK/admin-existing.yaml" "$WORK/allinone.yaml" \
  "$WORK/allinone-ephemeral.yaml"; do
  assert_contains "$rendered" "admin_password_file: \"$ADMIN_PATH\"" 'admin_password_file must point at the mounted admin password'
  assert_contains "$rendered" 'mountPath: /etc/lazy-observe-admin$' 'the admin password Secret must be mounted into the pod'
  assert_contains "$rendered" 'path: admin-password$' 'the admin password must be projected as admin-password'
  # 同上的 token 边界匹配：合法的 LO_SECURITY_ADMIN_PASSWORD_FILE 不再误报。
  assert_not_contains "$rendered" 'LO_SECURITY_ADMIN_PASSWORD([^A-Za-z0-9_]|$)' \
    'the admin password must not be passed through environment variables'
done

assert_contains "$WORK/standalone.yaml" 'secretName: test-lazy-observe-admin-password$' 'replicas must mount the chart-managed admin password Secret'
admin_generated=$(awk '/^kind: Secret$/ { in_secret = 1 } /^---/ { in_secret = 0 } in_secret && /^  admin-password: / { gsub(/"/, "", $2); print $2 }' "$WORK/standalone.yaml")
if [ -z "$admin_generated" ]; then
  echo "FAIL: chart-generated admin password Secret must carry data.admin-password" >&2
  exit 1
fi
if ! printf '%s' "$admin_generated" | base64 -d | grep -Eq '^[A-Za-z0-9]{32}$'; then
  echo "FAIL: generated admin password must be 32 alphanumeric characters without a newline" >&2
  exit 1
fi
admin_secret=$(awk '/^kind: Secret$/ { in_secret = 1 } /^---/ { in_secret = 0 } in_secret' "$WORK/standalone.yaml" | grep -c 'admin-password:' || true)
[ "$admin_secret" = 1 ] || { echo "FAIL: expected exactly one generated admin password Secret entry" >&2; exit 1; }

assert_contains "$WORK/admin-existing.yaml" 'secretName: lo-admin$' 'adminPassword.existingSecret must be mounted'
assert_contains "$WORK/admin-existing.yaml" 'key: password$' 'adminPassword.key must select the Secret key'
assert_not_contains "$WORK/admin-existing.yaml" 'test-lazy-observe-admin-password' 'existingSecret must suppress the chart-generated admin password Secret'

# allinone also defaults to a readable Secret: the distroless image has no shell
# to read <data_dir>/initial-admin-password from inside the pod.
assert_contains "$WORK/allinone.yaml" 'secretName: test-lazy-observe-admin-password$' 'allinone must mount the chart-managed admin password Secret'

assert_render_fails 'multi-replica without an admin password source must fail rendering' 'adminPassword' \
  --set adminPassword.autoGenerate=false
assert_render_fails 'HPA without an admin password source must fail rendering' 'adminPassword' \
  --set replicaCount=1 --set hpa.enabled=true --set adminPassword.autoGenerate=false
"$HELM" template test "$CHART" --set replicaCount=1 --set adminPassword.autoGenerate=false >"$WORK/single-no-admin.yaml"
assert_not_contains "$WORK/single-no-admin.yaml" 'admin_password_file' 'a single replica without an admin Secret falls back to data_dir'
"$HELM" template test "$CHART" --set mode=allinone --set adminPassword.autoGenerate=false >"$WORK/allinone-no-admin.yaml"
assert_not_contains "$WORK/allinone-no-admin.yaml" 'admin_password_file' 'allinone without an admin Secret falls back to data_dir'

# envFrom 是「把密钥本体当环境变量注入」最现实的一条路：密钥名字写在 ConfigMap/Secret 的
# data 里，上面的 token 匹配依赖容器 env 里出现这个名字，envFrom 能把它整包注入。
# 而 envFrom.prefix 还能在运行时拼出变量名，任何按名字匹配的断言都拦不住，所以只要 chart
# 开始用 envFrom 就必须停下来重新审议密钥防护。当前 chart 只在 deployment.yaml 用 env:。
for rendered in "$WORK/standalone.yaml" "$WORK/allinone.yaml" "$WORK/cluster.yaml" \
  "$WORK/key-existing.yaml" "$WORK/allinone-ephemeral.yaml" "$WORK/admin-existing.yaml" \
  "$WORK/single-no-admin.yaml" "$WORK/allinone-no-admin.yaml"; do
  assert_not_contains "$rendered" '^[[:space:]]*envFrom:' \
    'the chart must not inject key material with envFrom (rethink the key guards first)'
done

# NOTES must tell the operator how to read the password back. `helm template`
# never renders NOTES.txt and `helm install --dry-run` needs a cluster, so a
# scratch copy of the chart wraps NOTES in a define and emits it base64-encoded.
NOTES_CHART="$WORK/notes-chart/lazy-observe"
mkdir -p "$WORK/notes-chart"
cp -R "$CHART" "$NOTES_CHART"
{
  printf '{{- define "test.notes" -}}\n'
  cat "$NOTES_CHART/templates/NOTES.txt"
  printf '\n{{- end -}}\n'
} >"$NOTES_CHART/templates/_test_notes.tpl"
rm "$NOTES_CHART/templates/NOTES.txt"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test-notes\ndata:\n  notes: {{ include "test.notes" . | b64enc }}\n' \
  >"$NOTES_CHART/templates/test-notes.yaml"
render_notes() {
  output=$1
  shift
  "$HELM" template test "$NOTES_CHART" --show-only templates/test-notes.yaml "$@" |
    sed -n 's/^  notes: //p' | base64 -d >"$output"
}
render_notes "$WORK/notes-generated.txt"
assert_contains "$WORK/notes-generated.txt" \
  "kubectl get secret test-lazy-observe-admin-password -n default -o jsonpath='\\{\\.data\\.admin-password\\}' \\| base64 -d" \
  'NOTES must show how to read the generated admin password'
render_notes "$WORK/notes-existing.txt" --set adminPassword.existingSecret=lo-admin
assert_contains "$WORK/notes-existing.txt" 'key admin-password of your Secret lo-admin' 'NOTES must name the provided admin password Secret'
render_notes "$WORK/notes-datadir.txt" --set mode=allinone --set adminPassword.autoGenerate=false
assert_contains "$WORK/notes-datadir.txt" '/data/initial-admin-password' 'NOTES must show where the server writes the initial admin password'

# Container image reference. Without a digest the chart keeps the historical
# <repository>:<tag> form (the tag falls back to Chart.AppVersion). With a digest
# the digest decides which bytes run, and the tag - when also set - stays in the
# reference for readability, the same shape the pinned third-party images use in
# scripts/e2e-multi-replica.sh. A malformed digest must fail rendering instead of
# silently downgrading the deployment to a mutable tag.
IMAGE_REPO='ghcr\.io/eimsound/lazy-observe'
DIGEST=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
"$HELM" template test "$CHART" >"$WORK/image-default.yaml"
assert_contains "$WORK/image-default.yaml" "image: \"${IMAGE_REPO}:latest\"$" \
  'without image.tag or image.digest the chart falls back to Chart.AppVersion'
"$HELM" template test "$CHART" --set image.tag=1.2.3 >"$WORK/image-tag.yaml"
assert_contains "$WORK/image-tag.yaml" "image: \"${IMAGE_REPO}:1\\.2\\.3\"$" \
  'image.tag must be used while image.digest is empty'
"$HELM" template test "$CHART" --set image.digest="$DIGEST" >"$WORK/image-digest.yaml"
assert_contains "$WORK/image-digest.yaml" "image: \"${IMAGE_REPO}@$DIGEST\"$" \
  'image.digest alone must render repository@digest without inventing a tag'
assert_not_contains "$WORK/image-digest.yaml" 'lazy-observe:latest' \
  'a digest-only reference must not keep the mutable default tag'
"$HELM" template test "$CHART" --set image.tag=1.2.3 --set image.digest="$DIGEST" \
  >"$WORK/image-tag-digest.yaml"
assert_contains "$WORK/image-tag-digest.yaml" "image: \"${IMAGE_REPO}:1\\.2\\.3@$DIGEST\"$" \
  'image.tag plus image.digest must render repository:tag@digest'

# 数值型 tag：values 里写 `tag: 0.1` 会被 YAML 解析成 float，`--set image.tag=2` 会被解析成
# int64，直接 printf %s 会输出 Go 的格式错误串（%!s(float64=0.1)），渲染出 InvalidImageName
# 而 helm 不报错。release 本就发 {{major}}.{{minor}} 形式的 tag，所以 tag 必须先 toString。
cat >"$WORK/image-numeric-values.yaml" <<'YAML'
image:
  tag: 0.1
YAML
"$HELM" template test "$CHART" -f "$WORK/image-numeric-values.yaml" >"$WORK/image-numeric.yaml"
assert_contains "$WORK/image-numeric.yaml" "image: \"${IMAGE_REPO}:0\\.1\"$" \
  'a numeric YAML image.tag must render as a plain version string'
assert_not_contains "$WORK/image-numeric.yaml" '%!s' \
  'a numeric image.tag must not leak Go formatting verbs into the image reference'
"$HELM" template test "$CHART" --set image.tag=2 --set image.digest="$DIGEST" \
  >"$WORK/image-int-digest.yaml"
assert_contains "$WORK/image-int-digest.yaml" "image: \"${IMAGE_REPO}:2@$DIGEST\"$" \
  'an integer --set image.tag must render as a plain version string alongside a digest'
assert_not_contains "$WORK/image-int-digest.yaml" '%!s' \
  'an integer image.tag must not leak Go formatting verbs into the image reference'

assert_render_fails 'a truncated digest must fail rendering' 'image.digest must be "sha256:"' \
  --set image.digest=sha256:abc
assert_render_fails 'an uppercase digest must fail rendering' 'image.digest must be "sha256:"' \
  --set image.digest=sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef
assert_render_fails 'a non-sha256 digest must fail rendering' 'image.digest must be "sha256:"' \
  --set image.digest=sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

# The chart is not packaged or published: the release workflow renders the
# digest it just pushed into a values fragment and attaches it to the GitHub
# Release (helm-values-lazy-observe-<version>.yaml). Render that fragment with
# fake inputs and feed it back into the chart, so the renderer and the template
# cannot drift apart.
RENDER_VALUES="$ROOT/scripts/render-helm-release-values.sh"
if [ ! -x "$RENDER_VALUES" ]; then
  echo "FAIL: scripts/render-helm-release-values.sh must exist and be executable" >&2
  exit 1
fi
bash "$RENDER_VALUES" --tag 1.2.3 --digest "$DIGEST" --output "$WORK/helm-values.yaml"
assert_contains "$WORK/helm-values.yaml" '^  repository: "ghcr\.io/eimsound/lazy-observe"$' \
  'the release values fragment must name the default image repository'
assert_contains "$WORK/helm-values.yaml" '^  tag: "1\.2\.3"$' \
  'the release values fragment must carry the released version as image.tag'
assert_contains "$WORK/helm-values.yaml" "^  digest: \"$DIGEST\"$" \
  'the release values fragment must pin the pushed digest'
"$HELM" template test "$CHART" -f "$WORK/helm-values.yaml" >"$WORK/image-release-values.yaml"
assert_contains "$WORK/image-release-values.yaml" "image: \"${IMAGE_REPO}:1\\.2\\.3@$DIGEST\"$" \
  'the release values fragment must render the published digest'
if bash "$RENDER_VALUES" --tag 1.2.3 --digest sha256:abc >"$WORK/render-fail.yaml" \
  2>"$WORK/render-fail.err"; then
  echo "FAIL: the release values renderer must reject a malformed digest" >&2
  exit 1
fi
assert_contains "$WORK/render-fail.err" 'sha256:' 'the renderer error must name the expected digest shape'
if [ -s "$WORK/render-fail.yaml" ]; then
  echo "FAIL: the release values renderer must not emit output for a malformed digest" >&2
  exit 1
fi
# 仓库名会被拼进镜像引用，拼错（大写、多余的分隔符）在 Kubernetes 里只是 InvalidImageName，
# 所以在生成阶段就拦下来。
for bad_repository in 'ghcr.io/Eimsound/lazy-observe' '/lazy-observe' 'lazy-observe/'; do
  if bash "$RENDER_VALUES" --repository "$bad_repository" --tag 1.2.3 --digest "$DIGEST" \
    >"$WORK/render-repo.yaml" 2>"$WORK/render-repo.err"; then
    echo "FAIL: the release values renderer must reject repository '$bad_repository'" >&2
    exit 1
  fi
  assert_contains "$WORK/render-repo.err" '--repository must be a lowercase' \
    'the renderer error must name the rejected --repository value'
done

echo "PASS: Helm templates satisfy standalone-ha, clustered HA, and allinone assertions"
echo "PASS: Helm image references honour image.digest and the release values fragment"
