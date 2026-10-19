# Roadmap

## v0.4.5 - Handover reliability

Stand-ins honour `minReadySeconds`, failed surges retain the bounded eviction
hold, and every operator replica watches node signals for webhook decisions.
The sentinel chart arguments are corrected and checked against the binaries in
CI. Dependency updates address identified x/net, gRPC, and OpenTelemetry
advisories.

## v0.4.4 - GitOps baseline capacity

`spec.baselineReplicas` lets Git supply the desired baseline while Understudy
owns effective Deployment or Rollout replicas. Changes during handover remain
pending and the latest value converges afterward, including scale-to-zero.
`BaselineApplied` reports that state. Application release takeover applies the
latest baseline while disruption protection stands down.

The [Argo CD ownership contract](gitops-capacity.md) was validated against
Argo CD 3.5.5 and Argo Rollouts 1.10.1 with automatic sync and self-heal enabled,
including unrelated sync, operator restart, and release-during-handover checks.

## v0.4.3 - Argo Rollouts

Fully promoted canary and blue/green Rollouts can now be protected using
`targetRef.kind: Rollout`. Replica scaling uses Argo's native controller, and
only the serving revision counts toward replacement readiness. Progressive
delivery, pauses, and aborts cause the controller and webhook to stand down.
Argo Rollouts remains optional and is detected at operator startup.

Validated with Argo Rollouts v1.10.1 on an isolated Kubernetes 1.36.4 Kind
cluster, including eviction holds, scale-back, HTTP serving, and paused releases.

## v0.4.1 - holdMode

`hostageMode` is renamed to `holdMode`, the old field still accepted and
reported once as deprecated. `status.mode` carries the resolved value and
feeds the `MODE` print column. Upgrade notes moved out of the readme into
UPGRADING.md.

## v0.4.0 - admission-time eviction hold

The validating webhook is now the enforcement point. It returns the same HTTP
429 as a PodDisruptionBudget until a healthy stand-in is ready, then admits the
drainer's retry. Understudy owns no budgets.

This resolves budget-pinned nodes structurally. Karpenter was live-observed
retrying the webhook 21 times over 43 seconds and completing when admission
opened, with no `DisruptionBlocked` event. `kubectl drain` followed the same
retry path. A startup migration sweep removes 0.3.x budgets.

The failure posture is intentionally fail-open: operator unavailability creates
a protection gap, not a stuck cluster. PDB migration RBAC remains through
v0.4.x and will be removed in v0.5.0.

## Shipped foundations

- Core surge state machine with cordon and taint adapters
- Universal eviction webhook and admission hold
- AWS cloud sentinel, with GCP and Azure probes covered by unit tests
- Readiness gates, deletion-cost steering and bounded hold relaxation

## Open design items

- **Azure event approval.** Scheduled Events can be approved after traffic
  readiness instead of merely observed.
- **GCP and Azure live validation.** Both sentinels remain experimental until
  exercised against their real metadata services.
- **Flux coexistence.** Validate and document Flux field delegation for the
  declarative baseline capacity contract.
- **EvictionRequest migration.** KEP-4563 is the future interception point.
  When drainers adopt it, the same state machine can respond before acking
  without changing the product boundary.
- **kubectl plugin.** Add `kubectl understudy status` for incident-time
  visibility.
