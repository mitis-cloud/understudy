# How Understudy works

Understudy protects a single-replica Deployment during announced node
disruptions without keeping a second replica running all the time.

## The mechanism

Five steps happen in order.

**1. Answer the eviction at the door.** The always-on validating webhook
handles `pods/eviction` CREATE. For a protected pod with no ready stand-in it
returns HTTP 429 and `TooManyRequests`. Drainers already retry that response.
No PodDisruptionBudget is created, so Karpenter and cluster-autoscaler can
select a node without being blocked by a budget pre-check.

**2. Record the node as doomed.** Cordon, taint, eviction and optional cloud
sentinel adapters normalize their observations into a `NodeDoom`.

**3. Surge.** The controller increases the Deployment replica count and marks
the old pod with a negative `controller.kubernetes.io/pod-deletion-cost`.

**4. Wait for traffic readiness.** A stand-in counts only when it is Ready, is
not deleting, and runs on neither the victim node nor another doomed node.
Readiness gates therefore include load balancer registration. Once enough
healthy stand-ins exist, the webhook admits the drainer's next retry.

**5. Scale back.** After the old pod leaves, the Deployment returns to its
original replica count. The deletion cost makes the old pod the preferred
scale-down target.

```mermaid
sequenceDiagram
    participant D as Drainer
    participant K as API server
    participant U as Understudy
    participant W as Workload

    D->>K: evict pod
    K->>U: admission review
    U-->>K: 429, stand-in not ready
    K-->>D: retry later
    Note over U: node recorded as doomed
    U->>W: replicas 1 to 2
    W-->>U: stand-in Ready, including readiness gates
    D->>K: evict pod (retry)
    K->>U: admission review
    U-->>K: admit
    K-->>D: accepted
    U->>W: replicas back to 1
```

## Signals

The core consumes one provider-neutral value:

```go
NodeDoom{
    Node      string
    Class     VoluntaryUnbounded | Involuntary
    Deadline  *time.Time
    ExpiresAt *time.Time
    Source    string
}
```

| Adapter | Signal |
|---|---|
| Cordon watch | `spec.unschedulable`, including `kubectl drain` and many managed upgrades |
| Taint watch | Karpenter, cluster-autoscaler and configured drainer taints |
| Eviction webhook | Every matching eviction attempt, including eviction-only drainers |
| Cloud sentinel | Optional cloud termination notice with its deadline |

The webhook is both a signal source and the enforcement point. Dry-run
requests, deleting pods, unsupported targets and unprotected pods are admitted.
If multiple Understudy resources match a pod, every one must be ready or
relaxed before the eviction is admitted.

## Hold and release decisions

Voluntary disruption is held until the stand-in is ready. An involuntary
deadline is compared with `minSurgeTimeSeconds`; if too little time remains,
the hold is relaxed immediately while the surge continues. A hold is also
relaxed after `readinessDeadlineSeconds`, ensuring an unschedulable stand-in
cannot wedge a drain forever.

```mermaid
flowchart TD
    A[eviction attempt] --> B{protected target}
    B -->|no| C[admit]
    B -->|yes| D{mode off or relaxed}
    D -->|yes| C
    D -->|no| E{healthy stand-in ready}
    E -->|yes| C
    E -->|no| F[return 429 and surge]
    F --> G{ready before deadline}
    G -->|yes| C
    G -->|no| H[relax hold and admit]
```

## Phases

| Phase | Meaning |
|---|---|
| `Idle` | No disruption is active; matching evictions are protected |
| `Surging` | A doomed node was seen and the stand-in is not ready |
| `Releasing` | A healthy stand-in is ready and eviction retries are admitted |
| `Relaxed` | The hold was released without a ready stand-in |
| `Unsupported` | The target cannot be protected and is never held |

## Failure behaviour

The webhook uses `failurePolicy: Ignore`. If all operator replicas are
unavailable, requests fail open after `webhook.timeoutSeconds` and Kubernetes
returns to its normal eviction behavior. Deleting the
`<release>-understudy-eviction-hold` ValidatingWebhookConfiguration disables
holds immediately.

An operator restart therefore creates a protection gap rather than a stuck
cluster. Run two replicas on different nodes for production. Reconciliation
uses leader election, but every replica serves webhook traffic.

The upgrade from 0.3.x runs a leader-gated startup sweep that deletes legacy
PodDisruptionBudgets labeled `understudy.sh/owned=true`. PDB read/delete RBAC
exists only for that migration and is scheduled for removal in v0.5.0.

## Metrics

- `understudy_oldest_blocked_eviction_seconds`: age of the oldest active hold
- `understudy_evictions_held_total`: admission requests answered with 429
- `understudy_eviction_attempts_observed_total`: attempts split by protection
- `understudy_surges_total`: surges by signal class
- `understudy_hold_relaxed_total`: holds relaxed by reason

## Limits

Understudy guarantees a ready replacement, not preservation of requests
already in flight. Use a `preStop` hook long enough for load balancer
deregistration.

Cold capacity still takes time. Voluntary disruptions can wait; involuntary
ones race the platform deadline. Workloads that cannot briefly run two
instances, including single-replica StatefulSets, are out of scope.
