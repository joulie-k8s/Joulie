{{- define "joulie-simulator.name" -}}
joulie-telemetry-sim
{{- end -}}

{{- define "joulie-simulator.labels" -}}
app.kubernetes.io/name: {{ include "joulie-simulator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "joulie-simulator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "joulie-simulator.name" . }}
{{- end -}}
