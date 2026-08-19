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

{{/*
Name of the secret holding the webhook serving certificate.
Either the cert-manager-issued one, or a secret the user supplies.
*/}}
{{- define "graceful-drain-controller.certSecret" -}}
{{- if .Values.certManager.enabled -}}
{{ include "graceful-drain-controller.fullname" . }}-cert
{{- else -}}
{{- required "certManager.enabled is false, so tls.existingSecret must name a TLS secret holding the webhook serving certificate" .Values.tls.existingSecret -}}
{{- end -}}
{{- end }}
