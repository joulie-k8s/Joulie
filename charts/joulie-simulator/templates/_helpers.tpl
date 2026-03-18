{{- define "joulie-simulator.fullname" -}}
joulie-telemetry-sim
{{- end -}}

{{- define "joulie-simulator.labels" -}}
app.kubernetes.io/name: joulie-telemetry-sim
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "joulie-simulator.selectorLabels" -}}
app.kubernetes.io/name: joulie-telemetry-sim
{{- end -}}
