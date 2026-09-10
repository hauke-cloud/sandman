# sandman

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

Wake-on-LAN and graceful shutdown for Kubernetes nodes, with a REST API, Prometheus metrics, alerting rules and a Grafana dashboard.

A cluster of physical machines does not need every machine running all the time.
This chart runs [sandman](https://github.com/hauke-cloud/sandman), which turns
them on with a Wake-on-LAN magic packet and off through the kubelet, describes
each one with a `SandmanDevice` custom resource, and exposes the fleet over a
small REST API.

## Installation

```sh
helm install sandman oci://ghcr.io/hauke-cloud/charts/sandman \
  --namespace sandman --create-namespace
```

Then describe a machine:

```yaml
apiVersion: sandman.hauke.cloud/v1alpha1
kind: SandmanDevice
metadata:
  name: worker-01
spec:
  nodeRef:
    name: worker-01
  wakeOnLAN:
    macAddress: "aa:bb:cc:dd:ee:ff"
    broadcastAddress: "192.168.1.255"
```

A device is created `Unmanaged`: sandman reports its state and does not touch it
until something asks. Devices can also be declared in `devices` in this chart's
values, which is convenient for a small fleet described in one place.

## The CRD is installed by the controller, not by this chart

There is no `crds/` directory here on purpose. Sandman applies its own
`CustomResourceDefinition` at start-up, from a copy embedded in the binary.

Helm installs a chart's `crds/` once and never upgrades it — it refuses to touch
a CRD on `helm upgrade`, because it cannot know what would be lost — so a
chart-installed CRD drifts behind the controller that depends on it, and the
symptom is a field the API server silently drops. Applying it from the binary
binds the schema to the code that understands it.

The cost is one permission and one ordering quirk:

- The ClusterRole includes `apiextensions.k8s.io/customresourcedefinitions`.
  Drop it with `crds.rbac=false`, which also requires `crds.install=false`.
- Devices declared in `devices` on a **first** install may be rejected until the
  pod has started once, because the type does not exist until then. Run the
  upgrade again, or create the devices separately.

For a cluster where a controller may not write CRDs, install it yourself and
turn the behaviour off:

```sh
kubectl apply -f https://raw.githubusercontent.com/hauke-cloud/sandman/main/internal/crd/assets/sandman.hauke.cloud_sandmandevices.yaml

helm install sandman oci://ghcr.io/hauke-cloud/charts/sandman \
  --set crds.install=false --set crds.rbac=false
```

## Host networking

`hostNetwork` is **true** by default, and the chart is close to useless without
it. A magic packet is a link-local broadcast: from inside the pod network it
reaches the CNI bridge and stops there.

- On a host with more than one network, set `controller.wolSourceAddress` to an
  address on the machines' network. Otherwise the kernel picks an interface and
  the packet leaves by the wrong one.
- The port is a host port, so replicas cannot share a node. The chart refuses
  `replicaCount > 1` alongside `hostNetwork` rather than leaving a second pod
  `Pending` forever.
- Pin sandman with a `nodeSelector` to a machine it will not switch off
  underneath itself — a control-plane node is the usual answer.

## Securing the API

An API that turns machines off is worth a token even inside a cluster:

```sh
kubectl -n sandman create secret generic sandman-api --from-literal=token="$(openssl rand -hex 32)"

helm upgrade sandman ... --set api.existingSecret=sandman-api
```

Every `/api/v1` route then requires `Authorization: Bearer <token>`. `/healthz`,
`/readyz` and `/metrics` stay open, because the kubelet and Prometheus have no
token to present. `api.token` writes the token into a Secret this release owns,
which means anyone who can run `helm get values` can read it — fine for a trial,
not for anything else. Either way it is mounted as a file rather than passed
through the environment, so it does not appear in `kubectl describe pod`.

`api.readOnly=true` refuses every state-changing route outright, which is what
you want for an instance behind a dashboard. The chart refuses to publish an
unauthenticated write API through an Ingress.

## Privileged shutdown pods

Stopping a machine means scheduling a privileged pod onto it that enters PID 1's
namespaces and calls `systemctl poweroff`. That pod needs a namespace which
allows privileged workloads under Pod Security admission:

```yaml
shutdown:
  namespace: sandman-shutdown
  createNamespace: true
```

The chart then creates that namespace labelled `pod-security.kubernetes.io/enforce:
privileged`, and the release namespace itself can stay restricted. Without this,
the shutdown pods land in the release namespace, and stops fail with an
admission error if it is not labelled for them.

Sandman's own pod is unprivileged: non-root, read-only root filesystem, no
capabilities.

## Monitoring

```yaml
metrics:
  serviceMonitor:
    enabled: true
    labels:
      release: prometheus   # whatever your Prometheus selects on
  prometheusRule:
    enabled: true
  grafanaDashboard:
    enabled: true
```

The default alerts match on the `service` label the Prometheus Operator derives
from this chart's Service, so they do not depend on the scrape's job name:

| Alert | Fires when |
| --- | --- |
| `SandmanDown` | sandman has not been scraped for 15 minutes |
| `SandmanDeviceTransitionFailed` | sandman gave up on a start or a stop |
| `SandmanDeviceStuckInTransition` | a device has been between states past `stuckFor` |
| `SandmanDeviceUnexpectedlyOff` | a machine is off while something wants it on |
| `SandmanWakePacketsFailing` | magic packets cannot be sent — a network problem here, not there |
| `SandmanShutdownFailing` | a shutdown pod could not be created, usually Pod Security |
| `SandmanDeviceCollectFailing` | sandman cannot read its own devices |

`SandmanDeviceUnexpectedlyOff` is the critical one: it means workloads are
somewhere other than where they were meant to be. `SandmanDeviceStuckInTransition`
is a backstop — the controller declares its own transitions failed on time, so a
device stuck here means sandman has stopped reconciling rather than that a
machine is slow.

Set `metrics.prometheusRule.rules` to replace the lot with your own.

The dashboard ConfigMap is labelled `grafana_dashboard: "1"` for the
grafana-k8s-sidecar. It shows the fleet at a glance, a table of every machine
and its state, how long starts and stops take, and the controller's own loop.

## Scaling

A second replica is a warm standby, not extra throughput: only the leader acts
on devices, and both serve the API. With `hostNetwork` on — the default — they
cannot share a node anyway, so `replicaCount: 1` and a fast restart is usually
the better trade. Leader election stays on regardless, so a rollout never has
two controllers draining the same node.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for the pod. |
| api.existingSecret | string | `""` | Name of an existing Secret holding the token. Set this and `token` is ignored. |
| api.readOnly | bool | `false` | Refuse every request that would change a machine's power state. Reads keep working. Useful for an instance exposed to a dashboard. |
| api.secretKey | string | `"token"` | Key inside the Secret that holds the token. |
| api.token | string | `""` | Bearer token required on every API route. Empty leaves the API open to anything that can reach the Service -- which, for an API that turns machines off, is worth thinking about even inside a cluster. Stored in a Secret this chart creates and mounted as a file, so it never appears in the pod's environment. |
| controller.leaderElection | bool | `true` | Elect one replica to act on devices. Leave this on: two replicas draining the same node is not something to find out about later. |
| controller.leaderElectionID | string | `"sandman.hauke.cloud"` | Name of the leader election lease. |
| controller.logFormat | string | `"json"` | Log format: text or json. |
| controller.logLevel | string | `"info"` | Log level: debug, info, warn or error. |
| controller.wolSourceAddress | string | `""` | Local address magic packets are sent from. Empty lets the kernel choose, which is right on a host with one network. On a host with several -- a node with a storage network, or a second NIC facing the machines -- name the address on the network the machines are on, or the packets leave by the wrong door. |
| crds.install | bool | `true` | Let sandman apply its own CustomResourceDefinition at start-up. This is the default because a chart's `crds/` directory is installed once and never upgraded -- Helm refuses to touch a CRD on `helm upgrade` -- so a chart-installed CRD drifts behind the controller and fields start disappearing silently. Applying it from the binary keeps the schema and the code that reads it in step. |
| crds.rbac | bool | `true` | Grant the RBAC needed for that. Turning this off while `install` is on makes sandman fail at start-up with a message saying exactly that. |
| devices | list | `[]` | SandmanDevices to create with the release. Handy for a small fleet described in one values file; a larger one is usually better off with the devices in their own manifests. Every field of the CRD is accepted here. |
| dnsPolicy | string | `"ClusterFirstWithHostNet"` | DNS policy. With host networking, ClusterFirstWithHostNet keeps cluster DNS working; the API server itself is reached by IP, so this only matters for anything else the pod resolves. |
| env | list | `[]` | Extra environment variables for the container. |
| envFrom | list | `[]` | Extra environment sources for the container. |
| fullnameOverride | string | `""` | Overrides the full name of the released resources. |
| hostNetwork | bool | `true` | Host networking. A magic packet is a link-local broadcast: from inside the pod network it reaches the CNI's bridge and stops there. Host networking is what puts it on the network the machines are actually on, so it is on by default -- this chart does not work without it unless every device names a routable directed broadcast address and the network forwards those. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/hauke-cloud/sandman"` | Container image repository. |
| image.tag | string | `""` | Image tag. Empty uses the chart's appVersion. |
| imagePullSecrets | list | `[]` | Secrets for pulling the image from a private registry. |
| ingress.annotations | object | `{}` | Annotations for the Ingress. |
| ingress.className | string | `""` | Ingress class. |
| ingress.enabled | bool | `false` | Expose the REST API through an Ingress. Think about `api.token` first. |
| ingress.hosts | list | `[{"host":"sandman.local","paths":[{"path":"/","pathType":"Prefix"}]}]` | Hosts and paths. |
| ingress.tls | list | `[]` | TLS configuration. |
| livenessProbe | object | `{"httpGet":{"path":"/healthz","port":"http"},"initialDelaySeconds":5,"periodSeconds":30}` | Liveness probe. It does not depend on the cluster or on any machine: restarting sandman cannot fix an unreachable node, it would only interrupt the transitions in flight. |
| metrics.grafanaDashboard.annotations | object | `{}` | Extra annotations for the ConfigMap. The folder annotation is added automatically when `folder` is set. |
| metrics.grafanaDashboard.enabled | bool | `false` | Create a ConfigMap holding the Grafana dashboard, labelled `grafana_dashboard: "1"` for the grafana-k8s-sidecar. |
| metrics.grafanaDashboard.folder | string | `""` | Grafana folder the sidecar imports the dashboard into. Requires the sidecar's folder feature to be enabled. |
| metrics.grafanaDashboard.labels | object | `{}` | Extra labels for the ConfigMap. |
| metrics.grafanaDashboard.namespace | string | `""` | Namespace the dashboard ConfigMap is created in. Empty uses the release namespace. |
| metrics.prometheusRule.annotations | object | `{}` | Annotations for the PrometheusRule. |
| metrics.prometheusRule.enabled | bool | `false` | Create a PrometheusRule with a default set of alerts. Requires the monitoring.coreos.com CRDs. |
| metrics.prometheusRule.labels | object | `{}` | Extra labels for the PrometheusRule. |
| metrics.prometheusRule.namespace | string | `""` | Namespace the PrometheusRule is created in. Empty uses the release namespace. |
| metrics.prometheusRule.rules | list | `[]` | Alerting rules. Empty uses the chart's defaults, which match on the `service` label the Prometheus Operator derives from this chart's Service, so they work regardless of the scrape's job name. |
| metrics.prometheusRule.stuckFor | string | `"20m"` | How long a device may sit in a transition before it is alerted on. The controller's own timeouts declare a transition failed first; this is the backstop for a sandman that has stopped reconciling at all. |
| metrics.prometheusRule.unexpectedlyOffFor | string | `"10m"` | How long a machine may be off while something is asking for it to be on before that is alerted on. |
| metrics.prometheusRule.wakeFailureRate | float | `0.05` | Rate of failed wake attempts, per second, above which the network path to the machines is treated as broken. |
| metrics.serviceMonitor.annotations | object | `{}` | Annotations for the ServiceMonitor. |
| metrics.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor scraping /metrics. Requires the monitoring.coreos.com CRDs. |
| metrics.serviceMonitor.interval | string | `"30s"` | How often Prometheus scrapes /metrics. Scraping reads the controller's own cache and touches neither the cluster nor a machine, so a short interval costs nothing -- and a boot takes a minute or two, which is not many samples at a slow one. |
| metrics.serviceMonitor.labels | object | `{}` | Extra labels for the ServiceMonitor. Prometheus instances commonly select on a label like `release: prometheus`, which belongs here. |
| metrics.serviceMonitor.metricRelabelings | list | `[]` | Metric relabeling rules applied to the scrape. |
| metrics.serviceMonitor.namespace | string | `""` | Namespace the ServiceMonitor is created in. Empty uses the release namespace. Prometheus often watches other namespaces, so point this at its own when the Service is scraped from a different one. |
| metrics.serviceMonitor.relabelings | list | `[]` | Relabeling rules applied to the scrape targets. |
| metrics.serviceMonitor.scrapeTimeout | string | `"10s"` | Give up on a scrape after this long. |
| nameOverride | string | `""` | Overrides the chart name used in resource names. |
| nodeSelector | object | `{}` | Node selector for the pod. Worth setting: sandman has to run on a machine that is on the same network as the ones it wakes, and that is not going to be switched off by sandman itself. |
| podAnnotations | object | `{}` | Extra annotations for the pod. |
| podDisruptionBudget | object | `{"enabled":false,"minAvailable":1}` | PodDisruptionBudget for the controller. |
| podLabels | object | `{}` | Extra labels for the pod. |
| podSecurityContext | object | `{"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context. The sandman pod itself is unprivileged; the only privileged thing in this chart is the short-lived shutdown pod it creates on a node that is being stopped. |
| rbac.create | bool | `true` | Create the ClusterRole and binding sandman needs. |
| readinessProbe | object | `{"httpGet":{"path":"/readyz","port":"http"},"initialDelaySeconds":2,"periodSeconds":10}` | Readiness probe. Fails until the device cache has synced, because an unsynced cache answers "no devices" rather than "not ready". |
| replicaCount | int | `1` | Number of replicas. A second replica is a warm standby, not extra throughput: only the leader acts on devices, and both serve the API. |
| resources | object | `{"limits":{"memory":"256Mi"},"requests":{"cpu":"10m","memory":"64Mi"}}` | Resource requests and limits. sandman holds a device per machine and a node per machine in memory and does nothing between requests. No CPU limit is set on purpose: throttling a controller only delays the reconcile that notices a machine has come back. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container-level security context. The image is a single static binary that writes nothing, so it runs read-only with no capabilities. Broadcasting a magic packet needs none: the kernel sets SO_BROADCAST on ordinary UDP sockets. |
| service.annotations | object | `{}` | Annotations for the Service. |
| service.port | int | `8080` | Service port. With host networking this is also the port on the host. |
| service.type | string | `"ClusterIP"` | Service type. |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount. |
| serviceAccount.automount | bool | `true` | Mount the ServiceAccount token. Required: sandman talks to the Kubernetes API for everything it does. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount for the pod. |
| serviceAccount.name | string | `""` | Name of the ServiceAccount. Empty generates one from the fullname. |
| shutdown.createNamespace | bool | `false` | Create that namespace, labelled for the privileged Pod Security standard. Only meaningful when `namespace` is set to something other than the release namespace. |
| shutdown.namespace | string | `""` | Namespace the privileged shutdown pods are created in. Empty uses the release namespace. That namespace must allow privileged pods, so a dedicated one is often tidier than relaxing Pod Security on the namespace everything else runs in. |
| startupProbe | object | `{"failureThreshold":24,"httpGet":{"path":"/healthz","port":"http"},"periodSeconds":5}` | Startup probe, allowing time for the CRD to be applied and established. |
| strategy | object | `{"type":"Recreate"}` | Deployment update strategy. Recreate keeps a host port from colliding during a rollout, which matters because host networking is the default. |
| terminationGracePeriodSeconds | int | `30` | Grace period for shutdown. sandman finishes in-flight requests and stops; the transitions it was driving are picked up from the cluster by whatever starts next. |
| tolerations | list | `[]` | Tolerations for the pod. |

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Hauke Mettendorf | <hauke@mettendorf.it> |  |

## Source Code

* <https://github.com/hauke-cloud/sandman>
