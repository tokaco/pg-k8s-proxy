{{/*
Chart name, overridable.
*/}}
{{- define "pg-k8s-proxy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified resource name.
*/}}
{{- define "pg-k8s-proxy.fullname" -}}
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
Cluster-scoped resources are not namespaced, so their names carry the namespace
to keep two releases in different namespaces from colliding.
*/}}
{{- define "pg-k8s-proxy.clusterScopedName" -}}
{{- printf "%s-%s" (include "pg-k8s-proxy.fullname" .) .Release.Namespace | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "pg-k8s-proxy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Labels on every object.
*/}}
{{- define "pg-k8s-proxy.labels" -}}
helm.sh/chart: {{ include "pg-k8s-proxy.chart" . }}
{{ include "pg-k8s-proxy.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: pg-k8s-proxy
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Labels that select the gateway pods. These must never change for a release.
*/}}
{{- define "pg-k8s-proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pg-k8s-proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "pg-k8s-proxy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "pg-k8s-proxy.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container image, preferring a digest over a tag when both are given.
*/}}
{{- define "pg-k8s-proxy.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
Whether the operator watches the whole cluster.
*/}}
{{- define "pg-k8s-proxy.clusterScoped" -}}
{{- eq .Values.scope.type "Cluster" -}}
{{- end }}

{{/*
The namespaces a Namespaced-scope release covers: the release namespace plus
anything the user listed. Rendered as a YAML list for `list` consumption.
*/}}
{{- define "pg-k8s-proxy.watchedNamespaces" -}}
{{- $namespaces := concat (list .Release.Namespace) (default (list) .Values.scope.namespaces) | uniq | sortAlpha -}}
{{- toYaml $namespaces -}}
{{- end }}

{{/*
Fail fast on value combinations that would deploy something broken.
*/}}
{{- define "pg-k8s-proxy.validateValues" -}}
{{- if not (has .Values.scope.type (list "Cluster" "Namespaced")) -}}
{{- fail (printf "scope.type must be Cluster or Namespaced, got %q" .Values.scope.type) -}}
{{- end -}}
{{- if and (eq .Values.scope.type "Cluster") .Values.scope.namespaces -}}
{{- fail "scope.namespaces is only meaningful when scope.type is Namespaced; remove it or switch to Namespaced" -}}
{{- end -}}
{{- if not (has .Values.proxy.tls.mode (list "disable" "allow" "require")) -}}
{{- fail (printf "proxy.tls.mode must be disable, allow, or require, got %q" .Values.proxy.tls.mode) -}}
{{- end -}}
{{- if and (ne .Values.proxy.tls.mode "disable") (not .Values.proxy.tls.secretName) -}}
{{- fail "proxy.tls.secretName is required when proxy.tls.mode is not disable" -}}
{{- end -}}
{{- if and .Values.serviceDiscovery.enabled (not .Values.serviceDiscovery.labelSelector) -}}
{{- fail "serviceDiscovery.labelSelector must not be empty; an empty selector would adopt every Service in scope" -}}
{{- end -}}
{{- if and .Values.podDisruptionBudget.enabled (not .Values.autoscaling.enabled) (lt (int .Values.replicaCount) 2) -}}
{{- fail "podDisruptionBudget.enabled needs replicaCount of at least 2, or every voluntary eviction would be blocked" -}}
{{- end -}}
{{- end }}

{{/*
Where TLS material is mounted inside the container.
*/}}
{{- define "pg-k8s-proxy.tlsMountPath" -}}/etc/pg-k8s-proxy/tls{{- end }}
{{- define "pg-k8s-proxy.clientCAMountPath" -}}/etc/pg-k8s-proxy/client-ca{{- end }}

{{/*
The permissions the operator needs, independent of how widely they are granted.
Reading Services resolves named ports and confirms a backend exists; writing
PostgresRoutes is what lets label-based Service discovery materialise routes.
*/}}
{{- define "pg-k8s-proxy.rules" -}}
- apiGroups:
    - pgproxy.io
  resources:
    - postgresroutes
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
- apiGroups:
    - pgproxy.io
  resources:
    - postgresroutes/status
  verbs:
    - get
    - update
    - patch
- apiGroups:
    - pgproxy.io
  resources:
    - postgresroutes/finalizers
  verbs:
    - update
- apiGroups:
    - ""
  resources:
    - services
  verbs:
    - get
    - list
    - watch
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
{{- if .Values.rbac.readCABundleSecrets }}
{{/*
Only Secrets labelled pgproxy.io/ca-bundle=true are ever read, and the operator
caches nothing else. Kubernetes RBAC cannot express a label restriction, so the
grant is broader than the access; keep this off unless routes need it.
*/}}
- apiGroups:
    - ""
  resources:
    - secrets
  verbs:
    - get
    - list
    - watch
{{- end }}
{{- end }}

