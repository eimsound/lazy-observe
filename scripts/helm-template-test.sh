#!/bin/sh
set -eu

# Helm chart 的安全与供应链断言，别的不测：
#   - 平台密钥与首个管理员密码只经挂载的 Secret 文件交给进程，不进环境变量、不用 envFrom；
#   - standalone-ha 的平台密钥必须有活过 pod 的来源，chart 生成的密钥 Secret 不随 uninstall 删除；
#   - 镜像引用里的 digest 决定实际运行的字节，格式错误必须渲染失败，不能悄悄退回可变 tag；
#   - 发版附带的 values 片段与 chart 对得上，且拒绝畸形 digest。
# 副本数、探针、集群 URL、NOTES 文案、生成密钥的编码等实现细节不断言。

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
"$HELM" template test "$CHART" --set mode=allinone >"$WORK/allinone.yaml"
"$HELM" template test "$CHART" -f "$CHART/values-standalone-ha-cluster.yaml" --set secret.create=true >"$WORK/cluster.yaml"
"$HELM" template test "$CHART" --set mode=allinone --set persistence.enabled=false >"$WORK/allinone-ephemeral.yaml"
"$HELM" template test "$CHART" --set encryptionKey.existingSecret=lo-platform-key \
  --set encryptionKey.key=platform.key >"$WORK/key-existing.yaml"
"$HELM" template test "$CHART" --set adminPassword.existingSecret=lo-admin \
  --set adminPassword.key=password >"$WORK/admin-existing.yaml"

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

# 镜像引用：digest 决定实际运行的字节；格式错误必须失败，不能悄悄退回可变 tag。
IMAGE_REPO='ghcr\.io/eimsound/lazy-observe'
DIGEST=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
"$HELM" template test "$CHART" --set image.digest="$DIGEST" >"$WORK/image-digest.yaml"
assert_contains "$WORK/image-digest.yaml" "image: \"${IMAGE_REPO}@$DIGEST\"$" \
  'image.digest alone must render repository@digest'
assert_not_contains "$WORK/image-digest.yaml" 'lazy-observe:latest' \
  'a digest-only reference must not keep the mutable default tag'
"$HELM" template test "$CHART" --set image.tag=1.2.3 --set image.digest="$DIGEST" \
  >"$WORK/image-tag-digest.yaml"
assert_contains "$WORK/image-tag-digest.yaml" "image: \"${IMAGE_REPO}:1\\.2\\.3@$DIGEST\"$" \
  'image.tag plus image.digest must render repository:tag@digest'

assert_render_fails 'a truncated digest must fail rendering' 'image.digest must be "sha256:"' \
  --set image.digest=sha256:abc
assert_render_fails 'an uppercase digest must fail rendering' 'image.digest must be "sha256:"' \
  --set image.digest=sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef
assert_render_fails 'a non-sha256 digest must fail rendering' 'image.digest must be "sha256:"' \
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

echo "PASS: Helm chart keeps key material out of the environment and honours image.digest"
