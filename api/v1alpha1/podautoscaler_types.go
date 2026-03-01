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

// Package v1alpha1 contains API Schema definitions for the autoscaling v1alpha1 API group.
// The main type is PodAutoscaler, which represents a policy for automatic vertical
// scaling of pod resources (CPU and memory requests/limits).
//
// +kubebuilder:object:generate=true
// +groupName=autoscaling.podfather.io
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---- Constants & Enums ----

// PodAutoscalerPhase describes the high-level lifecycle phase of the autoscaler.
type PodAutoscalerPhase string

const (
	// PhasePending means the PodAutoscaler has been accepted but is not yet acting.
	PhasePending PodAutoscalerPhase = "Pending"
	// PhaseActive means the PodAutoscaler is actively monitoring and adjusting pods.
	PhaseActive PodAutoscalerPhase = "Active"
	// PhaseDegraded means the PodAutoscaler is running but encountered recoverable issues.
	PhaseDegraded PodAutoscalerPhase = "Degraded"
	// PhaseError means the PodAutoscaler has encountered a fatal error and is not operating.
	PhaseError PodAutoscalerPhase = "Error"
)

// UpdateMode controls how Podfather applies resource changes to pods.
type UpdateMode string

const (
	// UpdateModeAuto tries in-place pod resize (KEP-1287) first, then falls back to eviction.
	UpdateModeAuto UpdateMode = "Auto"
	// UpdateModeInPlace only attempts in-place pod resize; errors if unsupported.
	UpdateModeInPlace UpdateMode = "InPlace"
	// UpdateModeRecreate always evicts the pod and lets the workload controller recreate it.
	UpdateModeRecreate UpdateMode = "Recreate"
	// UpdateModeOff disables mutations; recommendations are computed and stored in status only.
	UpdateModeOff UpdateMode = "Off"
)

// RemediationStrategy controls how metrics from multiple replicas within
// a workload (Deployment, StatefulSet, DaemonSet) are aggregated before
// computing a single resource recommendation for the workload.
type RemediationStrategy string

const (
	// RemediationMaxPod bases the recommendation on the replica consuming the
	// most resources. Conservative: prevents starvation of the busiest pod.
	RemediationMaxPod RemediationStrategy = "MaxPod"
	// RemediationMinPod bases the recommendation on the replica consuming the
	// fewest resources. Aggressive: saves the most resources, but risks
	// throttling or OOMKill on busier replicas.
	RemediationMinPod RemediationStrategy = "MinPod"
	// RemediationAuto aggregates metrics across all replicas using P90
	// (90th percentile) for each metric field. Balanced: covers most workloads
	// without extreme over/under-provisioning.
	RemediationAuto RemediationStrategy = "Auto"
)

// StatefulSetUpdateMode controls how resource changes are applied to
// StatefulSet pods.
type StatefulSetUpdateMode string

const (
	// StatefulSetPerPodInPlace applies the unified recommendation to each
	// StatefulSet pod individually via in-place resize. The StatefulSet
	// template is never touched, so no rolling restart occurs.
	StatefulSetPerPodInPlace StatefulSetUpdateMode = "PerPodInPlace"
	// StatefulSetTemplate patches the StatefulSet PodTemplate, triggering
	// the StatefulSet controller's standard rolling update.
	StatefulSetTemplate StatefulSetUpdateMode = "Template"
)

// ---- Spec Types ----

