/*
Copyright 2026 Podfather Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package updater applies computed resource recommendations to pods.
//
// Three strategies are supported:
//
//   - In-place pod resize (Kubernetes 1.27+, KEP-1287): patches the
//     pod spec directly without restarting the container.
//   - Workload template patch: updates the parent workload (Deployment,
//     StatefulSet, DaemonSet) PodTemplate so the workload controller
//     rolls out new pods with corrected resources.
//   - Eviction/recreation: patches the workload template first, then
//     evicts the pod for immediate replacement.
package updater

import (
	"context"
	"fmt"
	"math"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/des57/podfather/internal/calculator"
)

// UpdateMode mirrors the CRD UpdateMode for internal use.
type UpdateMode string

const (
	ModeAuto     UpdateMode = "Auto"
	ModeInPlace  UpdateMode = "InPlace"
	ModeRecreate UpdateMode = "Recreate"
	ModeOff      UpdateMode = "Off"
)

// Workload kind constants.
const (
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
)

// Strategy result constants.
const (
	// StrategyAlreadyApplied indicates the workload template already has the
	// recommended resource values — no mutation was needed.
	StrategyAlreadyApplied = "already-applied"
)

// PodUpdater applies resource recommendations to pods.
type PodUpdater struct {
	client client.Client
}

// NewPodUpdater creates a new PodUpdater.
func NewPodUpdater(c client.Client) *PodUpdater {
	return &PodUpdater{client: c}
}

// ApplyRecommendation applies a resource recommendation to a specific container
// in a pod using the specified mode. Returns the strategy actually used.
//
// For ModeAuto: tries in-place resize first, then falls back to patching
// the parent workload's PodTemplate (letting its rolling-update replace pods).
// For ModeRecreate: patches the workload template, then evicts the pod for
// immediate replacement. This avoids the old eviction-only loop where the
// workload recreated pods with the original (stale) resource values.
func (u *PodUpdater) ApplyRecommendation(
	ctx context.Context,
	pod *corev1.Pod,
	rec calculator.ResourceRecommendation,
	mode UpdateMode,
) (string, error) {
	switch mode {
	case ModeOff:
		return "off", nil

	case ModeInPlace:
		if err := u.applyInPlace(ctx, pod, rec); err != nil {
			return "", fmt.Errorf("in-place update failed: %w", err)
		}
		return "in-place", nil

	case ModeRecreate:
		// Patch the workload template first so the new pod gets correct resources,
		// then evict to force immediate replacement.
		if !hasRecreatingOwner(pod) {
			return "", fmt.Errorf("pod %s/%s has no owning controller; cannot use recreate mode",
				pod.Namespace, pod.Name)
		}
		patched, err := u.applyViaWorkloadPatch(ctx, pod, rec)
		if err != nil {
			return "", fmt.Errorf("workload template patch failed: %w", err)
		}
		// In Recreate mode we always evict, even when the template already
		// matches the recommendation. The running pod may still have stale
		// resources and the caller explicitly asked for immediate replacement.
		if err := u.evictPod(ctx, pod); err != nil {
			if patched {
				// Template was patched — the rolling update will eventually
				// replace the pod even if eviction fails. Surface as partial
				// success so the caller does not treat it as fully failed.
				return "workload-patch-eviction-failed", nil
			}
			// Template was already up-to-date and eviction failed.
			return StrategyAlreadyApplied, fmt.Errorf("eviction failed: %w", err)
		}
		return "recreate", nil

	case ModeAuto:
		// 1. Try in-place resize (KEP-1287, zero-downtime).
		if err := u.applyInPlace(ctx, pod, rec); err == nil {
			return "in-place", nil
		}
		// 2. Fall back to workload template patch (rolling update handles replacement).
		if hasRecreatingOwner(pod) {
			patched, err := u.applyViaWorkloadPatch(ctx, pod, rec)
			if err != nil {
				return "", fmt.Errorf("in-place resize failed and workload patch failed: %w", err)
			}
			if !patched {
				return StrategyAlreadyApplied, nil
			}
			return "workload-patch", nil
		}
		// 3. Bare pod — neither strategy is safe.
		return "", fmt.Errorf("in-place resize failed and pod %s/%s has no owning controller",
			pod.Namespace, pod.Name)

	default:
		return "", fmt.Errorf("unknown update mode: %s", mode)
	}
}

// applyInPlace patches the pod's container resources directly (KEP-1287).
func (u *PodUpdater) applyInPlace(ctx context.Context, pod *corev1.Pod, rec calculator.ResourceRecommendation) error {
	patch := client.MergeFrom(pod.DeepCopy())

	updated := false
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == rec.ContainerName {
			if pod.Spec.Containers[i].Resources.Requests == nil {
				pod.Spec.Containers[i].Resources.Requests = corev1.ResourceList{}
			}
			if pod.Spec.Containers[i].Resources.Limits == nil {
				pod.Spec.Containers[i].Resources.Limits = corev1.ResourceList{}
			}
			pod.Spec.Containers[i].Resources.Requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(
				int64(math.Round(rec.CPUCoresRequest*1000)), resource.DecimalSI)
			pod.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(
				int64(math.Round(rec.MemoryBytesRequest)), resource.BinarySI)
			pod.Spec.Containers[i].Resources.Limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(
				int64(math.Round(rec.CPUCoresLimit*1000)), resource.DecimalSI)
			pod.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory] = *resource.NewQuantity(
				int64(math.Round(rec.MemoryBytesLimit)), resource.BinarySI)
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("container %q not found in pod %s/%s", rec.ContainerName, pod.Namespace, pod.Name)
	}

	return u.client.Patch(ctx, pod, patch)
}

// ---- Workload Template Patching ----

// applyViaWorkloadPatch resolves the pod's owning workload (Deployment,
// StatefulSet, DaemonSet) and patches its PodTemplate with the new resource
// values. The workload controller's rolling-update mechanism then replaces
// pods with the corrected spec — breaking the evict-recreate-same-resources loop.
// Returns (true, nil) if the template was actually patched, (false, nil) if it
// already had the correct values, or (false, err) on failure.
func (u *PodUpdater) applyViaWorkloadPatch(ctx context.Context, pod *corev1.Pod, rec calculator.ResourceRecommendation) (bool, error) {
	kind, name, err := u.resolveOwningWorkload(ctx, pod)
	if err != nil {
		return false, fmt.Errorf("could not resolve owning workload for pod %s/%s: %w",
			pod.Namespace, pod.Name, err)
	}

	switch kind {
	case kindDeployment:
		return u.patchDeploymentTemplate(ctx, pod.Namespace, name, rec)
	case kindStatefulSet:
		return u.patchStatefulSetTemplate(ctx, pod.Namespace, name, rec)
	case kindDaemonSet:
		return u.patchDaemonSetTemplate(ctx, pod.Namespace, name, rec)
	default:
		return false, fmt.Errorf("unsupported workload kind %q for pod %s/%s",
			kind, pod.Namespace, pod.Name)
	}
}

// resolveOwningWorkload walks the pod's ownerReferences to find the top-level
// workload that controls PodTemplate resources.
//
// Supported chains: Pod → ReplicaSet → Deployment, Pod → StatefulSet,
// Pod → DaemonSet.
func (u *PodUpdater) resolveOwningWorkload(ctx context.Context, pod *corev1.Pod) (string, string, error) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "ReplicaSet":
			// Walk one level up: ReplicaSet → Deployment
			rs := &appsv1.ReplicaSet{}
			if err := u.client.Get(ctx, types.NamespacedName{
				Namespace: pod.Namespace,
				Name:      ref.Name,
			}, rs); err != nil {
				return "", "", fmt.Errorf("failed to get ReplicaSet %s: %w", ref.Name, err)
			}
			for _, rsRef := range rs.OwnerReferences {
				if rsRef.Controller != nil && *rsRef.Controller && rsRef.Kind == kindDeployment {
					return kindDeployment, rsRef.Name, nil
				}
			}
			return "", "", fmt.Errorf("ReplicaSet %s has no Deployment owner", ref.Name)

		case kindStatefulSet:
			return kindStatefulSet, ref.Name, nil

		case kindDaemonSet:
			return kindDaemonSet, ref.Name, nil
		}
	}
	return "", "", fmt.Errorf("no supported owning workload found for pod %s/%s",
		pod.Namespace, pod.Name)
}

func (u *PodUpdater) patchDeploymentTemplate(ctx context.Context, namespace, name string, rec calculator.ResourceRecommendation) (bool, error) {
	deploy := &appsv1.Deployment{}
	if err := u.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, deploy); err != nil {
		return false, fmt.Errorf("failed to get Deployment %s/%s: %w", namespace, name, err)
	}

	// Skip if template already has the recommended resources (avoids redundant patches)
	if containerResourcesMatch(deploy.Spec.Template.Spec.Containers, rec) {
		return false, nil
	}

	patch := client.MergeFrom(deploy.DeepCopy())
	if !patchContainerResources(deploy.Spec.Template.Spec.Containers, rec) {
		return false, fmt.Errorf("container %q not found in Deployment %s/%s template",
			rec.ContainerName, namespace, name)
	}
	if err := u.client.Patch(ctx, deploy, patch); err != nil {
		return false, err
	}
	return true, nil
}

func (u *PodUpdater) patchStatefulSetTemplate(ctx context.Context, namespace, name string, rec calculator.ResourceRecommendation) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := u.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, sts); err != nil {
		return false, fmt.Errorf("failed to get StatefulSet %s/%s: %w", namespace, name, err)
	}

	if containerResourcesMatch(sts.Spec.Template.Spec.Containers, rec) {
		return false, nil
	}

	patch := client.MergeFrom(sts.DeepCopy())
	if !patchContainerResources(sts.Spec.Template.Spec.Containers, rec) {
		return false, fmt.Errorf("container %q not found in StatefulSet %s/%s template",
			rec.ContainerName, namespace, name)
	}
	if err := u.client.Patch(ctx, sts, patch); err != nil {
		return false, err
	}
	return true, nil
}

func (u *PodUpdater) patchDaemonSetTemplate(ctx context.Context, namespace, name string, rec calculator.ResourceRecommendation) (bool, error) {
	ds := &appsv1.DaemonSet{}
	if err := u.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, ds); err != nil {
		return false, fmt.Errorf("failed to get DaemonSet %s/%s: %w", namespace, name, err)
	}

	if containerResourcesMatch(ds.Spec.Template.Spec.Containers, rec) {
		return false, nil
	}

	patch := client.MergeFrom(ds.DeepCopy())
	if !patchContainerResources(ds.Spec.Template.Spec.Containers, rec) {
		return false, fmt.Errorf("container %q not found in DaemonSet %s/%s template",
			rec.ContainerName, namespace, name)
	}
	if err := u.client.Patch(ctx, ds, patch); err != nil {
		return false, err
	}
	return true, nil
}

// containerResourcesMatch checks whether the named container in the template
// already has resources matching the recommendation. This prevents redundant
// patches that would trigger unnecessary rolling updates and reconcile storms.
func containerResourcesMatch(containers []corev1.Container, rec calculator.ResourceRecommendation) bool {
	wantCPUReq := resource.NewMilliQuantity(int64(math.Round(rec.CPUCoresRequest*1000)), resource.DecimalSI)
	wantMemReq := resource.NewQuantity(int64(math.Round(rec.MemoryBytesRequest)), resource.BinarySI)
	wantCPULim := resource.NewMilliQuantity(int64(math.Round(rec.CPUCoresLimit*1000)), resource.DecimalSI)
	wantMemLim := resource.NewQuantity(int64(math.Round(rec.MemoryBytesLimit)), resource.BinarySI)

	for i := range containers {
		if containers[i].Name != rec.ContainerName {
			continue
		}
		reqs := containers[i].Resources.Requests
		lims := containers[i].Resources.Limits
		if reqs == nil || lims == nil {
			return false
		}
		curCPUReq := reqs.Cpu()
		curMemReq := reqs.Memory()
		curCPULim := lims.Cpu()
		curMemLim := lims.Memory()
		return curCPUReq.Cmp(*wantCPUReq) == 0 &&
			curMemReq.Cmp(*wantMemReq) == 0 &&
			curCPULim.Cmp(*wantCPULim) == 0 &&
			curMemLim.Cmp(*wantMemLim) == 0
	}
	return false
}

// patchContainerResources finds the named container and sets its resources
// to the recommended values. Returns false if the container was not found.
func patchContainerResources(containers []corev1.Container, rec calculator.ResourceRecommendation) bool {
	for i := range containers {
		if containers[i].Name != rec.ContainerName {
			continue
		}
		if containers[i].Resources.Requests == nil {
			containers[i].Resources.Requests = corev1.ResourceList{}
		}
		if containers[i].Resources.Limits == nil {
			containers[i].Resources.Limits = corev1.ResourceList{}
		}
		containers[i].Resources.Requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(
			int64(math.Round(rec.CPUCoresRequest*1000)), resource.DecimalSI)
		containers[i].Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(
			int64(math.Round(rec.MemoryBytesRequest)), resource.BinarySI)
		containers[i].Resources.Limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(
			int64(math.Round(rec.CPUCoresLimit*1000)), resource.DecimalSI)
		containers[i].Resources.Limits[corev1.ResourceMemory] = *resource.NewQuantity(
			int64(math.Round(rec.MemoryBytesLimit)), resource.BinarySI)
		return true
	}
	return false
}

// ---- Eviction ----

// evictPod evicts a pod via the Kubernetes Eviction API, honoring
// PodDisruptionBudgets. This should only be called after the owning
// workload's template has already been patched.
func (u *PodUpdater) evictPod(ctx context.Context, pod *corev1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	if err := u.client.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		return fmt.Errorf("failed to evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// hasRecreatingOwner returns true if the pod has at least one owner reference
// with Controller=true, meaning a workload controller (Deployment, ReplicaSet,
// StatefulSet, DaemonSet, Job) will recreate the pod after eviction.
func hasRecreatingOwner(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// ---- StatefulSet Per-Pod In-Place ----

// StatefulSetUpdateMode mirrors the CRD StatefulSetUpdateMode.
type StatefulSetUpdateMode string

const (
	// STSPerPodInPlace resizes each pod individually via in-place resize.
	STSPerPodInPlace StatefulSetUpdateMode = "PerPodInPlace"
	// STSTemplate patches the StatefulSet PodTemplate, triggering a rolling update.
	STSTemplate StatefulSetUpdateMode = "Template"
)

// ApplyToStatefulSetPods applies the aggregated recommendation to all
// StatefulSet pods using the specified StatefulSet update mode.
//
// PerPodInPlace: applies in-place resize to each pod individually. If in-place
// resize fails for a pod, it logs a warning and skips (never touches the STS
// template unless the mode is Template).
//
// Template: patches the StatefulSet PodTemplate, triggering the standard
// rolling update. Applied once — not per-pod.
func (u *PodUpdater) ApplyToStatefulSetPods(
	ctx context.Context,
	pods []corev1.Pod,
	rec calculator.ResourceRecommendation,
	stsName string,
	stsMode StatefulSetUpdateMode,
) (string, error) {
	if len(pods) == 0 {
		return "no-pods", nil
	}

	namespace := pods[0].Namespace

	switch stsMode {
	case STSTemplate:
		patched, err := u.patchStatefulSetTemplate(ctx, namespace, stsName, rec)
		if err != nil {
			return "", fmt.Errorf("failed to patch StatefulSet %s/%s template: %w", namespace, stsName, err)
		}
		if !patched {
			return StrategyAlreadyApplied, nil
		}
		return "sts-template-patch", nil

	case STSPerPodInPlace:
		var applied int
		var lastErr error
		for i := range pods {
			if err := u.applyInPlace(ctx, &pods[i], rec); err != nil {
				lastErr = err
				continue
			}
			applied++
		}
		if applied == 0 && lastErr != nil {
			return "", fmt.Errorf("in-place resize failed for all %d StatefulSet pods: %w",
				len(pods), lastErr)
		}
		if lastErr != nil {
			// Partial success — some pods resized, some failed.
			return "sts-per-pod-partial", fmt.Errorf(
				"in-place resize succeeded for %d/%d pods, last error: %w",
				applied, len(pods), lastErr)
		}
		return "sts-per-pod-in-place", nil

	default:
		// Default to per-pod in-place for safety.
		return u.ApplyToStatefulSetPods(ctx, pods, rec, stsName, STSPerPodInPlace)
	}
}
