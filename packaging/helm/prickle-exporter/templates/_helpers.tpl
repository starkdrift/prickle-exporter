{{/* SPDX-License-Identifier: Apache-2.0 */}}

{{- define "prickle.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prickle.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "prickle.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "prickle.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "prickle.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: prickle-exporter
{{- end -}}

{{- define "prickle.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prickle.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The image tag. Empty in values means the chart's appVersion, so the chart and
the binary it deploys move together; nvml.enabled appends the suffix that
selects the dynamically linked artifact.
*/}}
{{- define "prickle.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- if .Values.nvml.enabled -}}
{{- $tag = printf "%s%s" $tag .Values.nvml.tagSuffix -}}
{{- end -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
