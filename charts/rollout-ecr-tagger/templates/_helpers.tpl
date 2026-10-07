{{- define "rollout-ecr-tagger.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "rollout-ecr-tagger.fullname" -}}
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

{{- define "rollout-ecr-tagger.selectorLabels" -}}
app.kubernetes.io/name: {{ include "rollout-ecr-tagger.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "rollout-ecr-tagger.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "rollout-ecr-tagger.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "rollout-ecr-tagger.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "rollout-ecr-tagger.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Rules needed on the workloads, shared by the ClusterRole and the namespaced Roles. */}}
{{- define "rollout-ecr-tagger.workloadRules" -}}
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch", "update"]
- apiGroups: ["argoproj.io"]
  resources: ["rollouts"]
  verbs: ["get", "list", "watch", "patch", "update"]
# Read the workload referenced by a Rollout's spec.workloadRef.
- apiGroups: [""]
  resources: ["podtemplates"]
  verbs: ["get"]
- apiGroups: ["apps"]
  resources: ["replicasets"]
  verbs: ["get"]
{{- if .Values.enableDeployments }}
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get", "list", "watch", "patch"]
{{- else }}
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["get"]
{{- end }}
{{- end -}}