// PodAutoscalerSpec defines the desired state of PodAutoscaler.
type PodAutoscalerSpec struct {
	// Selector identifies the set of pods managed by this PodAutoscaler.
	// Only pods matching this selector will be monitored and (optionally) resized.
	// +kubebuilder:validation:Required
	Selector *metav1.LabelSelector `json:"selector"`

	// TargetRef is an optional reference to the workload controller (e.g. Deployment)
	// that owns the managed pods. Used for informational purposes and future eviction safety.
	// +optional
	TargetRef *CrossVersionObjectReference `json:"targetRef,omitempty"`

	// ResourcePolicy provides per-container resource boundaries.
	// If omitted, Podfather uses built-in default constraints.
	// +optional
	ResourcePolicy *ResourcePolicy `json:"resourcePolicy,omitempty"`

	// UpdatePolicy controls how and whether resource changes are applied to pods.
	// +optional
	UpdatePolicy *UpdatePolicy `json:"updatePolicy,omitempty"`

	// MetricsCollectionIntervalSeconds is how often (in seconds) the controller
	// reconciles and re-evaluates resource allocations. Minimum: 10.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:default=30
	// +optional
	MetricsCollectionIntervalSeconds *int32 `json:"metricsCollectionIntervalSeconds,omitempty"`

	// EvaluationWindowMinutes controls the time window for aggregating metrics. Minimum: 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	EvaluationWindowMinutes *int32 `json:"evaluationWindowMinutes,omitempty"`

	// VarianceThresholdPercent is the minimum percentage difference between current
	// allocation and recommended allocation required to trigger an update.
	// This prevents excessive churn caused by small oscillations.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=15
	// +optional
	VarianceThresholdPercent *int32 `json:"varianceThresholdPercent,omitempty"`

	// DryRun, when true, makes the controller calculate recommendations but never
	// mutate pods. Useful for auditing before enabling live updates.
	// +kubebuilder:default=false
	// +optional
	DryRun bool `json:"dryRun,omitempty"`

	// VPAPolicy configures how Podfather interacts with Kubernetes Vertical Pod
	// Autoscaler (VPA). When enabled, Podfather detects VPA objects targeting the
	// same workload and uses VPA recommendations instead of its own algorithm.
	// +optional
	VPAPolicy *VPAPolicy `json:"vpaPolicy,omitempty"`

	// RemediationStrategy controls how metrics from multiple replicas within
	// a workload are aggregated before computing a single recommendation.
	// MaxPod uses the busiest replica, MinPod the quietest, and Auto uses P90.
	// +kubebuilder:validation:Enum=MaxPod;MinPod;Auto
	// +kubebuilder:default=Auto
	// +optional
	RemediationStrategy *RemediationStrategy `json:"remediationStrategy,omitempty"`

	// StatefulSetPolicy configures how resource changes are applied to
	// StatefulSet pods. By default, Podfather applies the aggregated
	// recommendation via in-place resize to each pod individually, avoiding
	// a rolling restart.
	// +optional
	StatefulSetPolicy *StatefulSetPolicy `json:"statefulSetPolicy,omitempty"`
}

// CrossVersionObjectReference identifies a workload controller across API versions.
type CrossVersionObjectReference struct {
	// Kind of the referent (e.g. "Deployment", "StatefulSet").
	Kind string `json:"kind"`
	// Name of the referent.
	Name string `json:"name"`
	// API version of the referent (e.g. "apps/v1").
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
}

// ResourcePolicy groups per-container resource constraints.
type ResourcePolicy struct {
	// ContainerPolicies is a list of per-container resource policies.
	// Use containerName "*" to set defaults for all containers.
	// +optional
	ContainerPolicies []ContainerResourcePolicy `json:"containerPolicies,omitempty"`
}

// ContainerResourcePolicy defines resource constraints for a single container.
type ContainerResourcePolicy struct {
	// ContainerName is the name of the container, or "*" for all containers.
	ContainerName string `json:"containerName"`

	// MinAllowed specifies the minimum resources Podfather may recommend.
	// +optional
	MinAllowed corev1.ResourceList `json:"minAllowed,omitempty"`

	// MaxAllowed specifies the maximum resources Podfather may recommend.
	// +optional
	MaxAllowed corev1.ResourceList `json:"maxAllowed,omitempty"`

	// ControlledResources lists which resources (cpu, memory) Podfather manages.
	// If empty, both cpu and memory are managed.
	// +optional
	ControlledResources []corev1.ResourceName `json:"controlledResources,omitempty"`
}

