{{/*
Helpers of the glusterfs-csi chart.

There is deliberately no nameOverride/fullnameOverride: every resource name is a
fixed literal equal to the kustomize manifests, so both installation methods
produce the same set of objects and can replace each other (D-32).
*/}}

{{/*
Top-level metadata.labels. The Helm specific labels of this helper must only be
used on top-level metadata.labels, never on a Pod template or a selector
(selectors are immutable and have to match the kustomize manifests, D-32).

Pass a dict: (dict "ctx" . "component" "node"), component may be omitted.
*/}}
{{- define "glusterfs-csi.labels" -}}
app.kubernetes.io/name: glusterfs-csi
app.kubernetes.io/part-of: glusterfs-csi
helm.sh/chart: {{ printf "%s-%s" .ctx.Chart.Name .ctx.Chart.Version | replace "+" "_" }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
{{- if .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end -}}

{{/*
Repository:tag of the driver image; an empty tag means .Chart.AppVersion.
*/}}
{{- define "glusterfs-csi.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{/*
Kubelet plugin directory of the node DaemonSet:
<kubeletDir>/plugins/glusterfs.csi.io
*/}}
{{- define "glusterfs-csi.pluginDir" -}}
{{- printf "%s/plugins/glusterfs.csi.io" .Values.kubeletDir -}}
{{- end -}}

{{/*
Directory the kubelet watches for plugin registrations:
<kubeletDir>/plugins_registry
*/}}
{{- define "glusterfs-csi.registrationDir" -}}
{{- printf "%s/plugins_registry" .Values.kubeletDir -}}
{{- end -}}
