{{- define "gemaal.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "gemaal.labels" -}}
app.kubernetes.io/name: gemaal
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "gemaal.selectorLabels" -}}
app.kubernetes.io/name: gemaal
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "gemaal.serviceAccountName" -}}
{{- .Values.serviceAccount.name | default (include "gemaal.fullname" .) }}
{{- end }}
