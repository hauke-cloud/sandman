<a href="https://hauke.cloud" target="_blank"><img src="https://img.shields.io/badge/home-hauke.cloud-brightgreen" alt="hauke.cloud" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud" target="_blank"><img src="https://img.shields.io/badge/github-hauke.cloud-blue" alt="hauke.cloud Github Organisation" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud/readme-management" target="_blank"><img src="https://img.shields.io/badge/template-default-orange" alt="Repository type - default" style="display: block;" /></a>


# Sandman


<img src="https://raw.githubusercontent.com/hauke-cloud/.github/main/resources/img/organisation-logo-small.png" alt="hauke.cloud logo" width="109" height="123" align="right">


Wake-on-LAN and graceful shutdown for Kubernetes nodes, shipped with an OCI
image and a Helm chart carrying Prometheus metrics, alerting rules and a Grafana
dashboard.

A cluster of physical machines does not need every machine running all the time.
Sandman turns them on with a magic packet and off through the kubelet, describes
each one with a `SandmanDevice` custom resource, and exposes the fleet over a
small REST API — so "bring worker-03 up for the nightly batch" is one HTTP call
rather than a trip to the basement.

## 🚀 Getting started

```bash
helm install sandman oci://ghcr.io/hauke-cloud/charts/sandman \
  --namespace sandman --create-namespace

kubectl apply -f - <<'EOF'
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
EOF

kubectl get sandmandevices
# NAME        NODE        MAC                 DESIRED     STATE     READY   SINCE
# worker-01   worker-01   aa:bb:cc:dd:ee:ff   Unmanaged   Running   true    2m
```

Then, through the API:

```bash
kubectl -n sandman port-forward svc/sandman 8080:8080

curl -X POST localhost:8080/api/v1/devices/worker-01/stop
curl -s   localhost:8080/api/v1/devices/worker-01/status
curl -X POST localhost:8080/api/v1/devices/worker-01/start
```

## 🌗 How a machine goes up and down

Two very different mechanisms, because a machine that is off and a machine that
is running have nothing in common from the cluster's point of view.

**Starting is Wake-on-LAN.** A magic packet is six `0xff` bytes followed by the
target's MAC address sixteen times over. A sleeping NIC scans every frame it
sees for that pattern and powers the machine up when it matches. Nothing
acknowledges it, so sandman sends a few, then waits for the node to register and
report Ready — resending on an interval, because a NIC that is still settling
after a shutdown quietly drops the first one.

**Stopping is the kubelet.** There is no IPMI here, no BMC, no serial console —
the one thing sandman can always reach on a *running* node is Kubernetes itself.
So it cordons the node, drains it through the eviction API so
PodDisruptionBudgets are honoured, and then schedules a privileged pod onto the
node that enters PID 1's namespaces and asks the host's systemd to power off:

```
nsenter -t 1 -m -u -i -n -p -- systemctl poweroff
```

The machine runs its own shutdown sequence — unmounting filesystems, stopping
services — rather than having the power cut under a live kernel. The pod is
pinned with `spec.nodeName` rather than scheduled, because by then the node is
cordoned and no scheduler would put anything there.

**Noticing it worked** is the subtle part. A machine that loses power does not
set its node's Ready condition to false; there is nothing left to say so. The
condition simply stops being refreshed. Sandman therefore treats a node as off
when the object is gone *or* its Ready heartbeat is older than
`timeouts.stoppedGracePeriod`.

## 🔌 States

`GET /api/v1/devices/worker-01/status` reports one of these:

| State | Meaning |
| --- | --- |
| `Running` | the node is registered and Ready — the machine is up and working |
| `Stopped` | no node object, or its heartbeat has gone stale since a shutdown |
| `Starting` | magic packets have gone out; waiting for the node to appear |
| `Stopping` | draining, or the shutdown pod is running, or waiting for the node to go |
| `Pending` | a request is accepted but held — the device is paused |
| `Failed` | the last transition timed out or errored, and will not be retried on its own |
| `Unknown` | not observed yet |

`Starting`, `Stopping` and `Pending` come back with `"inFlight": true`, which is
the signal to keep polling rather than to conclude anything.

`desiredPowerState` takes `Running`, `Stopped` or `Unmanaged` — the two settled
states plus "leave it alone", so a spec and a status can be compared by reading
them. Not `On` and `Off`: YAML reads both of those as booleans, so
`desiredPowerState: On` would reach the API server as `true` and be rejected.

A `Failed` device stays failed on purpose. A machine that ignores magic packets
needs a person, not a retry loop — and a retry loop is very good at hiding that.
Asking again through the API clears it: every call writes a fresh request id,
and an id the controller has not acted on is a new request even when the
requested state has not changed.

