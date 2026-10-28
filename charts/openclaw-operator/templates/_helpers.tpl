{{/*
Expand the name of the chart.
*/}}
{{- define "openclaw-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "openclaw-operator.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "openclaw-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "openclaw-operator.labels" -}}
helm.sh/chart: {{ include "openclaw-operator.chart" . }}
{{ include "openclaw-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "openclaw-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "openclaw-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "openclaw-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "openclaw-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Manager rules. Single source of truth so the same set of permissions is
rendered into the cluster-scoped ClusterRole or per-namespace Role.
hack/check-helm-rbac-sync.sh asserts this set is a superset of the
kubebuilder-generated config/rbac/role.yaml.
*/}}
{{- define "openclaw-operator.managerRules" -}}
# Core API resources
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: [""]
  resources: ["services"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
# Apps API
- apiGroups: ["apps"]
  resources: ["statefulsets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list", "watch", "delete"]
# Batch API (backup/restore Jobs, periodic backup CronJobs)
- apiGroups: ["batch"]
  resources: ["jobs"]
  verbs: ["get", "list", "watch", "create", "delete"]
- apiGroups: ["batch"]
  resources: ["cronjobs"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
# RBAC
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles", "rolebindings"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
# Networking
- apiGroups: ["networking.k8s.io"]
  resources: ["networkpolicies", "ingresses"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
# Gateway API
- apiGroups: ["gateway.networking.k8s.io"]
  resources: ["httproutes"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
# Policy
- apiGroups: ["policy"]
  resources: ["poddisruptionbudgets"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
# Autoscaling
- apiGroups: ["autoscaling"]
  resources: ["horizontalpodautoscalers"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
# Monitoring
- apiGroups: ["monitoring.coreos.com"]
  resources: ["servicemonitors", "prometheusrules"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
# OpenClaw CRDs
- apiGroups: ["openclaw.rocks"]
  resources: ["openclawinstances"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["openclaw.rocks"]
  resources: ["openclawinstances/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["openclaw.rocks"]
  resources: ["openclawinstances/finalizers"]
  verbs: ["update"]
# OpenClawSelfConfig CRD
- apiGroups: ["openclaw.rocks"]
  resources: ["openclawselfconfigs"]
  verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
- apiGroups: ["openclaw.rocks"]
  resources: ["openclawselfconfigs/status"]
  verbs: ["get", "update", "patch"]
- apiGroups: ["openclaw.rocks"]
  resources: ["openclawselfconfigs/finalizers"]
  verbs: ["update"]
{{- end }}

{{/*
Cluster-scoped manager rules. OpenClawClusterDefaults is a cluster-scoped
singleton (#457), so the operator needs these permissions via a ClusterRole
even when it watches only specific namespaces -- a namespaced Role cannot grant
access to a cluster-scoped resource (#529). Kept separate from managerRules so
the namespaced-mode per-namespace Roles do not carry an ineffective rule, while
the cluster-wide ClusterRole renders both sets.
hack/check-helm-rbac-sync.sh parses this define alongside managerRules so the
union is asserted to be a superset of config/rbac/role.yaml.
*/}}
{{- define "openclaw-operator.clusterScopedRules" -}}
# Read the instance namespace's Pod Security labels (storage.fixOwnership default)
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get"]
{{- if .Values.watchNamespaces }}
  resourceNames: {{ .Values.watchNamespaces | toJson }}
{{- end }}
# OpenClawClusterDefaults singleton (#457)
- apiGroups: ["openclaw.rocks"]
  resources: ["openclawclusterdefaults"]
  verbs: ["get", "list", "watch"]
{{- end }}
