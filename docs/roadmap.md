# Roadmap

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
- **GitOps coexistence.** Document Argo CD and Flux `ignoreDifferences`
  recipes, or move surge state to a separate standby Deployment.
- **EvictionRequest migration.** KEP-4563 is the future interception point.
  When drainers adopt it, the same state machine can respond before acking
  without changing the product boundary.
- **kubectl plugin.** Add `kubectl understudy status` for incident-time
  visibility.
