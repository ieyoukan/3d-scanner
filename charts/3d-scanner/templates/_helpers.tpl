{{/*
共通で使う小さなテンプレート（部品）。
define で名前を付け、他のファイルから include で呼び出す。
*/}}

{{/*
リソース名の基になる名前。
Service の名前は英字で始まる必要がある（DNS ラベルの規則）ので、数字で始まる chart 名
"3d-scanner" はそのまま使えない。既定を "scanner" にしている。
*/}}
{{- define "3d-scanner.fullname" -}}
{{- .Values.fullnameOverride | default "scanner" | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{/* 全リソースに付ける共通ラベル。kubectl get -l や Argo CD の表示で、このアプリのものだと分かる。 */}}
{{- define "3d-scanner.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/name: 3d-scanner
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}

{{/*
Service が転送先の Pod を選ぶためのラベル。
ここを後から変えると Deployment の selector（変更不可の項目）と食い違うので、最小限にしておく。
*/}}
{{- define "3d-scanner.selectorLabels" -}}
app.kubernetes.io/name: 3d-scanner
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: app
{{- end -}}

{{- define "3d-scanner.postgresql.fullname" -}}
{{- printf "%s-postgresql" (include "3d-scanner.fullname" .) -}}
{{- end -}}

{{- define "3d-scanner.postgresql.selectorLabels" -}}
app.kubernetes.io/name: 3d-scanner
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/component: postgresql
{{- end -}}

{{/* この chart が作る Secret の名前（existingSecret を使わないときだけ作られる）。 */}}
{{- define "3d-scanner.secretName" -}}
{{- include "3d-scanner.fullname" . -}}
{{- end -}}

{{/* DB のパスワードを持つ Secret の名前とキー。内蔵 PostgreSQL か外部 DB かで出どころが変わる。 */}}
{{- define "3d-scanner.database.secretName" -}}
{{- if .Values.postgresql.enabled -}}
{{- .Values.postgresql.auth.existingSecret | default (include "3d-scanner.secretName" .) -}}
{{- else -}}
{{- required "postgresql.enabled=false のときは externalDatabase.existingSecret（DB パスワードの Secret 名）が必要です" .Values.externalDatabase.existingSecret -}}
{{- end -}}
{{- end -}}

{{- define "3d-scanner.database.secretKey" -}}
{{- if .Values.postgresql.enabled -}}
{{- .Values.postgresql.auth.existingSecretKey -}}
{{- else -}}
{{- .Values.externalDatabase.existingSecretKey -}}
{{- end -}}
{{- end -}}

{{/* この chart が Secret を作る必要があるか（DB パスワードが値で直接渡されたときだけ作る）。 */}}
{{- define "3d-scanner.createSecret" -}}
{{- $db := and .Values.postgresql.enabled (not .Values.postgresql.auth.existingSecret) .Values.postgresql.auth.password -}}
{{- if $db -}}true{{- end -}}
{{- end -}}

{{/*
設定の組み合わせを検証し、足りなければ helm template / install の時点で止める。
「秘密の値が未設定のまま配備され、起動してから気づく」ことを防ぐ。
*/}}
{{- define "3d-scanner.validate" -}}
{{- if and .Values.postgresql.enabled (not .Values.postgresql.auth.existingSecret) (not .Values.postgresql.auth.password) -}}
{{- fail "PostgreSQL のパスワードが未設定です。postgresql.auth.existingSecret（推奨）か postgresql.auth.password を指定してください。" -}}
{{- end -}}
{{- if and (not .Values.postgresql.enabled) (not .Values.externalDatabase.host) -}}
{{- fail "postgresql.enabled=false のときは externalDatabase.host が必要です。" -}}
{{- end -}}
{{- end -}}
