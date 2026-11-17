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

