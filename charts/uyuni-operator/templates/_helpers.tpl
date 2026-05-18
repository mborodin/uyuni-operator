{{/*
Expand the name of the chart.
*/}}
{{- define "uyuni-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
Truncated at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
*/}}
{{- define "uyuni-operator.fullname" -}}
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

{{/*
Create chart label value (name-version), used in helm.sh/chart label.
*/}}
{{- define "uyuni-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to all resources.
*/}}
{{- define "uyuni-operator.labels" -}}
helm.sh/chart: {{ include "uyuni-operator.chart" . }}
{{ include "uyuni-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels used in Deployment matchLabels and Service selector.
*/}}
{{- define "uyuni-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "uyuni-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "uyuni-operator.serviceAccountName" -}}
{{- include "uyuni-operator.fullname" . }}
{{- end }}

{{/*
Compute the full image reference: repository[:name]:tag
Values:
  image.repository  — registry + optional path prefix (e.g. ghcr.io/mborodin/uyuni-operator)
  image.name        — optional override of the image name portion only
  image.tag         — explicit tag; falls back to .Chart.AppVersion
*/}}
{{- define "uyuni-operator.image" -}}
{{- $repo := .Values.image.repository -}}
{{- $name := .Values.image.name -}}
{{- $tag  := .Values.image.tag | default .Chart.AppVersion -}}
{{- if $name -}}
{{- /* Strip trailing slash from repo, prepend name */}}
{{- printf "%s/%s:%s" ($repo | trimSuffix "/") $name $tag -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end }}
{{- end }}

{{/*
Webhook certificate secret name (used by cert-manager and the Deployment volume).
*/}}
{{- define "uyuni-operator.webhookCertSecret" -}}
{{- include "uyuni-operator.fullname" . }}-webhook-server-cert
{{- end }}

{{/*
Webhook service name.
*/}}
{{- define "uyuni-operator.webhookServiceName" -}}
{{- include "uyuni-operator.fullname" . }}-webhook-service
{{- end }}

{{/*
cert-manager inject-ca-from annotation value: "<namespace>/<certificate-name>"
*/}}
{{- define "uyuni-operator.certManagerCertRef" -}}
{{- printf "%s/%s" .Release.Namespace (include "uyuni-operator.fullname" .) }}-serving-cert
{{- end }}
