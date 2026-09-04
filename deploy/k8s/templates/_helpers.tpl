{{/*
Common labels applied to every resource in the chart. `component` doubles as
the Job selector label used by `k8s-apply` (kubectl delete job -l
app.kubernetes.io/component=migrate) to work around Job immutability.
*/}}
{{- define "scheduler.labels" -}}
app.kubernetes.io/name: scheduler
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/part-of: scheduler
app.kubernetes.io/component: {{ .component }}
helm.sh/chart: scheduler-{{ .ctx.Chart.Version }}
{{- end }}