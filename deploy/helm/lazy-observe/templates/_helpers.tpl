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
  datadir   - server default <data_dir>/secret.key (allinone with a PVC, or a
              single standalone replica that explicitly opted out of a Secret).
standalone-ha with more than one replica (or HPA) and no Secret fails rendering:
each pod would otherwise generate its own key in its emptyDir data_dir.
*/ -}}
{{- define "lazy-observe.encryptionKey.source" -}}
{{- $scaled := and .Values.hpa.enabled (gt (int .Values.hpa.maxReplicas) 1) -}}
{{- $multi := and (eq .Values.mode "standalone-ha") (or (gt (int .Values.replicaCount) 1) $scaled) -}}
{{- if .Values.encryptionKey.existingSecret -}}
existing
{{- else if and (eq .Values.mode "allinone") .Values.persistence.enabled -}}
datadir
{{- else if .Values.encryptionKey.autoGenerate -}}
generated
{{- else if $multi -}}
{{- fail "encryptionKey: standalone-ha with more than one replica (replicaCount > 1 or hpa.enabled) needs one shared platform encryption key; set encryptionKey.existingSecret to a Secret you manage, or set encryptionKey.autoGenerate=true" -}}
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

