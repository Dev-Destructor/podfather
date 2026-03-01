# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [0.1.1]

### Fixed

- **Multi-replica oscillation bug** — Previously, Podfather evaluated each pod independently. On a Deployment with mixed-load replicas (e.g. one pod at 20% and another at 80%), the low-consuming pod would trigger a workload template patch that immediately throttled or OOMKilled the high-consuming siblings, which would then trigger a second patch in the opposite direction. This created a continuous oscillation loop. The root cause has been resolved by grouping pods by their owning workload and computing a **single aggregated recommendation per workload** before applying any changes.

- **Eviction/recreate loop** — The previous `ModeRecreate` path evicted pods without first patching the workload template. The owning controller (Deployment, StatefulSet, etc.) would recreate new pods using the original stale template, immediately reverting the change. The recreate path now **patches the workload's PodTemplate first**, then evicts the pod so the new replica immediately inherits the correct resources.

- **Reconcile storm on status updates** — The controller was re-enqueued on every `Status().Update()` call because `GenerationChangedPredicate` was not set. This caused a tight reconciliation loop after each status write. The controller now filters out status-only updates via `WithEventFilter(predicate.GenerationChangedPredicate{})`.

- **PodMetrics cache failure** — The Kubernetes Metrics API only supports `GET`/`LIST`, not `WATCH`. The controller-runtime informer cache would fail continuously when trying to establish a watch on `PodMetrics`. `PodMetrics` is now explicitly excluded from the manager's informer cache via `client.CacheOptions.DisableFor`.

### Added

#### CRD: `RemediationStrategy`

New field `spec.remediationStrategy` controls how metrics from multiple replicas of the same workload are aggregated before computing a single resource recommendation.

| Value | Behaviour |
|---|---|
| `MaxPod` | Uses the busiest replica's metrics. Conservative — prevents starvation of the most loaded pod. |
| `MinPod` | Uses the quietest replica's metrics. Aggressive — saves the most resources but risks throttling busier replicas. |
| `Auto` *(default)* | Uses the **P90 (90th percentile)** of each metric across all replicas. Balanced — covers the vast majority of workloads without extreme over- or under-provisioning. Extreme outlier spikes are dampened. |

Example:
```yaml
spec:
  remediationStrategy: Auto   # MaxPod | MinPod | Auto
```

#### CRD: `StatefulSetPolicy`

New field `spec.statefulSetPolicy` controls how Podfather applies recommendations to StatefulSet pods. StatefulSets are identity-aware (named pods with stable storage), so a rolling restart may be undesirable.

| `updateMode` | Behaviour |
|---|---|
| `PerPodInPlace` *(default)* | Applies the unified aggregated recommendation to **each StatefulSet pod individually** via Kubernetes in-place resize (KEP-1287). The StatefulSet template is **never touched** — no rolling restart occurs. |
| `Template` | Patches the StatefulSet PodTemplate, triggering the StatefulSet controller's standard ordered rolling update. |

Example:
```yaml
spec:
  statefulSetPolicy:
    updateMode: PerPodInPlace   # PerPodInPlace | Template
```

#### CRD Status: `WorkloadGroups`

New field `status.workloadGroups` reports per-workload aggregation results. Each entry describes one workload group (Deployment, StatefulSet, DaemonSet, or bare pod) and the aggregate recommendation last computed for it.

```yaml
status:
  workloadGroups:
    - kind: Deployment
      name: my-app
      replicas: 5
      strategy: Auto
      containerRecommendations:
        - containerName: app
          target:
            cpu: "250m"
            memory: "512Mi"
```

#### Metric Aggregation (`internal/calculator/aggregate.go`)

New pure-Go aggregation functions with no Kubernetes dependencies:

- `AggregateUsage(samples, strategy)` — aggregates per-pod `ResourceUsage` samples using the configured strategy (Max, Min, or P90).
- `AggregateAllocations(samples)` — aggregates per-pod `ResourceAllocation` samples using max of each field (represents the current workload template baseline).
- `percentile(vals, p)` — nearest-rank percentile implementation used by the `Auto` strategy.

13 new table-driven unit tests in `internal/calculator/aggregate_test.go` covering:
- Empty/mismatched input error paths
- MaxPod, MinPod, Auto correctness
- P90 with 2, 5, and 10 pods
- Extreme outlier suppression (10-pod cluster where 1 pod spikes to 10× normal)
- Identical-pod degenerate case
- Unknown strategy error

#### Pod-to-Workload Grouping (`internal/controller/grouping.go`)

New package-level functions:

- `groupPodsByOwner(ctx, client, pods)` — groups running pods by their owning workload. Walks `Pod → ReplicaSet → Deployment`, `Pod → StatefulSet`, and `Pod → DaemonSet` owner chains. Pods with no recognized controller owner fall back to individual `BarePod` groups (preserving prior per-pod behaviour).
- `resolveWorkloadKey(ctx, client, pod)` — resolves a single pod's top-level workload key.
- `isRolloutInProgress(ctx, client, key)` — returns `true` if `UpdatedReplicas < DesiredReplicas`. Checked before applying any update; if a rollout is still in progress the current reconcile cycle skips applying and emits a `RolloutInProgress` event instead. Recommendations are still computed and written to `status.workloadGroups` for observability.

#### Workload Template Patching (`internal/updater/updater.go`)

