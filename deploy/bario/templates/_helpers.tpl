{{- define "bario.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bario.fullname" -}}
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

{{- define "bario.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "bario.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "bario.selectorLabels" -}}
app.kubernetes.io/name: {{ include "bario.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "bario.serviceName" -}}
{{- printf "%s-%s" (include "bario.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Render an image reference, optionally prefixed by global.registry.
Expects a dict with "root" (the chart context) and "image" (the image values). */}}
{{- define "bario.image" -}}
{{- $registry := .root.Values.global.registry | default "" -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" (trimSuffix "/" $registry) .image.repository .image.tag -}}
{{- else -}}
{{- printf "%s:%s" .image.repository .image.tag -}}
{{- end -}}
{{- end -}}

{{- define "bario.databaseURL" -}}
postgres://{{ .Values.postgres.user }}:$(POSTGRES_PASSWORD)@{{ include "bario.fullname" . }}-postgres:5432/{{ .Values.postgres.database }}?sslmode=disable
{{- end -}}
