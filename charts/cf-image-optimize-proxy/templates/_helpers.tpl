{{/*
Expand the name of the chart.
*/}}
{{- define "cf-image-optimize-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "cf-image-optimize-proxy.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "cf-image-optimize-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "cf-image-optimize-proxy.labels" -}}
helm.sh/chart: {{ include "cf-image-optimize-proxy.chart" . }}
{{ include "cf-image-optimize-proxy.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "cf-image-optimize-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cf-image-optimize-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Create the name of the service account to use.
*/}}
{{- define "cf-image-optimize-proxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "cf-image-optimize-proxy.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Image reference.
*/}}
{{- define "cf-image-optimize-proxy.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Managed or existing secret name.
*/}}
{{- define "cf-image-optimize-proxy.secretName" -}}
{{- default (include "cf-image-optimize-proxy.fullname" .) .Values.existingSecret.name -}}
{{- end -}}

