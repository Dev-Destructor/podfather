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

// Package vpa provides VPA (Vertical Pod Autoscaler) integration for Podfather.
//
// It uses the unstructured client to interact with VPA resources, avoiding
// a hard dependency on the VPA Go module. This allows Podfather to:
//
//   - Detect whether VPA CRDs are installed in the cluster.
//   - Discover VPA objects targeting the same workload as a PodAutoscaler.
//   - Extract resource recommendations from VPA status.
//   - Validate that VPA is in "Off" updateMode to prevent conflicts.
//
// When a matching VPA is found and is in "Off" mode, Podfather uses the VPA
// recommendations instead of its own algorithm, ensuring the two systems
// complement rather than conflict with each other.
package vpa

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var log = logf.Log.WithName("vpa")

// VPA GroupVersionResource for autoscaling.k8s.io/v1 VerticalPodAutoscaler.
var vpaGVR = schema.GroupVersionResource{
	Group:    "autoscaling.k8s.io",
	Version:  "v1",
	Resource: "verticalpodautoscalers",
}

// VPA update modes.
const (
	UpdateModeOff      = "Off"
	UpdateModeInitial  = "Initial"
	UpdateModeRecreate = "Recreate"
	UpdateModeAuto     = "Auto"
)

// ContainerRecommendation holds VPA's resource recommendation for a single container.
type ContainerRecommendation struct {
	// ContainerName is the name of the container.
	ContainerName string
	// Target is the VPA-recommended resource requests.
	Target corev1.ResourceList
	// LowerBound is the lower end of the recommendation range.
	LowerBound corev1.ResourceList
	// UpperBound is the upper end of the recommendation range.
	UpperBound corev1.ResourceList
	// UncappedTarget is the recommendation without applying min/max constraints.
	UncappedTarget corev1.ResourceList
}

// VPAInfo holds parsed information about a discovered VPA object.
type VPAInfo struct {
	// Name of the VPA resource.
	Name string
	// Namespace of the VPA resource.
	Namespace string
	// UpdateMode is the VPA's configured update policy mode (Off, Auto, Recreate, Initial).
	UpdateMode string
	// TargetRefKind is the kind of the VPA's target (e.g. "Deployment").
	TargetRefKind string
	// TargetRefName is the name of the VPA's target.
	TargetRefName string
	// HasRecommendation is true if the VPA status contains recommendations.
	HasRecommendation bool
	// ContainerRecommendations holds per-container recommendations from VPA status.
	ContainerRecommendations []ContainerRecommendation
}

// Client provides VPA detection and recommendation fetching capabilities.
type Client struct {
	client client.Client
}

// NewClient creates a new VPA client using the provided controller-runtime client.
func NewClient(c client.Client) *Client {
	return &Client{client: c}
}

// IsVPAInstalled checks whether the VPA CRD is installed in the cluster by
// attempting to list VPA resources. Returns true if the CRD exists, false otherwise.
func (c *Client) IsVPAInstalled(ctx context.Context) bool {
	vpaList := &unstructured.UnstructuredList{}
	vpaList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   vpaGVR.Group,
		Version: vpaGVR.Version,
		Kind:    "VerticalPodAutoscalerList",
	})

	// Use a cluster-wide list with limit=1 to check if the CRD exists.
	err := c.client.List(ctx, vpaList, client.Limit(1))
	if err != nil {
		if errors.IsNotFound(err) || isNoMatchError(err) {
			return false
		}
		// Log but treat other errors as VPA not available to be safe.
		log.V(1).Info("Error checking VPA availability, treating as not installed", "error", err)
		return false
	}
	return true
}

// FindMatchingVPA looks for a VPA object in the given namespace that targets
// the same workload (matching targetRef kind and name). Returns nil if no
// matching VPA is found.
func (c *Client) FindMatchingVPA(ctx context.Context, namespace, targetKind, targetName string) (*VPAInfo, error) {
	vpaList := &unstructured.UnstructuredList{}
	vpaList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   vpaGVR.Group,
		Version: vpaGVR.Version,
		Kind:    "VerticalPodAutoscalerList",
	})

	if err := c.client.List(ctx, vpaList, client.InNamespace(namespace)); err != nil {
		if errors.IsNotFound(err) || isNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to list VPA resources in namespace %s: %w", namespace, err)
	}

	for _, item := range vpaList.Items {
		info, err := parseVPA(&item)
		if err != nil {
			log.V(1).Info("Skipping unparseable VPA", "vpa", item.GetName(), "error", err)
			continue
		}

		if info.TargetRefKind == targetKind && info.TargetRefName == targetName {
			return info, nil
		}
	}

	return nil, nil
}

