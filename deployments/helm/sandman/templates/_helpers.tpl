{{/*
Expand the name of the chart.
*/}}
{{- define "sandman.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "sandman.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "sandman.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "sandman.labels" -}}
helm.sh/chart: {{ include "sandman.chart" . }}
{{ include "sandman.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/component: controller
app.kubernetes.io/part-of: sandman
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "sandman.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sandman.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "sandman.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "sandman.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding the API token.
*/}}
{{- define "sandman.secretName" -}}
{{- if .Values.api.existingSecret }}
{{- .Values.api.existingSecret }}
{{- else }}
{{- include "sandman.fullname" . }}
{{- end }}
{{- end }}

{{/*
Directory the API token is mounted at, and the full path of the token file.
The token is mounted rather than passed through the environment, so it does not
show up in `kubectl describe pod`.
*/}}
{{- define "sandman.tokenMountPath" -}}
/etc/sandman/secrets
{{- end }}

{{- define "sandman.tokenFile" -}}
{{- printf "%s/token" (include "sandman.tokenMountPath" .) }}
{{- end }}

{{/*
Namespace the privileged shutdown pods are created in.
*/}}
{{- define "sandman.shutdownNamespace" -}}
{{- default .Release.Namespace .Values.shutdown.namespace }}
{{- end }}

{{/*
The `service` label the Prometheus Operator derives from this chart's Service.
The default alerts match on it so they hold whatever the scrape job ends up
being called.
*/}}
{{- define "sandman.serviceLabel" -}}
{{- printf "%s/%s" .Release.Namespace (include "sandman.fullname" .) }}
{{- end }}

{{/*
Fail early on value combinations that produce a broken release.
*/}}
{{- define "sandman.validateValues" -}}
{{- if and .Values.crds.install (not .Values.crds.rbac) }}
{{- fail "sandman: crds.install needs crds.rbac, or sandman cannot apply its own CustomResourceDefinition. Set crds.install=false if the CRD is installed out of band." }}
{{- end }}
{{- if and .Values.crds.rbac (not .Values.rbac.create) }}
{{- fail "sandman: crds.rbac has nothing to attach to while rbac.create is false" }}
{{- end }}
{{- if and (not .Values.hostNetwork) (not .Values.crds.install) }}
{{- /* nothing: this combination is fine, it is only listed to keep the guards together */ -}}
{{- end }}
{{- if and .Values.ingress.enabled (not .Values.api.token) (not .Values.api.existingSecret) (not .Values.api.readOnly) }}
{{- fail "sandman: refusing to publish an unauthenticated write API through an Ingress. Set api.token, api.existingSecret, or api.readOnly=true." }}
{{- end }}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.controller.leaderElection) }}
{{- fail "sandman: more than one replica without leader election means two controllers draining the same node at once. Set controller.leaderElection=true or replicaCount=1." }}
{{- end }}
{{- if and (gt (int .Values.replicaCount) 1) .Values.hostNetwork }}
{{- fail "sandman: replicas share a host port under hostNetwork, so only one can run per node. Pin them apart with affinity and set hostNetwork=false, or keep replicaCount=1." }}
{{- end }}
{{- range .Values.devices }}
{{- if not .name }}
{{- fail "sandman: every entry in .Values.devices needs a name" }}
{{- end }}
{{- if not (.nodeRef).name }}
{{- fail (printf "sandman: device %s needs nodeRef.name" .name) }}
{{- end }}
{{- if not (.wakeOnLAN).macAddress }}
{{- fail (printf "sandman: device %s needs wakeOnLAN.macAddress" .name) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Default PrometheusRule alerts. They match on the `service` label the Prometheus
Operator sets from this chart's Service, so they keep working no matter what
job name the scrape ends up with.
*/}}
{{- define "sandman.defaultPrometheusRules" -}}
{{- $service := include "sandman.serviceLabel" . -}}
{{- $selector := printf `{service="%s"}` $service -}}
{{- $rule := .Values.metrics.prometheusRule -}}
- alert: SandmanDown
  expr: up{{ $selector }} == 0
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: sandman is not being scraped
    description: >-
      Prometheus has not been able to scrape {{ $service }} for 15 minutes. Nothing can be started or
      stopped while it is down, and a machine mid-transition is left where it was.
- alert: SandmanDeviceTransitionFailed
  expr: sandman_device_power_state{{ printf `{service="%s", state="Failed"}` $service }} == 1
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: Powering {{ `{{` }} $labels.node {{ `}}` }} on or off failed
    description: >-
      sandman gave up on the last power transition for {{ `{{` }} $labels.device {{ `}}` }} and will not retry it
      until a new request arrives. Look at the device's status for the reason -- a start that timed
      out means the machine never answered its magic packets, a stop that timed out usually means a
      drain that could not finish.
- alert: SandmanDeviceStuckInTransition
  expr: sandman_device_power_state{{ printf `{service="%s", state=~"Starting|Stopping"}` $service }} == 1
  for: {{ $rule.stuckFor | default "20m" }}
  labels:
    severity: warning
  annotations:
    summary: "{{ `{{` }} $labels.node {{ `}}` }} has been in transition for too long"
    description: >-
      {{ `{{` }} $labels.device {{ `}}` }} has been between power states for longer than its own timeout should
      allow. The controller declares a transition failed on its own, so a device stuck here means
      sandman has stopped reconciling it rather than that the machine is slow.
- alert: SandmanDeviceUnexpectedlyOff
  expr: >-
    (sandman_device_desired_power_state{{ printf `{service="%s", state="Running"}` $service }} == 1)
    and on(device, node) (sandman_device_power_state{{ printf `{service="%s", state="Stopped"}` $service }} == 1)
  for: {{ $rule.unexpectedlyOffFor | default "10m" }}
  labels:
    severity: critical
  annotations:
    summary: "{{ `{{` }} $labels.node {{ `}}` }} is off but is meant to be running"
    description: >-
      Something asked for {{ `{{` }} $labels.device {{ `}}` }} to be running and it is not. Either the machine went
      down on its own, or it was never woken. Its workloads are wherever the scheduler could put them.
- alert: SandmanWakePacketsFailing
  expr: >-
    rate(sandman_wake_attempts_total{{ printf `{service="%s", result="failure"}` $service }}[15m]) > {{ $rule.wakeFailureRate | default 0.05 }}
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: sandman cannot send magic packets for {{ `{{` }} $labels.node {{ `}}` }}
    description: >-
      The packets are not leaving the host, which is a network problem on sandman's side rather than
      a machine that will not wake. Check that the pod has host networking and that
      controller.wolSourceAddress names an address on the machines' network.
- alert: SandmanShutdownFailing
  expr: increase(sandman_shutdown_attempts_total{{ printf `{service="%s", result="failure"}` $service }}[1h]) > 0
  labels:
    severity: warning
  annotations:
    summary: sandman could not create a shutdown pod for {{ `{{` }} $labels.node {{ `}}` }}
    description: >-
      Creating the privileged pod that powers a machine off failed. The usual cause is Pod Security
      admission in {{ include "sandman.shutdownNamespace" . }}: the shutdown pod is privileged and needs a
      namespace that allows it.
- alert: SandmanDeviceCollectFailing
  expr: sandman_device_collect_failures_total{{ $selector }} == 1
  for: 10m
  labels:
    severity: info
  annotations:
    summary: sandman cannot read its own devices
    description: >-
      The device list could not be read on the last scrape, so every device gauge is missing rather
      than wrong. sandman is running but its cache is not usable.
{{- end }}
