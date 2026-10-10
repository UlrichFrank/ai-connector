{{/*
Expand the name of the chart.
*/}}
{{- define "ai-connector.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
A default fully qualified app name (63 characters at most, the DNS naming limit). If the release name contains
the chart name, it is used as is.
*/}}
{{- define "ai-connector.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "ai-connector.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "ai-connector.labels" -}}
helm.sh/chart: {{ include "ai-connector.chart" . }}
{{ include "ai-connector.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "ai-connector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ai-connector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The Secret with MLXLAB_PASSWORD (and MLXLAB_WORKER_TOKEN): existingSecret or the chart's own.
*/}}
{{- define "ai-connector.secretName" -}}
{{- if .Values.existingSecret }}
{{- .Values.existingSecret }}
{{- else }}
{{- printf "%s-secrets" (include "ai-connector.fullname" .) }}
{{- end }}
{{- end }}

{{/*
The host names of the ingress (console first), without empty ones.
*/}}
{{- define "ai-connector.hosts" -}}
{{- $hosts := list }}
{{- with .Values.hosts.console }}{{ $hosts = append $hosts . }}{{ end }}
{{- with .Values.hosts.app }}{{ if not (has . $hosts) }}{{ $hosts = append $hosts . }}{{ end }}{{ end }}
{{- toJson $hosts }}
{{- end }}