// VPAPolicy configures Podfather's VPA integration behaviour.
type VPAPolicy struct {
	// Enabled controls whether Podfather should look for a VPA targeting the
	// same workload. When true, if a matching VPA is found in "Off" mode,
	// Podfather uses VPA recommendations instead of its own algorithm.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// VPAName optionally specifies the exact VPA name to look for. If empty,
	// Podfather discovers a VPA by matching spec.targetRef against the
	// PodAutoscaler's targetRef.
	// +optional
	VPAName string `json:"vpaName,omitempty"`

	// StrictMode, when true, causes the reconciler to emit warnings and refuse
	// to act if a matching VPA is found but is NOT in "Off" mode. When false
	// (default), Podfather logs a warning and falls back to its own algorithm.
	// +kubebuilder:default=false
	// +optional
	StrictMode bool `json:"strictMode,omitempty"`
}

// UpdatePolicy controls the strategy used to apply resource changes.
type UpdatePolicy struct {
	// UpdateMode selects the update strategy. One of: Auto (default), InPlace, Recreate, Off.
	// +kubebuilder:validation:Enum=Auto;InPlace;Recreate;Off
	// +kubebuilder:default=Auto
	// +optional
	UpdateMode UpdateMode `json:"updateMode,omitempty"`

	// MaxUnavailable is the maximum number of pods that can be unavailable during
	// eviction-based updates. Only relevant for Recreate and Auto (fallback) modes.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`
}

// StatefulSetPolicy configures StatefulSet-specific update behaviour.
type StatefulSetPolicy struct {
	// UpdateMode controls how resource changes are applied to StatefulSet pods.
	// PerPodInPlace (default) resizes each pod in-place without touching the STS
	// template. Template patches the PodTemplate, triggering a rolling update.
	// +kubebuilder:validation:Enum=PerPodInPlace;Template
	// +kubebuilder:default=PerPodInPlace
	// +optional
	UpdateMode StatefulSetUpdateMode `json:"updateMode,omitempty"`
}

// ---- Status Types ----

