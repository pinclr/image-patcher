{{/*
Expand the name of the chart.
*/}}
{{- define "image-patcher.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Enforce that the chart is installed in the image-patch-system namespace.
Resource names use a fixed prefix and ClusterRoleBinding subjects point
at this namespace, so installing elsewhere yields a half-broken release.
Fail fast at template/install time with an actionable message.
*/}}
{{- define "image-patcher.assertNamespace" -}}
{{- if ne .Release.Namespace "image-patch-system" -}}
{{- fail (printf "image-patcher must be installed in the 'image-patch-system' namespace, got %q. Pass: --namespace image-patch-system --create-namespace" .Release.Namespace) -}}
{{- end -}}
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
{{- default (include "image-patcher.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret carrying registry auth (docker config JSON).
Fixed to match the controller's defaultDockerAuthSecret const in
internal/controller/imagepatch_controller.go, which the build Job mounts
as Kaniko's docker config (used for base-image pull, cache, and push).
The chart-generated Secret and the dedup mount reuse this name, and the
data key is always "config.json" (the build Job mounts it with subPath
config.json and no remapping).
*/}}
{{- define "image-patcher.registryAuthSecretName" -}}
image-registry-secret
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
