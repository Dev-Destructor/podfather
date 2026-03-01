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

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Workload kind constants used in WorkloadKey.Kind.
const (
	WorkloadKindDeployment  = "Deployment"
	WorkloadKindStatefulSet = "StatefulSet"
	WorkloadKindDaemonSet   = "DaemonSet"
	WorkloadKindBarePod     = "BarePod"
)

// WorkloadKey uniquely identifies a workload that owns a group of pods.
type WorkloadKey struct {
	Kind      string
	Namespace string
	Name      string
}

// String returns a human-readable representation of the workload key.
func (k WorkloadKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.Kind, k.Namespace, k.Name)
}

// BarePodsKey returns a synthetic key for pods that have no owning controller.
// Each bare pod is its own group, keyed by pod name.
func BarePodsKey(namespace, podName string) WorkloadKey {
	return WorkloadKey{Kind: WorkloadKindBarePod, Namespace: namespace, Name: podName}
}

// groupPodsByOwner groups running pods by their owning workload controller.
//
// Supported owner chains:
//   - Pod → ReplicaSet → Deployment
//   - Pod → StatefulSet
//   - Pod → DaemonSet
//
// Pods without a recognized controller owner are each placed in their own
// group under a synthetic "BarePod" key.
//
// ReplicaSet → Deployment resolution is cached for the duration of the call
// to avoid redundant API server GETs for pods sharing the same ReplicaSet.
func groupPodsByOwner(ctx context.Context, c client.Client, pods []corev1.Pod) map[WorkloadKey][]corev1.Pod {
	logger := logf.FromContext(ctx)
	groups := make(map[WorkloadKey][]corev1.Pod)
	// Cache ReplicaSet→Deployment resolution to avoid N+1 GETs for pods
	// that share the same ReplicaSet (e.g. all replicas of one Deployment).
	rsCache := make(map[types.NamespacedName]WorkloadKey)

	for i := range pods {
		pod := &pods[i]
		key, err := resolveWorkloadKey(ctx, c, pod, rsCache)
		if err != nil {
			// Unresolvable owner — treat as bare pod.
			logger.V(1).Info("Could not resolve owner, treating as bare pod",
				"pod", pod.Name, "error", err)
			key = BarePodsKey(pod.Namespace, pod.Name)
		}
		groups[key] = append(groups[key], pods[i])
	}

	return groups
}

// resolveWorkloadKey walks the pod's ownerReferences to determine the
// top-level workload key. rsCache is used to avoid duplicate API calls for
// pods that share the same ReplicaSet.
func resolveWorkloadKey(ctx context.Context, c client.Client, pod *corev1.Pod, rsCache map[types.NamespacedName]WorkloadKey) (WorkloadKey, error) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}

		switch ref.Kind {
		case "ReplicaSet":
			// Check cache first to avoid redundant GETs.
			rsKey := types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}
			if cached, ok := rsCache[rsKey]; ok {
				return cached, nil
			}
			// Walk one level: ReplicaSet → Deployment.
			rs := &appsv1.ReplicaSet{}
			if err := c.Get(ctx, rsKey, rs); err != nil {
				return WorkloadKey{}, fmt.Errorf("failed to get ReplicaSet %s: %w", ref.Name, err)
			}
			for _, rsRef := range rs.OwnerReferences {
				if rsRef.Controller != nil && *rsRef.Controller && rsRef.Kind == WorkloadKindDeployment {
					wk := WorkloadKey{
						Kind:      WorkloadKindDeployment,
						Namespace: pod.Namespace,
						Name:      rsRef.Name,
					}
					rsCache[rsKey] = wk
					return wk, nil
				}
			}
			return WorkloadKey{}, fmt.Errorf("ReplicaSet %s has no Deployment owner", ref.Name)

		case WorkloadKindStatefulSet:
			return WorkloadKey{
				Kind:      WorkloadKindStatefulSet,
				Namespace: pod.Namespace,
				Name:      ref.Name,
			}, nil

		case WorkloadKindDaemonSet:
			return WorkloadKey{
				Kind:      WorkloadKindDaemonSet,
				Namespace: pod.Namespace,
				Name:      ref.Name,
			}, nil
		}
	}

	return WorkloadKey{}, fmt.Errorf("no supported owning workload found for pod %s/%s",
		pod.Namespace, pod.Name)
}

// isRolloutInProgress checks whether the workload currently has a rolling
// update in progress. Returns true if updatedReplicas < desiredReplicas,
// meaning the previous recommendation is still rolling out.
func isRolloutInProgress(ctx context.Context, c client.Client, key WorkloadKey) (bool, error) {
	switch key.Kind {
	case WorkloadKindDeployment:
		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, deploy); err != nil {
			return false, fmt.Errorf("failed to get Deployment %s: %w", key.Name, err)
		}
		desired := int32(1)
		if deploy.Spec.Replicas != nil {
			desired = *deploy.Spec.Replicas
		}
		return deploy.Status.UpdatedReplicas < desired, nil

	case WorkloadKindStatefulSet:
		sts := &appsv1.StatefulSet{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, sts); err != nil {
			return false, fmt.Errorf("failed to get StatefulSet %s: %w", key.Name, err)
		}
		desired := int32(1)
		if sts.Spec.Replicas != nil {
			desired = *sts.Spec.Replicas
		}
		return sts.Status.UpdatedReplicas < desired, nil

	case WorkloadKindDaemonSet:
		ds := &appsv1.DaemonSet{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: key.Name}, ds); err != nil {
			return false, fmt.Errorf("failed to get DaemonSet %s: %w", key.Name, err)
		}
		return ds.Status.UpdatedNumberScheduled < ds.Status.DesiredNumberScheduled, nil

	default:
		// Bare pods or unknown kinds — no rollout concept.
		return false, nil
	}
}
