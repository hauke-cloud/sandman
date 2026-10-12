<!-- llm-readme-management spec=1 commit=857bfb11639b5af244ce0478f8f0b24602658be8 template=terraform model=qwen3.8-27b-q4 digest=f68682b55584 generated=2026-10-12T11:40:40Z -->
<a href="https://hauke.cloud" target="_blank"><img src="https://img.shields.io/badge/home-hauke.cloud-brightgreen" alt="hauke.cloud" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud" target="_blank"><img src="https://img.shields.io/badge/github-hauke.cloud-blue" alt="hauke.cloud Github Organisation" style="display: block;" /></a>
<a href="https://github.com/hauke-cloud/llm-readme-management" target="_blank"><img src="https://img.shields.io/badge/template-terraform-orange" alt="Repository type - terraform" style="display: block;" /></a>


# sandman


<img src="https://raw.githubusercontent.com/hauke-cloud/.github/main/resources/img/organisation-logo-small.png" alt="hauke.cloud logo" width="109" height="123" align="right">


<llm header hint="Say whether this is a reusable module or a root module that owns real state.">

A Go service and Helm chart that wakes and powers off the physical machines behind a Kubernetes cluster's nodes via Wake-on-LAN and kubelet-driven shutdown. It is a root deployment, not a reusable module, for you if you operate a small bare-metal or homelab cluster and do not need every machine running all the time.

</llm>


## :book: Description

<llm description>

A cluster of physical machines does not need every machine running all the time. Sandman is a Kubernetes controller that powers bare-metal nodes on and off over the network. You describe each machine with a `SandmanDevice` custom resource; the controller handles the rest.

Starting a machine sends Wake-on-LAN magic packets (with optional SecureOn) and retries until the node reports Ready. Stopping a machine cordons it, drains workloads through the eviction API so PodDisruptionBudgets are honoured, then runs `systemctl poweroff` in the host's namespaces via a short-lived privileged pod. A REST API under `/api/v1` lets you issue start, stop, and status requests by device name; it returns immediately and never blocks on hardware.

- Wake-on-LAN start with configurable packet count, retry interval, and SecureOn password
- Graceful stop: cordon → drain (PDB-aware, never forced) → `systemctl poweroff`
- REST API with bearer-token authentication and read-only mode
- Prometheus metrics, alerting rules, and a Grafana dashboard
- Helm chart at `oci://ghcr.io/hauke-cloud/charts/sandman`

The chart lives in the `hauke-cloud` organisation's OCI registry and is built by the shared `hauke-cloud/inpacken-un-af-dor-mit` CI action.

</llm>


## :clipboard: Requirements

<llm requirements hint="Give the Terraform version from .terraform-version and the provider constraints from versions.tf, plus the credentials the providers need.">

- Go 1.27.0 (pinned in `go.mod`)
- A running Kubernetes cluster and a kubeconfig with the ClusterRole permissions the chart creates (CRD, node, pod, eviction, secret, event, and lease access)
- A container engine (docker or podman); `docker buildx` for multi-arch image builds
- `helm` and `kubectl`
- golangci-lint v2.13.2 and controller-gen v0.22.0 (fetched via `go run`; a Go module proxy is needed for first runs)
- Terraform 1.9 (`.terraform-version`) or OpenTofu 1.8.0 (`.opentofu-version`) for the deployment workflow template
- Wake-on-LAN-capable NICs on every target machine, on a network segment reachable from the node running sandman
- Target hosts running systemd (shutdown invokes `systemctl poweroff`)
- A namespace that admits privileged pods (Pod Security admission) for the short-lived shutdown pods
- The `busybox:1.37` image (or your `shutdown.image`) pullable on the nodes being stopped
- An API token (`api.token` or `api.existingSecret`) if the Service is reachable beyond localhost
- Optional: Prometheus Operator CRDs (ServiceMonitor, PrometheusRule), grafana-k8s-sidecar, or a Gateway API installation

</llm>


## 🚀 Getting started

<llm getting_started hint="terraform init, plan and apply, with the backend configuration the repository actually uses. Say plainly if apply touches real infrastructure.">

This repository does not contain Terraform or OpenTofu modules. The `.terraform-version` and `.opentofu-version` files are org-template boilerplate; there are no `.tf` files, no state, and no providers. The deployment path is Helm.

1. Clone the repository.

```bash
git clone https://github.com/hauke-cloud/sandman.git
cd sandman
```

2. Install the Helm chart into your cluster. The controller installs its own CRD on first start, so no separate CRD step is needed.

```bash
helm install sandman oci://ghcr.io/hauke-cloud/charts/sandman --namespace sandman --create-namespace
```

3. Create a `SandmanDevice` for a physical machine. The examples file documents the required fields (node name, MAC address, desired power state).

```bash
kubectl apply -f examples/sandmandevice.yaml
```

4. Forward the API port to your local machine.

```bash
kubectl -n sandman port-forward svc/sandman 8080:8080
```

5. Query the device status to confirm the controller is reconciling.

