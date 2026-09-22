{{- define "joulie.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "joulie.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "joulie.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "joulie.labels" -}}
app.kubernetes.io/name: {{ include "joulie.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "joulie.selectorLabels" -}}
app.kubernetes.io/name: {{ include "joulie.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}


{{/*
joulie.controllerManager renders the controllerManager values section as YAML.
Until the release after next, keys the user still sets under the deprecated
"operator" section are merged on top, so a values file written for the old key
keeps working. Read it with:
  {{- $cm := include "joulie.controllerManager" . | fromYaml }}
*/}}
{{- define "joulie.controllerManager" -}}
{{- $cm := deepCopy (.Values.controllerManager | default dict) -}}
{{- $legacy := .Values.operator | default dict -}}
{{- mergeOverwrite $cm $legacy | toYaml -}}
{{- end -}}
