# Architecture

[Project README](../README.md) | [Documentation index](README.md)

<a id="architecture"></a>

This guide describes what runs where, what it knows, and how an image moves
from a CI push to cleanup.

KubeWarm has two components. Both are static Go binaries with no external
Go dependencies.

| Component | Runs as | Responsibilities |
|---|---|---|
| **Controller** | Deployment, 1 replica | Lists node metadata, reconciles targeted policies and CI jobs, receives the preheat webhook, tracks every node's disk usage, images and layers, chooses seed nodes, schedules node-to-node transfers, watches for pods stuck on pulls, and tells mirrors which peer holds a blob. |
| **Worker** | DaemonSet, one per node | Reports its node, pulls when ordered, sends and receives images, runs the node's mirror, and cleans up the node's images. |

### Reporting and state

Each worker posts a report to the controller every `REPORT_INTERVAL_S` (15s),
and right away when containerd reports a pull or removal. A report has:

- root filesystem utilization, read from the local node-exporter;
- every image reference on the node;
- its **layer inventory**: the blob digests and snapshot chainIDs it holds,
  sent as numbered deltas so reports stay small on large nodes.

The controller keeps all state in memory and treats a worker that has been
silent for `WORKER_STALE_AFTER_S` as gone. It has no database or persistent
volume: after a restart it rebuilds its view from the next round of reports,
and each worker resends its full layer inventory once the controller no
longer holds the version its deltas build on.

Workers keep two small files on the node under `RESCUE_STATE_DIR`: snapshots
pinned for a retry, and image usage times for cleanup.

### An image's life

```mermaid
sequenceDiagram
    participant CI
    participant C as Controller
    participant S as Seed workers
    participant W as Other workers
    participant R as Registry

    CI->>R: push app:1.2.3
    CI->>C: POST /webhook/preheat
    C->>R: read manifest and config (a few KB)
    C->>S: POST /spread/fetch: one missing blob each
    S->>R: pull that blob
    S->>C: POST /spread/done
    loop every blob, as soon as any node has it
        C->>W: POST /spread/fetch from a peer
        W->>S: GET /spread/content/<digest>
        W->>C: POST /spread/done
    end
    C->>W: POST /spread/finalize (metadata)
    Note over W: image registered; kubelet finds it locally
```

1. **[Preheat and spread](propagation.md#propagation).** Missing blobs are pulled from
   the registry about once each, and every blob spreads from node to node
   the moment any node has it, until every eligible node has registered the
   image. Pushes whose manifest can't be read are [seeded](preheat.md#preheat) and
   [propagated](propagation.md#propagation) whole instead.
2. **[Mirror](mirror.md#mirror).** Any pull containerd still makes, for example on a
   node that joined later or for a third-party image, is served from the node
   or a peer when possible.
3. **[Rescue](rescue.md#rescue).** If a pod still gets stuck pulling the image, it is
   copied to that node from a node that has it.
4. **[Image cleanup](image-cleanup.md#image-cleanup).** Once nothing has used it on a node
   for 6 hours, or sooner when the disk runs high, the node removes it.

Whole-image propagation and rescue share one
[transfer mechanism](transfers.md#transfers); spread moves single blobs by digest.

### Talking to containerd

The worker image contains only the worker binary. Everything it does with
images goes through containerd's socket: its own gRPC API for content,
images, snapshots, leases and diffs, and CRI for seed pulls and container
listings, the calls `crictl` makes. Spread's registry pulls stream single
blobs by digest straight into the content store, with the same pull
secret; a whole-image seed pull is therefore the exact pull
kubelet would make. Work that needs privileges on the node (unpacking
layers, mounting snapshots, writing files with their owners and whiteouts)
happens inside containerd, so the worker has no capabilities and no host
PID namespace; see [Security](security.md#security).
Listings run one at a time, and each result is shared between callers
for up to 15 seconds. Blob content is read directly from containerd's
content store when it is mounted, and through the content API otherwise.

### Scope

- **It doesn't resolve tags.** The mirror serves content by digest only.
  Registry outages that block tag resolution are covered by rescue.
- **It doesn't replace kubelet's image GC.** kubelet still acts at disk
  pressure as the last line of defense. KubeWarm's cleanup runs well before
  it.
- **It doesn't restart pods.** After a rescue, kubelet's next retry finds the
  image.

## Configuration and boundaries

- Install via `helm/kubewarm`. `k8s/` contains reference manifests,
  not the complete installation. The chart uses one controller and Recreate
  rollout strategy to avoid two controllers issuing work concurrently.
- `warmingPolicies` is a list of `{name, image, nodeSelector}` in Helm values.
  Exact label equality is supported; match expressions and taint/toleration
  simulation are not. Policies apply across zones unless labels restrict them.
- Node discovery polls the API every 10 seconds, following pagination. The
  controller has list-only node permission in addition to pod access.
  Failed node discovery pauses targeting until refreshed. Invalid policy JSON
  pauses policy warming; active CI jobs are independent.
- CI jobs are in memory, capped at 1,000, and retained for one hour after
  completion. Controller restarts lose jobs; clients fail on 404 and must resubmit.
  Continuous policies recover on restart. HA, persistent jobs and idempotency
  keys are not implemented.
- Four concurrent controller transfer requests; retry cooldown is one minute
  per node/image. Asynchronous registry pulls can outlive those requests. This
  is not a global containerd bandwidth/concurrency limiter.
- New downloads pause at 70% reported disk utilization. Existing cached images
  can still satisfy readiness above that threshold. Image cleanup is
  enabled by default; continuous policies do not pin images against GC.
- Job completion stops further scheduling for that job; already started
  transfers may finish. A request targets currently matching nodes dynamically.
- Policies do not delay scheduling, taint nodes, scale Karpenter, or guarantee
  that new-node warming finishes before the first pod is scheduled.
- Target selection applies to KubeWarm job/policy recipients. It does not impose
  isolation on image recovery, application image pulls, or the global webhook.
- Use
  a homogeneous Linux/amd64 cluster and matching image digests; multi-architecture
  readiness accounting is not implemented.
- Mirror fallback, registry authentication and `imagePullPolicy` still matter.
  Configure `MIRROR_REGISTRIES` explicitly and review node settings.

