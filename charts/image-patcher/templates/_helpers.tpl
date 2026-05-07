{{/*
Expand the name of the chart.
*/}}
{{- define "image-patcher.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. Truncated to 63 chars per Kubernetes DNS spec.
*/}}
{{- define "image-patcher.fullname" -}}
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
Chart name + version label.
*/}}
{{- define "image-patcher.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "image-patcher.labels" -}}
helm.sh/chart: {{ include "image-patcher.chart" . }}
{{ include "image-patcher.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels. control-plane is preserved for compatibility with the
metrics Service selector and ServiceMonitor matchers from the kustomize layout.
*/}}
{{- define "image-patcher.selectorLabels" -}}
app.kubernetes.io/name: {{ include "image-patcher.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "image-patcher.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-controller-manager" (include "image-patcher.fullname" .)) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Controller image reference.
*/}}
{{- define "image-patcher.controllerImage" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- if .Values.image.registry -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.repository $tag -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end }}

{{/*
Kaniko executor image reference.
*/}}
{{- define "image-patcher.kanikoImage" -}}
{{- if .Values.kaniko.image.registry -}}
{{- printf "%s/%s:%s" .Values.kaniko.image.registry .Values.kaniko.image.repository .Values.kaniko.image.tag -}}
{{- else -}}
{{- printf "%s:%s" .Values.kaniko.image.repository .Values.kaniko.image.tag -}}
{{- end -}}
{{- end }}
