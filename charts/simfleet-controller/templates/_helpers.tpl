{{/*
Expand the name of the chart.
*/}}
{{- define "simfleet-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "simfleet-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "simfleet-controller.labels" -}}
helm.sh/chart: {{ include "simfleet-controller.chart" . }}
{{ include "simfleet-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: simfleet-controller
{{- end }}

{{/*
Selector labels
*/}}
{{- define "simfleet-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "simfleet-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
