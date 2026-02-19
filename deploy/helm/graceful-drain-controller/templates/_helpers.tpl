{{- define "graceful-drain-controller.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "graceful-drain-controller.labels" -}}
app.kubernetes.io/name: graceful-drain-controller
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "graceful-drain-controller.selectorLabels" -}}
app.kubernetes.io/name: graceful-drain-controller
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "graceful-drain-controller.drainTaints" -}}
{{- $taints := list }}
{{- range .Values.drainTaints }}
{{- $taints = append $taints (printf "%s:%s" .key .effect) }}
{{- end }}
{{- join "," $taints }}
{{- end }}