// PodAutoscalerStatus defines the observed state of PodAutoscaler.
type PodAutoscalerStatus struct {
	// Phase is the high-level lifecycle phase of the PodAutoscaler.
	// +optional
	Phase PodAutoscalerPhase `json:"phase,omitempty"`

	// Conditions represent the latest available observations of the PodAutoscaler's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// MonitoredPods is the current number of running pods being monitored.
	// +optional
	MonitoredPods int32 `json:"monitoredPods,omitempty"`

	// Recommendation holds the most recently computed resource recommendation.
	// +optional
	Recommendation *Recommendation `json:"recommendation,omitempty"`

	// LastEvaluationTime is the timestamp of the most recent metrics evaluation.
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`

	// LastUpdateTime is the timestamp of the most recent pod resource update.
	// +optional
	LastUpdateTime *metav1.Time `json:"lastUpdateTime,omitempty"`

	// TotalAdjustments is the cumulative number of resource adjustments applied.
	// +optional
	TotalAdjustments int64 `json:"totalAdjustments,omitempty"`

	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// VPAStatus holds information about VPA integration state.
	// +optional
	VPAStatus *VPAIntegrationStatus `json:"vpaStatus,omitempty"`

	// NamespaceConstraints reports detected LimitRange and ResourceQuota
	// boundaries that Podfather respects when making recommendations.
	// +optional
	NamespaceConstraints *NamespaceConstraintsStatus `json:"namespaceConstraints,omitempty"`

	// WorkloadGroups reports per-workload aggregation results. Each entry
	// describes one workload (Deployment, StatefulSet, etc.) and the
	// aggregate recommendation computed for it.
	// +optional
	WorkloadGroups []WorkloadGroupStatus `json:"workloadGroups,omitempty"`
}

// VPAIntegrationStatus reports the current state of VPA integration.
type VPAIntegrationStatus struct {
	// VPAInstalled indicates whether VPA CRDs are present in the cluster.
	// +optional
	VPAInstalled bool `json:"vpaInstalled,omitempty"`

	// MatchingVPAName is the name of the VPA object that targets the same workload.
	// Empty if no matching VPA is found.
	// +optional
	MatchingVPAName string `json:"matchingVPAName,omitempty"`

	// VPAUpdateMode reports the updateMode of the matched VPA.
	// +optional
	VPAUpdateMode string `json:"vpaUpdateMode,omitempty"`

	// UsingVPARecommendations is true when Podfather is actively using VPA
	// recommendations instead of its own algorithm.
	// +optional
	UsingVPARecommendations bool `json:"usingVPARecommendations,omitempty"`

	// VPAConflict describes any detected conflict with VPA (e.g. VPA not in Off mode).
	// Empty when there is no conflict.
	// +optional
	VPAConflict string `json:"vpaConflict,omitempty"`
}

// NamespaceConstraintsStatus reports detected LimitRange and ResourceQuota
// boundaries from the namespace.
type NamespaceConstraintsStatus struct {
	// LimitRangeFound is true when at least one LimitRange exists in the namespace.
	// +optional
	LimitRangeFound bool `json:"limitRangeFound,omitempty"`

	// ResourceQuotaFound is true when at least one ResourceQuota exists in the namespace.
	// +optional
	ResourceQuotaFound bool `json:"resourceQuotaFound,omitempty"`

	// EffectiveMinCPU is the effective minimum CPU (millicores) derived from LimitRange.
	// +optional
	EffectiveMinCPU string `json:"effectiveMinCPU,omitempty"`

	// EffectiveMaxCPU is the effective maximum CPU (millicores) derived from LimitRange.
	// +optional
	EffectiveMaxCPU string `json:"effectiveMaxCPU,omitempty"`

	// EffectiveMinMemory is the effective minimum memory derived from LimitRange.
	// +optional
	EffectiveMinMemory string `json:"effectiveMinMemory,omitempty"`

	// EffectiveMaxMemory is the effective maximum memory derived from LimitRange.
	// +optional
	EffectiveMaxMemory string `json:"effectiveMaxMemory,omitempty"`

	// ClampReasons lists human-readable descriptions of any recommendation
	// adjustments made due to namespace constraints in the last evaluation.
	// +optional
	ClampReasons []string `json:"clampReasons,omitempty"`
}

// WorkloadGroupStatus reports aggregation results for a single workload group.
type WorkloadGroupStatus struct {
	// Kind is the workload kind (e.g. "Deployment", "StatefulSet", "BarePod").
	Kind string `json:"kind"`

	// Name is the workload name.
	Name string `json:"name"`

	// Replicas is the number of running pods in this group.
	Replicas int32 `json:"replicas"`

	// Strategy is the remediation strategy used for this group.
	Strategy RemediationStrategy `json:"strategy"`

	// ContainerRecommendations lists the aggregated recommendation per container.
	// +optional
	ContainerRecommendations []ContainerRecommendation `json:"containerRecommendations,omitempty"`
}

// Recommendation holds per-container resource recommendation data.
type Recommendation struct {
	// ContainerRecommendations is a list of resource recommendations, one per container.
	// +optional
	ContainerRecommendations []ContainerRecommendation `json:"containerRecommendations,omitempty"`
}

// ContainerRecommendation is the recommended resources for a single container.
type ContainerRecommendation struct {
	// ContainerName is the name of the container.
	ContainerName string `json:"containerName"`

	// Target is the recommended resource requests.
	Target corev1.ResourceList `json:"target"`

	// LowerBound is the lower bound of the recommendation range.
	// +optional
	LowerBound corev1.ResourceList `json:"lowerBound,omitempty"`

	// UpperBound is the upper bound of the recommendation range (recommended limits).
	// +optional
	UpperBound corev1.ResourceList `json:"upperBound,omitempty"`
}

// ---- Root Types ----

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pa
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Monitored",type=integer,JSONPath=`.status.monitoredPods`
// +kubebuilder:printcolumn:name="Adjustments",type=integer,JSONPath=`.status.totalAdjustments`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PodAutoscaler is the Schema for the podautoscalers API.
// It defines a policy for automatically adjusting pod resource requests and limits
// based on observed usage metrics collected from the Kubernetes Metrics API.
type PodAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodAutoscalerSpec   `json:"spec,omitempty"`
	Status PodAutoscalerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodAutoscalerList contains a list of PodAutoscaler.
type PodAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodAutoscaler{}, &PodAutoscalerList{})
}
