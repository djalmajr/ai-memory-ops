{{/*
Expand the name of the chart.
*/}}
{{- define "ai-memory-svc.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "ai-memory-svc.fullname" -}}
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
Common labels
*/}}
{{- define "ai-memory-svc.labels" -}}
app: {{ include "ai-memory-svc.name" . }}
app.kubernetes.io/name: {{ include "ai-memory-svc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{/*
Selector labels (component-agnostic)
*/}}
{{- define "ai-memory-svc.selectorLabels" -}}
app: {{ include "ai-memory-svc.name" . }}
app.kubernetes.io/name: {{ include "ai-memory-svc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Secret name (created by the chart OR pre-existing in the namespace)
*/}}
{{- define "ai-memory-svc.secretName" -}}
{{- printf "%s-secrets" (include "ai-memory-svc.fullname" .) }}
{{- end }}

{{/*
ConfigMap name
*/}}
{{- define "ai-memory-svc.configMapName" -}}
{{- printf "%s-config" (include "ai-memory-svc.fullname" .) }}
{{- end }}

{{/*
PVC name (shared between mcp and etl-cron — SQLite + wiki clone + models cache).
See the storage-class notes in the chart values.
*/}}
{{- define "ai-memory-svc.dbPvcName" -}}
{{- printf "%s-data" (include "ai-memory-svc.fullname" .) }}
{{- end }}

{{/*
Service name for the optional `contributors` admission webhook deployment.
*/}}
{{- define "ai-memory-svc.contributorsFullname" -}}
{{- printf "%s-contributors" (include "ai-memory-svc.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Default admission webhook entry for the in-cluster `contributors` Service.
Rendered as YAML so deployment.yaml can `fromYaml` it and merge into the
JSON env var. Operators can still add MORE webhooks via
`aiMemory.admissionWebhooks` in values.yaml (this default is appended).
*/}}
{{- define "ai-memory-svc.contributorsEntry" -}}
name: contributors
url: http://{{ include "ai-memory-svc.contributorsFullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.webhooks.contributors.port | default 8080 }}/enrich
timeout_ms: {{ .Values.webhooks.contributors.timeoutMs | default 2000 }}
failure_policy: {{ .Values.webhooks.contributors.failurePolicy | default "ignore" }}
events:
  - write_page
  - consolidate
{{- end }}

{{/*
Service name for the optional `git-mirror` admission webhook deployment.
*/}}
{{- define "ai-memory-svc.gitMirrorFullname" -}}
{{- printf "%s-git-mirror" (include "ai-memory-svc.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Default admission webhook entry for the in-cluster `git-mirror` Service.
*/}}
{{- define "ai-memory-svc.gitMirrorEntry" -}}
name: git-mirror
url: http://{{ include "ai-memory-svc.gitMirrorFullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.webhooks.gitMirror.port | default 8080 }}/sync
timeout_ms: {{ .Values.webhooks.gitMirror.timeoutMs | default 2000 }}
failure_policy: {{ .Values.webhooks.gitMirror.failurePolicy | default "ignore" }}
events:
  - write_page
  - consolidate
  - delete
  - purge_project
{{- end }}
