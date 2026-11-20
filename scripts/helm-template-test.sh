#!/bin/sh
set -eu

# Helm chart 的安全、可用性与供应链断言：
#   - 平台密钥与首个管理员密码只经挂载的 Secret 文件交给进程，不进环境变量、不用 envFrom；
#   - standalone-ha 的平台密钥必须有活过 pod 的来源，chart 生成的密钥 Secret 不随 uninstall 删除；
#   - 工作负载默认满足 PSS restricted（非 root、fsGroup: 65532、只读根文件系统、drop ALL、seccompProfile、
#     关闭 automountServiceAccountToken、emptyDir sizeLimit 与 ephemeral-storage 上限）；
#   - PodDisruptionBudget 仅在 standalone-ha 且有效副本数大于 minAvailable 时渲染，防止单副本锁死 drain；
#   - HPA 启用时省略 Deployment spec.replicas；chart 自建 Secret 时注入 checksum/secret；
#   - 镜像引用里的 digest 决定实际运行的字节，格式错误必须渲染失败，不能悄悄退回可变 tag；
#   - 发版附带的 values 片段与 chart 对得上，且拒绝畸形 digest。

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
CHART="$ROOT/deploy/helm/lazy-observe"
HELM=${HELM:-helm}

if ! command -v "$HELM" >/dev/null 2>&1; then
  echo "helm not found; install Helm (see mise.toml) or set HELM=/path/to/helm" >&2
  exit 1
fi

TMPDIR_ROOT=${TMPDIR:-/tmp}
WORK=$(mktemp -d "$TMPDIR_ROOT/lazy-observe-helm.XXXXXX")
trap 'rm -rf "$WORK"' EXIT HUP INT TERM

