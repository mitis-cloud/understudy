# Understudy

[![Release](https://img.shields.io/github/v/release/kylan11/understudy?style=for-the-badge&label=release&color=1f6feb)](https://github.com/kylan11/understudy/releases)
[![Last commit](https://img.shields.io/github/last-commit/kylan11/understudy?style=for-the-badge&color=2ea043)](https://github.com/kylan11/understudy/commits/main)
[![Build](https://img.shields.io/github/actions/workflow/status/kylan11/understudy/ci.yaml?branch=main&style=for-the-badge&label=build)](https://github.com/kylan11/understudy/actions/workflows/ci.yaml)
[![Go version](https://img.shields.io/github/go-mod/go-version/kylan11/understudy?style=for-the-badge)](go.mod)
[![License](https://img.shields.io/github/license/kylan11/understudy?style=for-the-badge)](LICENSE)
[![Stars](https://img.shields.io/github/stars/kylan11/understudy?style=for-the-badge&color=e3b341)](https://github.com/kylan11/understudy/stargazers)

**Best-effort high availability with one replica.** Understudy keeps
single-replica workloads serving through node drains, upgrades, consolidation
and spot reclaims, without paying for a second replica around the clock. Best
effort, spelled out: no downtime from disruptions that announce themselves,
bounded downtime when a hard deadline cannot be won, and no protection against
unannounced hardware death. That last one is what a second replica is for, and
nothing changes it.

In theatre the understudy waits in the wings and steps in when the lead exits.
This operator does the same for a single-replica Deployment: when a node is
going away, it brings up a stand-in pod, waits until that pod is genuinely
serving traffic, and only then lets the original leave. The second pod exists
for the couple of minutes a disruption is actually in flight.

```yaml
apiVersion: apps.understudy.sh/v1alpha1
kind: Understudy
metadata:
  name: tenant-worker
spec:
  targetRef:
    kind: Deployment
    name: tenant-worker
  hostageMode: always
  minSurgeTimeSeconds: 60
  readinessDeadlineSeconds: 600
```

## Why not just run two replicas

Two replicas is rent paid every hour of every day against an event that lasts
minutes a month. It also halves your capacity headroom during every drain, and
for workloads that are singletons by nature (leader-elected controllers,
schedulers, queue consumers with sticky assignment) the second replica is idle
spend that buys nothing.

Understudy's bet is narrower: your workload can tolerate two pods for a few
minutes. That is the same property a rolling update with `maxSurge: 1` already
requires, so if you deploy without downtime today, you qualify. The second pod
then exists only while a disruption is happening.

## What it survives

| Kind of disruption | Examples | Time available |
|---|---|---|
| Voluntary | node drain, Karpenter drift and consolidation and expiry, node pool upgrades, manual `kubectl drain` | unbounded, the eviction is held |
| Involuntary | spot and preemptible reclaim, host maintenance | fixed deadline, seconds to minutes |
| Deploys | rolling update of the workload itself | not our business, Understudy stands down |

## Results

Measured on EKS with Karpenter on arm64 spot nodes, one replica behind an
application load balancer, with no spare capacity so every replacement waited
for a new EC2 instance. The availability probe ran at roughly three requests
per second. The disruption results below are from the v0.3.x PDB mechanism;
v0.4.0 separately validated the admission path on the same staging cluster:
same-node stand-ins remained held, a clear-node stand-in released immediately,
the 10-second TTL released in 12 seconds, and operator-down fail-open admitted
in 423 ms.

| Event | Failed requests |
|---|---|
| Nothing happening (control) | 0 of 120 |
| `kubectl drain` | 1 of 350 |
| Karpenter disruption | 1 of 189 |
| Real AWS spot interruption | 1 of 764 |
| Ordinary rolling update, no Understudy involved (control) | 2 of 38 |

The last row is the important one. The handful of requests lost during a
disruption are lost as the old pod exits, and an ordinary deploy of the same
workload loses more. Understudy makes a node disruption cost about what a
routine deploy costs.

## Install

```sh
helm install understudy oci://ghcr.io/kylan11/charts/understudy \
  --namespace understudy-system --create-namespace
```

Or from a checkout:

```sh
helm install understudy ./charts/understudy \
  --namespace understudy-system --create-namespace
```

Then protect a workload by creating an `Understudy` next to it. `kubectl get
understudies` shows the target, the mode, the phase, and how long anything has
been blocked.

The footprint of the default install is one Deployment with one replica. The
webhook is served from that same pod. There is no DaemonSet and no per-node
agent. Drains, Karpenter disruption, autoscaler scale-downs and node upgrades
are all detected from the API server by the operator pod alone, and on
Karpenter clusters that includes spot interruptions, because Karpenter reacts
to the interruption queue by tainting the node and the operator sees the
taint.

On clusters where every node carries a taint, the operator needs a matching
toleration and node selector, the same as any other controller you run there.

### Cloud sentinels (optional, off by default)

Everything above works without this section. Skipping it is a reasonable
choice, and if you do, the one thing to remember is to set
`readinessDeadlineSeconds` below your platform's spot notice window, so the
operator never holds an eviction against a deadline it cannot see.

What the sentinel adds is the deadline itself. Without it, a spot interruption
reaches the operator as an ordinary drainer taint, indistinguishable from an
unbounded drain. The sentinel reads the cloud's termination notice on the
node and passes along how long is actually left, which is what lets the
operator choose between holding the eviction and releasing it immediately. It
also shaves about 15 seconds off the reaction time compared to the queue path.

It is a DaemonSet only because every cloud serves termination notices on a
link-local metadata endpoint that nothing outside the instance can read.
Scope it to the node pools that can actually be preempted; it has no business
running anywhere else.

```sh
helm upgrade understudy ./charts/understudy \
  --set sentinel.enabled=true \
  --set sentinel.cloud=aws \
  --set 'sentinel.nodeSelector.karpenter\.sh/nodepool=spot'
```

The AWS probe has been validated against real spot interruptions. The GCP and
Azure probes are covered by unit tests only. Treat them as experimental until
someone runs them on GKE and AKS.

## Configuration

| Field | Default | What it does |
|---|---|---|
| `targetRef.name` | required | The Deployment to protect, in the same namespace |
| `surgeReplicas` | 1 | How many stand-ins to add |
| `minSurgeTimeSeconds` | 60 | How long this workload realistically takes to start serving. If a termination deadline is nearer than this, the eviction is released immediately instead of being held |
| `readinessDeadlineSeconds` | 600 | How long to hold an eviction with no progress before giving up and letting it through |
| `hostageMode` | `always` | `always`, `voluntary-only`, or `off` |

If you run on spot without the sentinel, set `readinessDeadlineSeconds` below
the platform's notice window, otherwise the operator can hold an eviction
against a deadline it cannot see.

## How it works

Briefly: answer the eviction at the door with HTTP 429, surge when its node is
reported doomed, admit a retry once the replacement passes its readiness
gates, and scale back by removing the right pod. No PodDisruptionBudget is
created. The full explanation, with diagrams and the reasoning behind each
decision, is in
[docs/how-it-works.md](docs/how-it-works.md).

Signals reach the operator through four adapters, and the core contains no
provider-specific code:

| Adapter | Sees |
|---|---|
| Cordon watch | `kubectl drain`, GKE and AKS and EKS upgrades, cluster-autoscaler, kured |
| Taint watch | Karpenter, cluster-autoscaler, plus any key you configure |
| Eviction webhook | every eviction attempt from every drainer, including ones nothing else detects |
| Cloud sentinel (optional, off by default) | spot and preemptible notices, with their deadlines |

## Safety

Alert on `understudy_oldest_blocked_eviction_seconds`. It exists so that a held
eviction is always visible and bounded.

If a surge cannot make progress, the hold is relaxed rather than stalling the
drain forever. The webhook is fail-open: if every operator replica is
unavailable, Kubernetes admits evictions normally within the webhook timeout.
To disable holds immediately:

```sh
kubectl delete validatingwebhookconfiguration understudy-eviction-hold
```

The operator refuses to manage itself, skips targets it cannot help, and stands
down while a rollout is in progress.

For production, use `replicaCount: 2` and place the replicas on different
nodes. The webhook serves from every replica even though reconciliation is
leader-gated. If the node hosting the only operator pod is drained, its own
eviction is admitted and protection for pods later in that drain can disappear.

## Limits and the tradeoff

Understudy reacts to disruptions; it never starts one. Because there is no
budget for a disrupter to pre-check, consolidation, drift and expiry can select
the node normally. The disrupter begins, retries the admission hold while the
stand-in starts, and proceeds as soon as the stand-in is ready.

**In-flight requests are not saved.** Understudy guarantees a ready
replacement, not the requests already travelling to the departing pod. Give
the pod a `preStop` hook that outlasts your load balancer's deregistration
delay; no surge mechanism can do it for you.

**An involuntary deadline is a race against the platform.** AWS gives about
two minutes, which a fast-booting workload can win even when a new node has
to be provisioned first. GCP and Azure give about thirty seconds, which
cannot be won cold, so there the value is an orderly release and an early
replacement rather than zero downtime.

**Workloads that cannot run two instances at once are out of scope**,
including single-replica StatefulSets, and always will be.

## Development

```sh
make test    # envtest suite, downloads Kubernetes binaries on first run
make lint
make build
```

## License

Apache 2.0