// GetVPA fetches a specific VPA by name and namespace.
func (c *Client) GetVPA(ctx context.Context, namespace, name string) (*VPAInfo, error) {
	vpa := &unstructured.Unstructured{}
	vpa.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   vpaGVR.Group,
		Version: vpaGVR.Version,
		Kind:    "VerticalPodAutoscaler",
	})

	if err := c.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, vpa); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get VPA %s/%s: %w", namespace, name, err)
	}

	return parseVPA(vpa)
}

// ValidateVPAMode checks that the VPA is in "Off" mode, which means it only
// provides recommendations without applying them. Returns an error describing
// the conflict if the VPA is in any other mode.
func ValidateVPAMode(vpa *VPAInfo) error {
	if vpa == nil {
		return nil
	}

	if vpa.UpdateMode != UpdateModeOff {
		return fmt.Errorf(
			"VPA %s/%s has updateMode=%q but must be %q to avoid conflicts with Podfather; "+
				"Podfather will apply VPA recommendations on behalf of VPA, so VPA should only recommend",
			vpa.Namespace, vpa.Name, vpa.UpdateMode, UpdateModeOff,
		)
	}
	return nil
}

// ---- Parsing helpers ----

// parseVPA extracts VPAInfo from an unstructured VPA object.
func parseVPA(obj *unstructured.Unstructured) (*VPAInfo, error) {
	info := &VPAInfo{
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
	}

	// Parse spec.targetRef
	targetRef, found, err := unstructured.NestedMap(obj.Object, "spec", "targetRef")
	if err != nil || !found {
		return nil, fmt.Errorf("VPA %s/%s missing spec.targetRef", obj.GetNamespace(), obj.GetName())
	}

	if kind, ok := targetRef["kind"].(string); ok {
		info.TargetRefKind = kind
	}
	if name, ok := targetRef["name"].(string); ok {
		info.TargetRefName = name
	}

	// Parse spec.updatePolicy.updateMode (default is "Auto" per VPA spec)
	info.UpdateMode = UpdateModeAuto // VPA default
	updateMode, found, err := unstructured.NestedString(obj.Object, "spec", "updatePolicy", "updateMode")
	if err == nil && found {
		info.UpdateMode = updateMode
	}

	// Parse status.recommendation.containerRecommendations
	containerRecs, found, err := unstructured.NestedSlice(obj.Object, "status", "recommendation", "containerRecommendations")
	if err == nil && found {
		info.HasRecommendation = true
		for _, cr := range containerRecs {
			crMap, ok := cr.(map[string]interface{})
			if !ok {
				continue
			}
			rec := parseContainerRecommendation(crMap)
			info.ContainerRecommendations = append(info.ContainerRecommendations, rec)
		}
	}

	return info, nil
}

// parseContainerRecommendation extracts a single container recommendation from VPA status.
func parseContainerRecommendation(crMap map[string]interface{}) ContainerRecommendation {
	rec := ContainerRecommendation{}

	if name, ok := crMap["containerName"].(string); ok {
		rec.ContainerName = name
	}

	rec.Target = parseResourceList(crMap, "target")
	rec.LowerBound = parseResourceList(crMap, "lowerBound")
	rec.UpperBound = parseResourceList(crMap, "upperBound")
	rec.UncappedTarget = parseResourceList(crMap, "uncappedTarget")

	return rec
}

// parseResourceList extracts a corev1.ResourceList from a nested map field.
func parseResourceList(crMap map[string]interface{}, field string) corev1.ResourceList {
	result := corev1.ResourceList{}

	fieldMap, ok := crMap[field].(map[string]interface{})
	if !ok {
		return result
	}

	if cpuStr, ok := fieldMap["cpu"].(string); ok {
		if q, err := resource.ParseQuantity(cpuStr); err == nil {
			result[corev1.ResourceCPU] = q
		}
	}
	if memStr, ok := fieldMap["memory"].(string); ok {
		if q, err := resource.ParseQuantity(memStr); err == nil {
			result[corev1.ResourceMemory] = q
		}
	}

	return result
}

// isNoMatchError checks if the error indicates the resource type is not registered
// (i.e., the CRD is not installed).
func isNoMatchError(err error) bool {
	// controller-runtime returns a *meta.NoKindMatchError when the GVK is unknown.
	// We check the error string as the type may not be directly importable.
	if err == nil {
		return false
	}
	errStr := err.Error()
	return contains(errStr, "no matches for kind") ||
		contains(errStr, "the server could not find the requested resource") ||
		contains(errStr, "no match")
}

// contains is a simple substring check to avoid importing strings package.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