assert_contains() {
  file=$1
  pattern=$2
  message=$3
  # `--` so a pattern may start with a dash.
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

"$HELM" template test "$CHART" --set secret.create=true --set hpa.enabled=true >"$WORK/standalone.yaml"
"$HELM" template test "$CHART" --set secret.create=true --set hpa.enabled=false >"$WORK/standalone-no-hpa.yaml"
"$HELM" template test "$CHART" --set mode=allinone >"$WORK/allinone.yaml"
"$HELM" template test "$CHART" -f "$CHART/values-standalone-ha-cluster.yaml" --set secret.create=true >"$WORK/cluster.yaml"
"$HELM" template test "$CHART" --set mode=allinone --set persistence.enabled=false >"$WORK/allinone-ephemeral.yaml"
"$HELM" template test "$CHART" --set encryptionKey.existingSecret=lo-platform-key \
  --set encryptionKey.key=platform.key >"$WORK/key-existing.yaml"
"$HELM" template test "$CHART" --set adminPassword.existingSecret=lo-admin \
  --set adminPassword.key=password >"$WORK/admin-existing.yaml"
"$HELM" template test "$CHART" --set replicaCount=1 >"$WORK/single-replica.yaml"
"$HELM" template test "$CHART" --set hpa.enabled=true --set hpa.minReplicas=1 >"$WORK/hpa-min1.yaml"
"$HELM" template test "$CHART" --set replicaCount=2 --set podDisruptionBudget.minAvailable=2 >"$WORK/pdb-equal.yaml"
"$HELM" template test "$CHART" --set security.sessionSecureCookie=true >"$WORK/secure-cookie.yaml"
"$HELM" template test "$CHART" --set external.metrics.kind=prometheus --set external.metrics.cluster=false >"$WORK/prometheus.yaml"
"$HELM" template test "$CHART" --set mode=allinone \
  --set meta.migrationLockTimeout=90s \
  --set process.startupTimeout=30s \
  --set process.shutdownTimeout=20s \
  --set process.drainTimeout=25s >"$WORK/allinone-custom-timeouts.yaml"
"$HELM" template test "$CHART" --set podAntiAffinity=hard \
  --set 'topologySpreadConstraints[0].maxSkew=1' \
  --set 'topologySpreadConstraints[0].topologyKey=kubernetes.io/hostname' \
  --set 'topologySpreadConstraints[0].whenUnsatisfiable=DoNotSchedule' >"$WORK/hard-anti-affinity.yaml"

extract_kind() {
  kind=$1
  src=$2
  dst=$3
  awk -v target="kind: $kind" '
    /^---/ { if (hit) printf "%s", doc; doc = ""; hit = 0 }
    { doc = doc $0 "\n" }
    $0 == target { hit = 1 }
    END { if (hit) printf "%s", doc }
  ' "$src" >"$dst"
}

# 密钥与密码：只走挂载的只读文件。变量名按完整 token 匹配（后面不能再紧跟标识符字符），
# 不会误中合法的 LO_SECURITY_*_FILE（值是文件路径）。
for rendered in "$WORK/standalone.yaml" "$WORK/cluster.yaml" "$WORK/key-existing.yaml" "$WORK/allinone-ephemeral.yaml"; do
  assert_contains "$rendered" 'defaultMode: (0444|292)$' 'the key file must be read-only and readable by the nonroot uid'
  assert_not_contains "$rendered" 'LO_SECURITY_SECRET_KEY([^A-Za-z0-9_]|$)' \
    'the platform key must not be exposed through environment variables'
done
for rendered in "$WORK/standalone.yaml" "$WORK/cluster.yaml" "$WORK/admin-existing.yaml" "$WORK/allinone.yaml" \
  "$WORK/allinone-ephemeral.yaml"; do
  assert_not_contains "$rendered" 'LO_SECURITY_ADMIN_PASSWORD([^A-Za-z0-9_]|$)' \
    'the admin password must not be passed through environment variables'
done

# 平台密钥不能悄悄落进每个 pod 自己的 emptyDir：standalone-ha 的元数据在外部数据库，pod 重建后密钥没了，
# 已加密的凭据再也解不开。没有密钥来源必须渲染失败；chart 生成的密钥 Secret 必须扛过 helm uninstall。
assert_render_fails 'standalone-ha without a key source must fail rendering' 'encryptionKey' \
  --set replicaCount=1 --set encryptionKey.autoGenerate=false
awk '/^---/ { if (hit) printf "%s", doc; doc = ""; hit = 0 }
  { doc = doc $0 "\n" }
  /^  name: test-lazy-observe-encryption-key$/ { hit = 1 }
  END { if (hit) printf "%s", doc }' "$WORK/standalone.yaml" >"$WORK/key-secret.yaml"
assert_contains "$WORK/key-secret.yaml" '^kind: Secret$' 'the chart must generate the platform key Secret'
assert_contains "$WORK/key-secret.yaml" 'helm\.sh/resource-policy: keep' \
  'the generated key Secret must survive helm uninstall'

# envFrom 能把密钥整包注入，envFrom.prefix 还能在运行时拼出变量名，按名字匹配的断言拦不住；
# chart 只要开始用 envFrom，就必须先重新审议密钥防护。
for rendered in "$WORK/standalone.yaml" "$WORK/allinone.yaml" "$WORK/cluster.yaml" \
  "$WORK/key-existing.yaml" "$WORK/allinone-ephemeral.yaml" "$WORK/admin-existing.yaml"; do
  assert_not_contains "$rendered" '^[[:space:]]*envFrom:' \
    'the chart must not inject key material with envFrom (rethink the key guards first)'
done

# Pod Security Standards (restricted)、PVC fsGroup: 65532 与临时卷上限。
for rendered in "$WORK/standalone.yaml" "$WORK/allinone.yaml" "$WORK/cluster.yaml"; do
  extract_kind Deployment "$rendered" "$WORK/deploy.yaml"
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*automountServiceAccountToken: false$' \
    'Deployment must disable automountServiceAccountToken by default'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*runAsNonRoot: true$' \
    'Deployment must enforce runAsNonRoot'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*fsGroup: 65532$' \
    'Deployment must set fsGroup: 65532 to match the distroless nonroot user for PVC writes'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*type: RuntimeDefault$' \
    'Deployment must set seccompProfile.type: RuntimeDefault'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*allowPrivilegeEscalation: false$' \
    'container must disable privilege escalation'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*readOnlyRootFilesystem: true$' \
    'container must run with readOnlyRootFilesystem: true'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*- ALL$' \
    'container must drop ALL capabilities'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*sizeLimit: ' \
    'emptyDir volumes must declare sizeLimit'
  assert_contains "$WORK/deploy.yaml" '^[[:space:]]*ephemeral-storage: ' \
    'container resources must bound ephemeral-storage'
done

# PodDisruptionBudget 守卫：仅在 standalone-ha 且有效副本数大于 minAvailable 时渲染。
for rendered in "$WORK/standalone.yaml" "$WORK/standalone-no-hpa.yaml" "$WORK/cluster.yaml"; do
  assert_contains "$rendered" '^kind: PodDisruptionBudget$' \
    "multi-replica standalone-ha ($rendered) must render PodDisruptionBudget"
done
for rendered in "$WORK/allinone.yaml" "$WORK/single-replica.yaml" "$WORK/hpa-min1.yaml" "$WORK/pdb-equal.yaml"; do
  assert_not_contains "$rendered" '^kind: PodDisruptionBudget$' \
    "single-replica or minAvailable>=replicas ($rendered) must not render PodDisruptionBudget"
done

# HPA 启用时省略 Deployment spec.replicas，防止 helm upgrade 打回 HPA 扩缩容副本数；未启用时保留。
extract_kind Deployment "$WORK/standalone.yaml" "$WORK/deploy-hpa.yaml"
assert_not_contains "$WORK/deploy-hpa.yaml" '^  replicas:' \
  'Deployment must omit spec.replicas when hpa.enabled=true'
extract_kind Deployment "$WORK/standalone-no-hpa.yaml" "$WORK/deploy-no-hpa.yaml"
assert_contains "$WORK/deploy-no-hpa.yaml" '^  replicas: 2$' \
  'Deployment must render spec.replicas when hpa.enabled=false'
extract_kind Deployment "$WORK/allinone.yaml" "$WORK/deploy-allinone.yaml"
assert_contains "$WORK/deploy-allinone.yaml" '^  replicas: 1$' \
  'allinone Deployment must pin spec.replicas to 1'

# chart 自建 Secret 时注入 checksum/secret，轮换值能触发滚动重启；使用 existingSecret 时不注入。
assert_contains "$WORK/deploy-hpa.yaml" '^[[:space:]]*checksum/secret: [0-9a-f]{64}$' \
  'Deployment must include checksum/secret when secret.create=true'
extract_kind Deployment "$WORK/key-existing.yaml" "$WORK/deploy-no-secret.yaml"
assert_not_contains "$WORK/deploy-no-secret.yaml" '^[[:space:]]*checksum/secret:' \
  'Deployment must not include checksum/secret when secret.create=false'

# security.session_secure_cookie 透传到 ConfigMap。
assert_contains "$WORK/standalone.yaml" '^[[:space:]]*session_secure_cookie: false$' \
  'default ConfigMap must render session_secure_cookie: false'
assert_contains "$WORK/secure-cookie.yaml" '^[[:space:]]*session_secure_cookie: true$' \
  'security.sessionSecureCookie=true must render session_secure_cookie: true'

# 集群 profile 与显式配置的 podAntiAffinity / topologySpreadConstraints 按 release 的 selectorLabels 隔离。
extract_kind Deployment "$WORK/cluster.yaml" "$WORK/deploy-cluster.yaml"
assert_contains "$WORK/deploy-cluster.yaml" '^[[:space:]]*podAntiAffinity:' \
  'cluster profile must render podAntiAffinity'
assert_contains "$WORK/deploy-cluster.yaml" '^[[:space:]]*topologySpreadConstraints:' \
  'cluster profile must render topologySpreadConstraints'
assert_contains "$WORK/deploy-cluster.yaml" '^[[:space:]]*topologyKey: "topology\.kubernetes\.io/zone"$' \
  'cluster profile topologySpreadConstraints must spread across zones'
assert_contains "$WORK/deploy-cluster.yaml" '^[[:space:]]*app\.kubernetes\.io/instance: test$' \
  'cluster profile affinity and topologySpreadConstraints must scope by app.kubernetes.io/instance'
extract_kind Deployment "$WORK/hard-anti-affinity.yaml" "$WORK/deploy-hard.yaml"
assert_contains "$WORK/deploy-hard.yaml" '^[[:space:]]*requiredDuringSchedulingIgnoredDuringExecution:' \
  'podAntiAffinity=hard must render requiredDuringSchedulingIgnoredDuringExecution'
assert_contains "$WORK/deploy-hard.yaml" '^[[:space:]]*topologySpreadConstraints:' \
  'explicit topologySpreadConstraints must render in Deployment'
assert_contains "$WORK/deploy-hard.yaml" '^[[:space:]]*app\.kubernetes\.io/name: lazy-observe$' \
  'hard anti-affinity / topologySpreadConstraints must bind app.kubernetes.io/name'
assert_contains "$WORK/deploy-hard.yaml" '^[[:space:]]*app\.kubernetes\.io/instance: test$' \
  'hard anti-affinity / topologySpreadConstraints must bind app.kubernetes.io/instance'

# 镜像引用：digest 决定实际运行的字节；格式错误必须失败，不能悄悄退回可变 tag。
IMAGE_REPO='ghcr\.io/eimsound/lazy-observe'
DIGEST=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
assert_contains "$WORK/standalone.yaml" '^[[:space:]]*imagePullPolicy: Always$' \
  'mutable :latest tag without digest must default imagePullPolicy to Always'
"$HELM" template test "$CHART" --set image.digest="$DIGEST" >"$WORK/image-digest.yaml"
assert_contains "$WORK/image-digest.yaml" "image: \"${IMAGE_REPO}@$DIGEST\"$" \
  'image.digest alone must render repository@digest'
assert_contains "$WORK/image-digest.yaml" '^[[:space:]]*imagePullPolicy: IfNotPresent$' \
  'digest-pinned image must default imagePullPolicy to IfNotPresent'
assert_not_contains "$WORK/image-digest.yaml" 'lazy-observe:latest' \
  'a digest-only reference must not keep the mutable default tag'
"$HELM" template test "$CHART" --set image.tag=1.2.3 --set image.digest="$DIGEST" \
  >"$WORK/image-tag-digest.yaml"
assert_contains "$WORK/image-tag-digest.yaml" "image: \"${IMAGE_REPO}:1\\.2\\.3@$DIGEST\"$" \
  'image.tag plus image.digest must render repository:tag@digest'
assert_contains "$WORK/image-tag-digest.yaml" '^[[:space:]]*imagePullPolicy: IfNotPresent$' \
  'version-pinned image must default imagePullPolicy to IfNotPresent'

# values.schema.json 静态校验：非法 digest、模式、枚举、副本数与未知字段必须在渲染前被拦截。
assert_render_fails 'schema must reject a truncated digest' '/image/digest' \
  --set image.digest=sha256:abc
assert_render_fails 'schema must reject an uppercase digest' '/image/digest' \
  --set image.digest=sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef
assert_render_fails 'schema must reject a non-sha256 digest' '/image/digest' \
  --set image.digest=sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
assert_render_fails 'schema must reject an unknown mode' '/mode' \
  --set mode=invalid-mode
assert_render_fails 'schema must reject replicaCount=0' '/replicaCount' \
  --set replicaCount=0
assert_render_fails 'schema must reject unknown metrics kind' '/external/metrics/kind' \
  --set external.metrics.kind=influxdb
assert_render_fails 'schema must reject invalid podAntiAffinity' '/podAntiAffinity' \
  --set podAntiAffinity=strict
assert_render_fails 'schema must reject unknown top-level keys' 'unknownKey' \
  --set unknownKey=true

# 即使跳过 schema 校验，模板层自身仍必须拒绝畸形 digest（纵深防御）。
assert_render_fails 'template must reject a truncated digest even with --skip-schema-validation' \
  'image.digest must be "sha256:"' --skip-schema-validation --set image.digest=sha256:abc
assert_render_fails 'template must reject an uppercase digest even with --skip-schema-validation' \
  'image.digest must be "sha256:"' --skip-schema-validation \
  --set image.digest=sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef
assert_render_fails 'template must reject a non-sha256 digest even with --skip-schema-validation' \
  'image.digest must be "sha256:"' --skip-schema-validation \
  --set image.digest=sha512:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

# 发版附带的 values 片段：用假输入渲染再喂回 chart，生成器与模板对不上就会在这里暴露；
# 畸形 digest 不得产出任何输出。
RENDER_VALUES="$ROOT/scripts/render-helm-release-values.sh"
bash "$RENDER_VALUES" --tag 1.2.3 --digest "$DIGEST" --output "$WORK/helm-values.yaml"
"$HELM" template test "$CHART" -f "$WORK/helm-values.yaml" >"$WORK/image-release-values.yaml"
assert_contains "$WORK/image-release-values.yaml" "image: \"${IMAGE_REPO}:1\\.2\\.3@$DIGEST\"$" \
  'the release values fragment must render the published digest'
if bash "$RENDER_VALUES" --tag 1.2.3 --digest sha256:abc >"$WORK/render-fail.yaml" \
  2>"$WORK/render-fail.err"; then
  echo "FAIL: the release values renderer must reject a malformed digest" >&2
  exit 1
fi
if [ -s "$WORK/render-fail.yaml" ]; then
  echo "FAIL: the release values renderer must not emit output for a malformed digest" >&2
  exit 1
fi

# Helm ConfigMap <-> Go internal/config.Load 双向契约校验：
# 所有组合渲染出的 ConfigMap config.yaml 必须通过 KnownFields(true) + Validate()，
# 且 Deployment 注入的 LO_* 环境变量必须与 internal/config 绑定一致。
(
  cd "$ROOT"
  CGO_ENABLED=0 go run ./scripts/check-helm-config \
    "$WORK/standalone.yaml" \
    "$WORK/standalone-no-hpa.yaml" \
    "$WORK/allinone.yaml" \
    "$WORK/cluster.yaml" \
    "$WORK/allinone-ephemeral.yaml" \
    "$WORK/key-existing.yaml" \
    "$WORK/admin-existing.yaml" \
    "$WORK/single-replica.yaml" \
    "$WORK/hpa-min1.yaml" \
    "$WORK/pdb-equal.yaml" \
    "$WORK/secure-cookie.yaml" \
    "$WORK/prometheus.yaml" \
    "$WORK/allinone-custom-timeouts.yaml" \
    "$WORK/hard-anti-affinity.yaml" \
    "$WORK/image-digest.yaml" \
    "$WORK/image-tag-digest.yaml" \
    "$WORK/image-release-values.yaml"
)

echo "PASS: Helm chart satisfies values.schema.json, security guards, image.digest, and Go config.Load contracts"
