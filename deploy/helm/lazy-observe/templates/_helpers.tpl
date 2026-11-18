{{- define "lazy-observe.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- define "lazy-observe.fullname" -}}
{{- if .Values.fullnameOverride }}{{ .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-%s" .Release.Name (include "lazy-observe.name" .) | trunc 63 | trimSuffix "-" }}{{ end }}
{{- end }}
{{- define "lazy-observe.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: {{ include "lazy-observe.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
{{- define "lazy-observe.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lazy-observe.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
{{- define "lazy-observe.secretName" -}}
{{- default (include "lazy-observe.fullname" .) .Values.secret.existingSecret }}
{{- end }}
{{- /*
Where the platform encryption key (security.secret_key_file) comes from:
  existing  - encryptionKey.existingSecret, mounted read-only;
  generated - chart-managed <fullname>-encryption-key Secret, mounted read-only;
  datadir   - server default <data_dir>/secret.key.
standalone-ha always fails rendering when no Secret provides the key, even with
replicaCount 1: its /data is an emptyDir while metadata lives in an external
database, so a generated key dies with the pod and the next pod is refused
because the key no longer matches the fingerprint recorded in metadata.
allinone with a PVC keeps the key in data_dir because the database lives in the
same volume; allinone without a PVC (allinone-ephemeral) loses the key and the
database together, so it cannot produce that mismatch.
*/ -}}
{{- define "lazy-observe.encryptionKey.source" -}}
{{- if .Values.encryptionKey.existingSecret -}}
existing
{{- else if and (eq .Values.mode "allinone") .Values.persistence.enabled -}}
datadir
{{- else if .Values.encryptionKey.autoGenerate -}}
generated
{{- else if eq .Values.mode "standalone-ha" -}}
{{- fail "encryptionKey: standalone-ha keeps metadata outside the pod, so the platform encryption key has to outlive pods; a key generated in the per-pod emptyDir data_dir is lost when the pod is recreated and the server then refuses to start because the key no longer matches the fingerprint recorded in metadata; set encryptionKey.existingSecret to a Secret you manage, or set encryptionKey.autoGenerate=true" -}}
{{- else -}}
datadir
{{- end -}}
{{- end }}
{{- define "lazy-observe.encryptionKey.mounted" -}}
{{- if ne (include "lazy-observe.encryptionKey.source" .) "datadir" }}true{{ end -}}
{{- end }}
{{- define "lazy-observe.encryptionKey.secretName" -}}
{{- default (printf "%s-encryption-key" (include "lazy-observe.fullname" .)) .Values.encryptionKey.existingSecret }}
{{- end }}
{{- define "lazy-observe.encryptionKey.mountPath" -}}
/etc/lazy-observe-key
{{- end }}
{{- /*
Where the first admin password (security.admin_password_file) comes from:
  existing  - adminPassword.existingSecret, mounted read-only;
  generated - chart-managed <fullname>-admin-password Secret, mounted read-only;
  datadir   - server generates <data_dir>/initial-admin-password (0600) and
              forces a change on first login (only when autoGenerate=false and
              a single replica runs).
Unlike the encryption key, allinone also defaults to a Secret: a person has to
read this value, and the distroless image has no shell to `cat` a file in the
pod. standalone-ha with more than one replica (or HPA) and no Secret fails
rendering: the generated file would only exist in the bootstrap winner's
emptyDir.
*/ -}}
{{- define "lazy-observe.adminPassword.source" -}}
{{- $scaled := and .Values.hpa.enabled (gt (int .Values.hpa.maxReplicas) 1) -}}
{{- $multi := and (eq .Values.mode "standalone-ha") (or (gt (int .Values.replicaCount) 1) $scaled) -}}
{{- if .Values.adminPassword.existingSecret -}}
existing
{{- else if .Values.adminPassword.autoGenerate -}}
generated
{{- else if $multi -}}
{{- fail "adminPassword: standalone-ha with more than one replica (replicaCount > 1 or hpa.enabled) needs one shared first admin password; set adminPassword.existingSecret to a Secret you manage, or set adminPassword.autoGenerate=true" -}}
{{- else -}}
datadir
{{- end -}}
{{- end }}
{{- define "lazy-observe.adminPassword.mounted" -}}
{{- if ne (include "lazy-observe.adminPassword.source" .) "datadir" }}true{{ end -}}
{{- end }}
{{- define "lazy-observe.adminPassword.secretName" -}}
{{- default (printf "%s-admin-password" (include "lazy-observe.fullname" .)) .Values.adminPassword.existingSecret }}
{{- end }}
{{- define "lazy-observe.adminPassword.mountPath" -}}
/etc/lazy-observe-admin
{{- end }}

