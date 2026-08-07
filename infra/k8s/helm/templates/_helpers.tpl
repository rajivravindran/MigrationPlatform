{{- define "mp.fullname" -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mp.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
environment: {{ .Values.global.environment }}
{{- end -}}

{{- define "mp.image" -}}
{{ tpl . $ }}
{{- end -}}

{{- define "mp.postgresDSN" -}}
postgres://{{ .Values.postgresql.auth.username }}:{{ .Values.postgresql.auth.password }}@{{ .Release.Name }}-postgresql:5432/{{ .Values.postgresql.auth.database }}
{{- end -}}

{{- define "mp.redisURL" -}}
redis://{{ .Release.Name }}-redis-master:6379/0
{{- end -}}

{{- define "mp.temporalAddress" -}}
{{ .Release.Name }}-temporal-frontend.{{ .Release.Namespace }}.svc.cluster.local:7233
{{- end -}}

{{- define "mp.minioEndpoint" -}}
http://{{ .Release.Name }}-minio:9000
{{- end -}}