## 🎛 The API

Everything is under `/api/v1`, and everything is addressable by device name or
by node name — an operator looking at a cluster thinks in nodes, and a device is
not necessarily named after the node it manages.

| Method | Path | Does |
| --- | --- | --- |
| `GET` | `/api/v1/devices` | every device, sorted, with a count per state |
| `GET` | `/api/v1/devices/{name}` | one device in full |
| `GET` | `/api/v1/devices/{name}/status` | the same in five fields, for polling |
| `POST` | `/api/v1/devices/{name}/start` | wake the machine |
| `POST` | `/api/v1/devices/{name}/stop` | drain and power the machine off |
| `POST` | `/api/v1/devices/{name}/release` | stop managing it; keep reporting it |
| `GET` | `/api/v1/nodes/{node}` | the device managing that node |
| `POST` | `/api/v1/nodes/{node}/start`, `/stop` | the same, by node |

A `POST` takes an optional body, entirely for the record:

```bash
curl -X POST localhost:8080/api/v1/devices/worker-01/stop \
  -H 'Content-Type: application/json' \
  -d '{"requestedBy": "nightly-batch", "reason": "no jobs queued"}'
```

It returns `202 Accepted` immediately. **The API never waits for hardware.** All
it does is record the desired state on the `SandmanDevice`; the controller
notices and acts. That is what makes a request survive sandman restarting,
keeps a caller from holding a connection open across a two-minute boot, and lets
a second replica serve requests without two controllers fighting over a power
switch.

Set a bearer token — `api.token` or, better, `api.existingSecret` — and every
API route requires it. `/healthz`, `/readyz` and `/metrics` stay open, because
the kubelet and Prometheus have no token to present.

## 📟 The custom resource

A `SandmanDevice` is cluster-scoped, like the `Node` it points at, and because
two devices in different namespaces aiming at one machine would be two
controllers fighting over a power switch.

```yaml
apiVersion: sandman.hauke.cloud/v1alpha1
kind: SandmanDevice
metadata:
  name: worker-02
spec:
  nodeRef:
    name: worker-02
  desiredPowerState: Running     # Running | Stopped | Unmanaged
  wakeOnLAN:
    macAddress: "aa:bb:cc:dd:ee:00"
    broadcastAddress: "192.168.1.255"
    packets: 3
    packetInterval: 200ms
    secureOnSecretRef:           # for NICs that want a six-byte password
      name: worker-02-secure-on
  drain:
    enabled: true
    ignoreDaemonSets: true
    deleteEmptyDirData: true
    force: false
    timeout: 5m
  shutdown:
    mode: PrivilegedPod          # or Disabled, to refuse stop requests
  timeouts:
    start: 10m
    stop: 10m
    wakeRetryInterval: 1m
    stoppedGracePeriod: 2m
  paused: false
```

A new device is `Unmanaged`: sandman reports its state and does not touch it
until something asks. `desiredPowerState` written here is a declaration —
sandman will keep the machine in that state and start it again if it goes down
on its own. The REST API writes the same field.

`paused: true` holds sandman off without discarding the request, which is the
difference between "hold on" and "never mind". `shutdown.mode: Disabled` makes a
stop request fail loudly rather than being ignored — right for a storage node
whose shutdown is somebody else's decision.

See [`examples/sandmandevice.yaml`](examples/sandmandevice.yaml) for the
annotated long form.

### Where the CRD comes from

Sandman installs its own `CustomResourceDefinition` at start-up, from a copy
embedded in the binary. The chart does **not** ship it.

That is deliberate. A chart's `crds/` directory is installed once and never
upgraded — Helm refuses to touch a CRD on `helm upgrade`, because it cannot know
what would be lost — so a chart-installed CRD drifts behind the controller that
depends on it, and the symptom is a field the API server silently drops.
Applying it from the binary binds the schema to the code that understands it.

The cost is one extra permission (`apiextensions.k8s.io/customresourcedefinitions`)
and one ordering quirk: devices declared in the same release as a first install
may be rejected until the pod has started once. If your cluster does not allow
a controller to write CRDs, install it yourself and turn the behaviour off:

```bash
kubectl apply -f internal/crd/assets/     # or `make install-crds`
helm install sandman ... --set crds.install=false --set crds.rbac=false
```

## 🕸 Networking

**Sandman runs with host networking, and needs to.** A magic packet is a
link-local broadcast: sent from inside the pod network it reaches the CNI bridge
and stops there. Host networking puts it on the wire the machines are actually
on.

Two consequences worth knowing about:

