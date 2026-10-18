# Installation and CI

[Project README](../README.md) | [Documentation index](README.md)

## Build and test

Prerequisites: Go 1.24.7+, [Task](https://taskfile.dev/) for project commands,
Docker for image builds, Helm 3, kubectl, and Python 3 for the optional CI
client. Linux nodes need containerd with overlayfs and node-exporter listening
on node port 9100. KubeWarm targets Kubernetes 1.30+.

```bash
task test
task build
task images
helm lint helm/kubewarm
```

Install and exercise a disposable cluster using the [runbook](#runbook).
On Windows, run the commands through WSL2 with Docker Desktop integration.


## Example CI request

Replace the placeholder with your actual image digest before submitting:

```json
{
  "image": "registry.example.com/payments@sha256:REPLACE_WITH_REAL_DIGEST",
  "nodeSelector": {"workload": "payments"},
  "minReady": 3,
  "zones": ["eu-west-1a", "eu-west-1b", "eu-west-1c"],
  "minReadyPerZone": 1,
  "timeoutSeconds": 300
}
```

```bash
export KUBEWARM_URL=http://127.0.0.1:8080
# Set KUBEWARM_TOKEN securely to the CI token, not the worker token.
python3 scripts/warm-and-wait.py --request request.json
# Deploy the SAME image digest only after the client exits successfully.
```

`POST /v1/warmings` returns 202 with a job ID and Location header.
`GET /v1/warmings/{id}` returns the request, state, ready nodes, eligible count,
zone counts and last synchronous worker errors. Both require the CI bearer token.
Invalid requests return 400; unknown/restarted/expired jobs return 404.
A missing token fails closed. Only these two endpoints are the job API.

States: `Queued` → `Running` → `Ready` or `TimedOut`. Transient errors retry until
the deadline; `Failed` is reserved for future terminal-error classification.
An impossible node/zone threshold waits until timeout, allowing autoscaling to
supply nodes. No nodes are provisioned by KubeWarm. A Ready result is a snapshot
at completion; it is not revalidated after terminal completion.


<a id="runbook"></a>

## Installation and testing runbook

Run this lab on Linux or WSL2 with Docker, kind, Helm, kubectl, Go and Python.
Commands assume the repository root. They create a disposable cluster named
`kubewarm`; do not point lab commands at a production context.

### 1. Unit tests and builds

```bash
task test
task build
task images
kind create cluster --name kubewarm --config examples/kind.yaml --image kindest/node:v1.31.0
kubectl config use-context kind-kubewarm
kind load docker-image kubewarm-controller:0.2.0 kubewarm-worker:0.2.0 --name kubewarm
```

The kind version/node image combination must be supported by your installed kind.
Inspect `/etc/containerd/config.toml` inside a kind node: overlayfs is required.
The lab disables mirror writes; production mirror installation requires the
registry `config_path` described in `#node-settings`.

### 2. Node exporter

The worker uses node-exporter filesystem metrics, not metrics-server.
Install an exporter on every node (including control-plane nodes):

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm upgrade --install node-exporter prometheus-community/prometheus-node-exporter \
  --namespace monitoring --create-namespace \
  --set hostNetwork=true --set hostPID=true
kubectl -n monitoring rollout status daemonset/node-exporter-prometheus-node-exporter
```

If your chart release names its DaemonSet differently, locate it with
`kubectl -n monitoring get daemonsets` and use that name. Confirm one exporter
per node and port 9100 reachable from the pod network.

### 3. Secrets and installation

Generate files so tokens do not appear as command-line arguments:

```bash
kubectl create namespace kubewarm
mkdir -p secrets
chmod 700 secrets
python3 -c 'import secrets,pathlib; p=pathlib.Path("secrets"); (p/"worker-token").write_text(secrets.token_hex(32)); (p/"ci-token").write_text(secrets.token_hex(32))'
chmod 600 secrets/*
kubectl -n kubewarm create secret generic kubewarm-rescue-token --from-file=token=secrets/worker-token
kubectl -n kubewarm create secret generic kubewarm-webhook-token --from-file=token=secrets/ci-token
helm upgrade --install kubewarm ./helm/kubewarm --namespace kubewarm \
  --set config.MIRROR_ENABLED=false \
  --set config.GC_ENABLED=false
kubectl -n kubewarm rollout status deployment/kubewarm-controller
kubectl -n kubewarm rollout status daemonset/kubewarm-worker
kubectl -n kubewarm get pods
```

The lab disables cleanup to keep the experiment isolated from disk reclamation.
Check worker/controller logs if readiness or reporting fails. No registry Secret
is needed for public images. Private images use a dockerconfigjson Secret named
`registry-pull` (or `registryCredentials.secretName`); a workload's own
imagePullSecrets do not configure these workers. For ECR, provide/refresh supported
registry credentials; automatic EKS Pod Identity credential exchange is not added.

### 4. Create an exact-digest request

Get a real Linux/amd64 image digest from your own build/push output. For a public
lab image, inspect an image you explicitly pull:

```bash
docker pull --platform linux/amd64 nginx:1.27-alpine
docker image inspect nginx:1.27-alpine --format '{{index .RepoDigests 0}}'
```

Use that `repository@sha256:...` value in `examples/warming.json`. Prefer the
platform-specific manifest digest; keep the same digest throughout your rollout.
In one terminal:

```bash
kubectl -n kubewarm port-forward service/kubewarm-controller 8080:8080
```

In another terminal:

```bash
export KUBEWARM_TOKEN="$(cat secrets/ci-token)"
python3 scripts/warm-and-wait.py --request examples/warming.json
```

Expected: 202 submission, Running, then Ready with one `workload=api` node per
zone. `workload=gpu` must not be targeted by this request. Inspect node inventories
using `docker exec <kind-node-name> crictl images --digests` and worker logs. Do not
call the old global webhook during the targeted test: it intentionally targets a
broader set of nodes.

### 5. Continuous warming and a newly eligible node

Replace the digest in `examples/policies-values.yaml`, then:

```bash
helm upgrade kubewarm ./helm/kubewarm -n kubewarm \
  -f examples/policies-values.yaml \
  --set config.MIRROR_ENABLED=false --set config.GC_ENABLED=false
kubectl label node kubewarm-worker3 workload=api --overwrite
```

After ConfigMap projection plus a reconcile interval, worker3 should receive the
image. This exercises newly matching-node detection. To test actual autoscaling,
use an EKS node group or Karpenter node with matching labels and ensure its worker
is reporting. KubeWarm does not create or gate nodes. Avoid relying on kind to add
nodes dynamically; label changes provide the local equivalent eligibility test.

### 6. Failure cases

- Remove the Authorization header: the new API returns 401.
- Submit `nginx:latest` instead of a digest: 400.
- Request a nonexistent zone or more ready nodes than exist: Running then TimedOut.
- Stop reporting from a node before job completion: it is excluded once stale.
- Restart the controller during a job: GET returns 404; resubmit. Policies recover.
- Make a peer unavailable: targeted transfer falls back to `/pull`; final readiness
  still requires inventory confirmation. A nonexistent digest eventually times out.
- Verify local-zone preference with the automated test, then in a cluster with two
  source nodes holding the same digest, one in the receiver's zone. Observe source
  selection in worker logs; local-zone labels alone do not simulate actual AZ costs.

### 7. Production adaptation

Push both built images to your registry and set controller.image.repository and
worker.image.repository accordingly. Use tag 0.2.0 or your own build tag. Install
only one KubeWarm release per cluster (resource names are fixed). Configure registry
authentication, private TLS API access, node selectors, real zones, and mirror
registries. Review worker privileges and cleanup settings. Run the same failure
cases on a pilot cluster. KubeWarm is single-controller and jobs are not durable.

### 8. Remove the lab

```bash
helm uninstall kubewarm -n kubewarm
kind delete cluster --name kubewarm
```

Delete the locally generated token files when no longer needed; never commit them.

