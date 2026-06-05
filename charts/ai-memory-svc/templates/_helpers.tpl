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
Resolve the effective ingress backend (the actual ingress controller flavor).
`ingress.className: auto` (the default) auto-detects Traefik by checking whether the
`traefik.io/v1alpha1` CRD is installed in the cluster; otherwise it falls back to
`nginx`. Any explicit value (e.g. `traefik`, `nginx`) is passed through verbatim.
This is the single source of truth for the auth-edge branching — templates compare
against THIS helper, never against `.Values.ingress.className` directly, so the same
chart artifact works transparently on a Traefik lab and an nginx-only cluster.
*/}}
{{- define "ai-memory-svc.ingressBackend" -}}
{{- $cn := .Values.ingress.className | default "auto" -}}
{{- if eq $cn "auto" -}}
{{- if .Capabilities.APIVersions.Has "traefik.io/v1alpha1" -}}traefik{{- else -}}nginx{{- end -}}
{{- else -}}
{{- $cn -}}
{{- end -}}
{{- end -}}

{{/*
In-cluster URL of the mcp-auth /verify endpoint (used by the nginx external-auth
annotation and conceptually mirrored by the Traefik forwardAuth Middleware).
*/}}
{{- define "ai-memory-svc.mcpAuthVerifyUrl" -}}
{{- printf "http://%s.%s.svc.cluster.local:%v/verify" (include "ai-memory-svc.fullname" .) .Release.Namespace (.Values.service.authPort | default 8081) -}}
{{- end -}}

{{/*
nginx-ingress proxy buffer annotations for routes whose responses can carry
the oauth2-proxy session cookie. Keycloak tokens (access + id + refresh)
push the encrypted session past 4 KB, so oauth2-proxy splits it into
multiple `_oauth2_proxy_N` cookies; the `/oauth2/callback` and auth-
request responses then ship 4+ large `Set-Cookie` headers that overflow
nginx's default 4 KB `proxy_buffer_size` → 502 Bad Gateway. Bumping to
16k × 4 buffers absorbs ~64 KB of response headers, which is well above
what Keycloak realistically emits. Renders nothing when oauth2-proxy is
off or the operator explicitly disables the annotation, so existing
clusters without the issue stay unchanged.
*/}}
{{- define "ai-memory-svc.nginxOauth2BufferAnnotations" -}}
{{- if and .Values.oauth2Proxy.enabled .Values.oauth2Proxy.nginxBuffers.enabled }}
nginx.ingress.kubernetes.io/proxy-buffer-size: {{ .Values.oauth2Proxy.nginxBuffers.size | quote }}
nginx.ingress.kubernetes.io/proxy-buffers-number: {{ .Values.oauth2Proxy.nginxBuffers.number | quote }}
{{- end }}
{{- end -}}

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
# Fire-and-forget: the mirror only copies pages (never mutates/rejects), so the
# engine must not block writes waiting on the git push. Set
# `webhooks.gitMirror.blocking=true` to restore synchronous behaviour.
blocking: {{ .Values.webhooks.gitMirror.blocking | default false }}
events:
  - write_page
  - consolidate
  - delete
  - purge_project
{{- end }}

{{/*
Service name for the optional `scope-guard` admission webhook deployment.
*/}}
{{- define "ai-memory-svc.scopeGuardFullname" -}}
{{- printf "%s-scope-guard" (include "ai-memory-svc.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Default admission webhook entry for the in-cluster `scope-guard` Service.
Forces `failure_policy: reject` so a misconfigured rule (or a network
hiccup talking to the webhook) ABORTS the write — the whole point of
scope-guard is loud failure, not a silent skip. Subscribes to every
mutation event the engine fires so cross-scope writes can't sneak in
via consolidate / delete / move-project either.
*/}}
{{- define "ai-memory-svc.scopeGuardEntry" -}}
name: scope-guard
url: http://{{ include "ai-memory-svc.scopeGuardFullname" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.webhooks.scopeGuard.port | default 8080 }}/admit
timeout_ms: {{ .Values.webhooks.scopeGuard.timeoutMs | default 2000 }}
failure_policy: reject
blocking: true
events:
  - write_page
  - consolidate
  - delete
  - purge_project
  - move_project
{{- end }}