- On a host with more than one network — a storage network, a second NIC facing
  the machines — set `controller.wolSourceAddress` to an address on the
  machines' network, or the kernel picks an interface and the packet leaves by
  the wrong door.
- Because the port is a host port, replicas cannot share a node. The chart
  refuses `replicaCount > 1` with host networking rather than letting the second
  pod sit `Pending` forever.

Sandman should also run somewhere it will not switch off underneath itself. A
`nodeSelector` pinning it to a control-plane node is the usual answer.

## 📊 Metrics

Everything is prefixed `sandman_`. The gauges are read from the controller's
cache at scrape time, so a deleted device takes its series with it rather than
freezing at a last value.

| Metric | Description |
| --- | --- |
| `sandman_device_power_state` | 1 on the state that applies, per device and state |
| `sandman_device_desired_power_state` | the same for what was requested |
| `sandman_device_node_found` / `_node_ready` / `_node_unschedulable` | the cluster's view of the node |
| `sandman_device_phase_since_timestamp_seconds` | when the current state was entered |
| `sandman_device_transition_deadline_timestamp_seconds` | when the transition in flight gives up; absent when nothing is in flight |
| `sandman_device_last_started_` / `_last_stopped_` / `_last_wake_` / `_last_shutdown_timestamp_seconds` | when things last happened |
| `sandman_device_wake_attempts` | packets sent during the start in flight |
| `sandman_wake_packets_sent_total` / `sandman_wake_attempts_total` | packets and attempts, by outcome |
| `sandman_shutdown_attempts_total` | shutdown pods created, by outcome |
| `sandman_pod_evictions_total` / `sandman_drain_duration_seconds` | what draining cost |
| `sandman_power_transitions_total` | completed transitions by direction and outcome |
| `sandman_power_transition_duration_seconds` | how long a start or stop took to settle |
| `sandman_api_requests_total` / `sandman_api_request_duration_seconds` | REST traffic by route |
| `sandman_device_collect_failures_total` | 1 when the last scrape could not read the devices |
| `sandman_build_info` | the running build |

`controller_runtime_*` and `workqueue_*` come along on the same endpoint.

The chart's alerts cover the ones worth waking up for — a device `Failed`, a
machine `Stopped` while something wants it `On`, magic packets that cannot be
sent, shutdown pods that cannot be created — and the dashboard shows the fleet,
a table of every machine, boot durations, and the controller's own loop. Turn
them on with:

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

`sandman_power_transition_duration_seconds{transition="start"}` is the one to
watch over time: it is honest hardware truth — POST, boot and the kubelet's
first heartbeat — and it is what tells you whether "wake the machine for the
batch job" is a ninety-second operation or a five-minute one.

## 🔐 What it can do to your cluster

Worth reading before installing, because the answer is "quite a lot":

- **It creates privileged pods.** One per stop, on the node being stopped, with
  `hostPID` and `privileged: true`. That is the whole mechanism. They live in
  `shutdown.namespace` (the release namespace by default), which must allow
  privileged pods — set `shutdown.namespace` and `shutdown.createNamespace` to
  keep them out of the namespace everything else runs in.
- **It evicts pods.** Through the eviction API, so budgets are honoured. A drain
  that cannot finish is abandoned, never forced: whatever is refusing to move is
  refusing for a reason, and powering the machine off anyway would override it
  in the most destructive way available.
- **It cordons and uncordons nodes.** It never deletes them — a node object
  outliving its machine is sandman's evidence that the machine is off.
- **Its own pod is unprivileged**: read-only root filesystem, no capabilities,
  non-root. Broadcasting needs none of them.
- **An unauthenticated API can turn your machines off.** Set a token.

## 🛠 Development

```bash
make help          # every target
make check         # fmt, vet, lint, CRD drift check, tests with -race
make run           # against the cluster in your kubeconfig
make manifests     # regenerate the embedded CRD after editing api/
make helm-template # render the chart with everything switched on
```

The CRD in `internal/crd/assets/` is generated and embedded — there is exactly
one copy on purpose, and `make ci-lint` fails if it has drifted from the Go
types in `api/v1alpha1`.

Where things live:

| Path | What |
| --- | --- |
| `api/v1alpha1` | the `SandmanDevice` types, and the defaults the CRD mirrors |
| `internal/power` | the reconciler: the state machine, draining, shutdown pods |
| `internal/wol` | magic packets |
| `internal/server` | the REST API |
| `internal/metrics` | instruments and the device collector |
| `internal/crd` | the embedded CRD and its installer |
| `deployments/helm/sandman` | the chart |

## 📜 License

GPL-3.0-only. See [LICENSE](LICENSE).