```bash
curl -s localhost:8080/api/v1/devices/worker-01/status
```

The API is unauthenticated by default. If the Service is reachable beyond your local port-forward, set `api.token` or `api.existingSecret` in the Helm values before exposing it.

</llm>


## :airplane: Usage

<llm usage hint="For a reusable module, the central example is a module block with source, version and the required variables filled in from variables.tf. For a root module, show the workflow instead.">

Once the chart is deployed, you manage physical machines through `SandmanDevice` custom resources and the REST API.

**Install the chart**

```bash
helm install sandman oci://ghcr.io/hauke-cloud/charts/sandman \
  --namespace sandman --create-namespace
```

**Define a device**

Each physical machine gets one cluster-scoped `SandmanDevice`. The minimal spec requires the node name and a Wake-on-LAN MAC address:

```yaml
apiVersion: sandman.hauke.cloud/v1alpha1
kind: SandmanDevice
metadata:
  name: worker-01
spec:
  nodeRef:
    name: worker-01
  desiredPowerState: Running
  wakeOnLAN:
    macAddress: "AA:BB:CC:DD:EE:FF"
```

Apply it with `kubectl apply -f device.yaml`. The controller sends magic packets to the broadcast address (default `255.255.255.255:9`) and waits for the node to report Ready.

**Control power via the API**

Port-forward the service, then issue start or stop requests. The API returns `202 Accepted` immediately and never blocks on hardware.

```bash
kubectl -n sandman port-forward svc/sandman 8080:8080
```

```bash
curl -X POST localhost:8080/api/v1/devices/worker-01/stop
curl -X POST localhost:8080/api/v1/devices/worker-01/start
curl -s localhost:8080/api/v1/devices/worker-01/status
```

A device in the `Failed` phase does not retry on its own; re-issue the same request to re-arm it.

</llm>


## :wrench: Configuration

<llm configuration hint="A table of the variables in variables.tf: name, type, default, required. Point at variables.tf for the full set and mention outputs.tf if it exists.">

Despite the presence of `.terraform-version` (1.9) and `.opentofu-version` (1.8.0), this repository contains no Terraform or OpenTofu modules. Configuration is resolved in the order CLI flag > environment variable > compiled-in default. The table below covers the binary-level settings; the full list is documented in `.env.example` and implemented in `internal/config/config.go`.

| Name | Default | Description |
|------|---------|-------------|
| `LISTEN_ADDRESS` | `:8080` | Address the HTTP server binds to (required) |
| `API_TOKEN` | *(empty)* | Bearer token for the REST API; empty means unauthenticated |
| `API_TOKEN_FILE` | *(empty)* | Path to a file containing the token; overrides `API_TOKEN` |
| `API_READ_ONLY` | `false` | Refuse all state-changing routes |
| `POD_NAMESPACE` | `sandman` | Namespace the controller watches (required) |
| `INSTALL_CRDS` | `true` | Apply the embedded CRD at startup |
| `LEADER_ELECTION` | `true` | Enable leader election |
| `WOL_SOURCE_ADDRESS` | *(empty)* | Egress IP for magic packets on multi-NIC hosts |
| `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `text` | Log format: `text` or `json` |

Per-device behaviour (wake-on-LAN parameters, drain options, shutdown mode, timeouts) is set on each `SandmanDevice` CR; defaults are declared as kubebuilder markers in `api/v1alpha1/sandmandevice_types.go`. Deployment-level settings (image, RBAC

</llm>


## :hammer: Development

<llm development hint="Cover terraform fmt, validate, tflint and terraform-docs where the repository configures them.">

Before pushing, run the full local check:

```bash
make check
```

This runs `gofmt -s`, `go vet`, golangci-lint (pinned v2.13.2), the CRD drift check, and `go test -race -count=1 ./...`. CI executes the same lint set via `make ci-lint` and tests with `go test -v -race -coverprofile=coverage.out -covermode=atomic ./...`.

If you change types under `api/v1alpha1/`, regenerate the deepcopy and the embedded CRD before committing:

```bash
make generate
make manifests
```

`make manifests-check` (part of `ci-lint`) fails the build if `internal/crd/assets/sandman.hauke.cloud_sandmandevices.yaml` has drifted from the Go types.

Install the pre-commit hooks after cloning:

```bash
pre-commit install
```

They run pre-commit-hooks (v4.4.0) and gitleaks (v8.18.0) on every commit. Run them across the whole tree with `pre-commit run --all-files`.

PR titles are validated by a workflow: the type must be `fix`, `feat`, `docs`, `ci`, or `chore`, and the subject must start with an uppercase letter. A `[WIP]` prefix is accepted.

</llm>


## 📄 License

This Project is licensed under the GNU General Public License v3.0

- see the [LICENSE](LICENSE) file for details.


## :coffee: Contributing

To become a contributor, please check out the [CONTRIBUTING](CONTRIBUTING.md) file.


## :email: Contact

For any inquiries or support requests, please open an issue in this
repository or contact us at [contact@hauke.cloud](mailto:contact@hauke.cloud).
