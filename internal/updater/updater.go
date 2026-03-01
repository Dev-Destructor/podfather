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
// Two strategies are supported:
//
//   - In-place pod resize (Kubernetes 1.27+, KEP-1287): patches the
//     pod spec directly without restarting the container.
//   - Eviction/recreation: annotates the pod with the recommendation,
//     then deletes it so the owning controller (Deployment, etc.)
//     recreates it with updated resources.
package updater

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		if err := u.applyViaEviction(ctx, pod, rec); err != nil {
			return "", fmt.Errorf("eviction update failed: %w", err)
		}
		return "eviction", nil
	case ModeAuto:
		// Try in-place first; fall back to eviction on failure
		if err := u.applyInPlace(ctx, pod, rec); err == nil {
			return "in-place", nil
		}
		if err := u.applyViaEviction(ctx, pod, rec); err != nil {
			return "", fmt.Errorf("fallback eviction also failed: %w", err)
		}
		return "eviction-fallback", nil
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
				int64(rec.CPUCoresRequest*1000), resource.DecimalSI)
			pod.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory] = *resource.NewQuantity(
				int64(rec.MemoryBytesRequest), resource.BinarySI)
			pod.Spec.Containers[i].Resources.Limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(
				int64(rec.CPUCoresLimit*1000), resource.DecimalSI)
			pod.Spec.Containers[i].Resources.Limits[corev1.ResourceMemory] = *resource.NewQuantity(
				int64(rec.MemoryBytesLimit), resource.BinarySI)
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("container %q not found in pod %s/%s", rec.ContainerName, pod.Namespace, pod.Name)
	}

	return u.client.Patch(ctx, pod, patch)
}

// applyViaEviction annotates the pod with the recommendation, then evicts it
// using the Kubernetes Eviction API so that PodDisruptionBudgets are honored.
// The owning workload controller recreates it with the new resources.
func (u *PodUpdater) applyViaEviction(ctx context.Context, pod *corev1.Pod, rec calculator.ResourceRecommendation) error {
	// Annotate with recommendation so the new pod can pick it up
	patch := client.MergeFrom(pod.DeepCopy())

	recJSON, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal recommendation: %w", err)
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations["podfather.io/last-recommendation"] = string(recJSON)

	if err := u.client.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("failed to annotate pod: %w", err)
	}

	// Use the Eviction API to honor PodDisruptionBudgets
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	if err := u.client.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		return fmt.Errorf("failed to evict pod: %w", err)
	}

	return nil
}