New updater methods:

- `applyViaWorkloadPatch(ctx, pod, rec)` — resolves the pod's owning workload and patches its PodTemplate with the new resource values. Returns `(false, nil)` (idempotent no-op) when the template already has the recommended values, preventing redundant patches that would cause unnecessary rolling updates.
- `patchDeploymentTemplate`, `patchStatefulSetTemplate`, `patchDaemonSetTemplate` — typed wrappers that fetch the workload, check idempotency via `containerResourcesMatch`, and apply a merge-patch.
- `containerResourcesMatch(containers, rec)` — idempotency guard; compares current template resources against the recommendation using `resource.Quantity.Cmp`.
- `patchContainerResources(containers, rec)` — mutates the named container's resource fields in-place on the slice.
- `ApplyToStatefulSetPods(ctx, pods, rec, stsName, stsMode)` — new top-level entry point for StatefulSet-specific update logic. Dispatches to `STSPerPodInPlace` (loop over each pod, `applyInPlace`) or `STSTemplate` (`patchStatefulSetTemplate` once). Partial per-pod success is reported as `"sts-per-pod-partial"` with a non-fatal error.
- `evictPod(ctx, pod)` — extracted from the old `applyViaEviction`; called only after the workload template has already been patched.
- `hasRecreatingOwner(pod)` — guards against calling the recreate path on bare pods that have no controller to recreate them.

#### RBAC Permissions

New permissions added (generated from `+kubebuilder:rbac` markers):

| API Group | Resource | Verbs | Reason |
|---|---|---|---|
| `""` (core) | `pods/eviction` | `create` | Evict pods via the Eviction API (PDB-aware) |
| `apps` | `deployments` | `get, list, watch, update, patch` | Read rollout status; patch PodTemplate |
| `apps` | `replicasets` | `get, list, watch` | Walk `Pod → ReplicaSet → Deployment` owner chain |
| `apps` | `statefulsets` | `get, list, watch, update, patch` | Read rollout status; patch template or per-pod in-place |
| `apps` | `daemonsets` | `get, list, watch, update, patch` | Read rollout status; patch PodTemplate |

### Changed

#### Reconciliation Loop

The pod evaluation loop in `Reconcile()` has been refactored from a flat per-pod iteration to a **group-based evaluation**:

**Before:**
```go
for i := range runningPods {
    recs, adj, ok, reasons := r.evaluatePodMetrics(ctx, pa, &runningPods[i], ...)
}
```

**After (non-VPA path):**
```go
groups, _ := groupPodsByOwner(ctx, r.Client, runningPods)
for key, groupPods := range groups {
    recs, adj, ok, reasons, wgStatus := r.evaluateWorkloadGroup(ctx, pa, key, groupPods, ...)
}
```

The VPA path is unchanged — VPA provides container-level recommendations independent of replica count, so pod-by-pod evaluation remains appropriate there.

New methods added to `PodAutoscalerReconciler`:
- `evaluateWorkloadGroup` — orchestrates metric collection, aggregation, calculation, and apply for a group of pods.
- `applyGroupRecommendation` — routes the apply call: StatefulSets go to `ApplyToStatefulSetPods`; all other workloads apply via the first pod in the group (the template patch idempotency guard handles deduplication).
- `collectGroupMetricsForStatus` — metrics-only read path used when a rollout is in progress (temporarily sets `dryRun=true` to suppress side effects).

#### Updater Package

The `ModeAuto` and `ModeRecreate` fallback paths in `ApplyRecommendation` have been replaced:

- `ModeAuto`: tries in-place resize → falls back to workload template patch (rolling update). Bare pods (no controller) fail fast instead of silently failing.
- `ModeRecreate`: patches the workload template first, then evicts the pod. The old annotation-only approach has been removed.

The old `applyViaEviction` function (which only annotated the pod and evicted it, leaving the template unchanged) has been replaced by `evictPod` (eviction only, called after the template is already patched).

#### CRD Manifest

Regenerated via `make generate && make manifests`. New fields `remediationStrategy`, `statefulSetPolicy`, and `workloadGroups` are reflected in `config/crd/bases/autoscaling.podfather.io_podautoscalers.yaml` along with their OpenAPI validation schemas and enum constraints.

#### Sample CR

`config/samples/autoscaling_v1alpha1_podautoscaler.yaml` now includes `remediationStrategy: Auto` and a commented `statefulSetPolicy` block to make the new fields discoverable.

---

## [0.1.0] — Initial Release

- Operator scaffolded with Operator SDK v1.42.0
- `PodAutoscaler` CRD with selector, targetRef, resourcePolicy, updatePolicy, vpaPolicy
- Reconciliation loop: discover pods → collect metrics → calculate → apply
- In-place vertical pod scaling (KEP-1287) with eviction fallback
- Per-container min/max constraints via `resourcePolicy`
- Configurable variance threshold to prevent update churn
- Dry-run mode
- VPA integration (use VPA recommendations when present)
- Namespace-aware clamping: LimitRange + ResourceQuota
- 6 custom Prometheus metrics
- 5 pre-built alerting rules
- Grafana dashboard (8 panels)
- OpenTelemetry distributed tracing
- Kubernetes Events for every significant action
- Finalizer-based cleanup on CR deletion
- Leader election for HA deployments
- OLM bundle and scorecard integration
