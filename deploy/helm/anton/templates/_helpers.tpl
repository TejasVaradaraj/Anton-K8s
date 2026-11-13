{{- define "anton.image" -}}{{ .Values.image.repository }}:{{ .Values.image.tag }}{{- end -}}
{{- define "anton.sandboxImage" -}}{{ .Values.sandboxImage.repository }}:{{ .Values.sandboxImage.tag }}{{- end -}}
{{- define "anton.webImage" -}}{{ .Values.webImage.repository }}:{{ .Values.webImage.tag }}{{- end -}}

{{- define "anton.labels" -}}
app.kubernetes.io/part-of: anton
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* Scheduling for pods that must survive every disruption. */}}
{{- define "anton.pinned" -}}
nodeSelector: {{- toYaml .Values.controlPlaneScheduling.nodeSelector | nindent 2 }}
tolerations: {{- toYaml .Values.controlPlaneScheduling.tolerations | nindent 2 }}
{{- end -}}

{{- define "anton.databaseURLEnv" -}}
{{- if eq .Values.postgres.mode "cnpg" }}
# CloudNativePG writes connection details for the app user into this Secret.
- name: DB_USER
  valueFrom: {secretKeyRef: {name: anton-db-app, key: username}}
- name: DB_PASSWORD
  valueFrom: {secretKeyRef: {name: anton-db-app, key: password}}
- name: DATABASE_URL
  value: "postgresql+psycopg://$(DB_USER):$(DB_PASSWORD)@anton-db-rw:5432/anton"
{{- else }}
- name: DB_PASSWORD
  valueFrom: {secretKeyRef: {name: anton-db, key: password}}
- name: DATABASE_URL
  value: "postgresql+psycopg://anton:$(DB_PASSWORD)@anton-postgres:5432/anton"
{{- end }}
{{- if .Values.tenancy.enabled }}
- {name: TENANCY, value: "1"}
{{- end }}
{{- end -}}
