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

// Package namespacelimits provides helpers for fetching and interpreting
// Kubernetes LimitRange and ResourceQuota objects so that Podfather can
// clamp its resource recommendations to values the API server will accept.
//
// The two relevant Kubernetes objects are:
//
//   - LimitRange: enforces per-container min/max/default requests and limits.
//     If Podfather recommends values outside these bounds, the API server
//     rejects the pod mutation.
//
//   - ResourceQuota: enforces aggregate resource budgets per namespace.
//     If a resize would cause total used resources to exceed the quota,
//     the update is rejected.
//
// This package fetches both objects and exposes a unified
// [NamespaceConstraints] struct that the calculator and controller can use
// to pre-clamp recommendations.
package namespacelimits

import (
	"context"
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NamespaceConstraints holds the effective resource boundaries derived from
// LimitRange and ResourceQuota objects in a namespace.
type NamespaceConstraints struct {
	// LimitRangeFound indicates whether at least one LimitRange was found.
	LimitRangeFound bool
	// ResourceQuotaFound indicates whether at least one ResourceQuota was found.
	ResourceQuotaFound bool

	// ---- Per-container bounds from LimitRange (type=Container) ----

	// MinCPUCores is the minimum CPU request a container must have.
	MinCPUCores float64
	// MaxCPUCores is the maximum CPU limit a container may have.
	MaxCPUCores float64
	// MinMemoryBytes is the minimum memory request a container must have.
	MinMemoryBytes float64
	// MaxMemoryBytes is the maximum memory limit a container may have.
	MaxMemoryBytes float64

	// DefaultCPURequest is the LimitRange default CPU request (used when none is set).
	DefaultCPURequest float64
	// DefaultCPULimit is the LimitRange default CPU limit.
	DefaultCPULimit float64
	// DefaultMemoryRequest is the LimitRange default memory request.
	DefaultMemoryRequest float64
	// DefaultMemoryLimit is the LimitRange default memory limit.
	DefaultMemoryLimit float64

	// MaxLimitRequestRatioCPU is the LimitRange maxLimitRequestRatio for CPU.
	// Zero means unconstrained.
	MaxLimitRequestRatioCPU float64
	// MaxLimitRequestRatioMemory is the LimitRange maxLimitRequestRatio for memory.
	MaxLimitRequestRatioMemory float64

	// ---- Pod-level bounds from LimitRange (type=Pod) ----

	// PodMaxCPUCores is the maximum total CPU for all containers in a pod.
	PodMaxCPUCores float64
	// PodMaxMemoryBytes is the maximum total memory for all containers in a pod.
	PodMaxMemoryBytes float64

	// ---- Namespace-level quota headroom ----

	// QuotaCPURemaining is the remaining CPU (cores) before hitting the namespace quota.
	// A value of math.MaxFloat64 means no quota or unlimited.
	QuotaCPURemaining float64
	// QuotaMemoryRemaining is the remaining memory (bytes) before hitting the namespace quota.
	QuotaMemoryRemaining float64
}

// EmptyConstraints returns constraints with no limits applied (all maximums at math.MaxFloat64).
func EmptyConstraints() NamespaceConstraints {
	return NamespaceConstraints{
		MaxCPUCores:          math.MaxFloat64,
		MaxMemoryBytes:       math.MaxFloat64,
		PodMaxCPUCores:       math.MaxFloat64,
		PodMaxMemoryBytes:    math.MaxFloat64,
		QuotaCPURemaining:    math.MaxFloat64,
		QuotaMemoryRemaining: math.MaxFloat64,
	}
}

// Fetcher retrieves LimitRange and ResourceQuota data from namespace.
type Fetcher struct {
	client client.Client
}

// NewFetcher creates a new Fetcher.
func NewFetcher(c client.Client) *Fetcher {
	return &Fetcher{client: c}
}

// GetNamespaceConstraints fetches all LimitRanges and ResourceQuotas in the
// given namespace and returns a merged [NamespaceConstraints].
func (f *Fetcher) GetNamespaceConstraints(ctx context.Context, namespace string) (NamespaceConstraints, error) {
	nc := EmptyConstraints()

	// ---- LimitRanges ----
	limitRangeList := &corev1.LimitRangeList{}
	if err := f.client.List(ctx, limitRangeList, client.InNamespace(namespace)); err != nil {
		return nc, fmt.Errorf("failed to list LimitRanges in namespace %s: %w", namespace, err)
	}

	if len(limitRangeList.Items) > 0 {
		nc.LimitRangeFound = true
		for _, lr := range limitRangeList.Items {
			for _, item := range lr.Spec.Limits {
				switch item.Type {
				case corev1.LimitTypeContainer:
					applyContainerLimitRangeItem(&nc, item)
				case corev1.LimitTypePod:
					applyPodLimitRangeItem(&nc, item)
				}
			}
		}
	}

	// ---- ResourceQuotas ----
	quotaList := &corev1.ResourceQuotaList{}
	if err := f.client.List(ctx, quotaList, client.InNamespace(namespace)); err != nil {
		return nc, fmt.Errorf("failed to list ResourceQuotas in namespace %s: %w", namespace, err)
	}

	if len(quotaList.Items) > 0 {
		nc.ResourceQuotaFound = true
		for _, rq := range quotaList.Items {
			applyResourceQuota(&nc, rq)
		}
	}

	return nc, nil
}

// ---- LimitRange helpers ----

func applyContainerLimitRangeItem(nc *NamespaceConstraints, item corev1.LimitRangeItem) {
	// Min
	if cpu, ok := item.Min[corev1.ResourceCPU]; ok {
		v := cpu.AsApproximateFloat64()
		if v > nc.MinCPUCores {
			nc.MinCPUCores = v
		}
	}
	if mem, ok := item.Min[corev1.ResourceMemory]; ok {
		v := mem.AsApproximateFloat64()
		if v > nc.MinMemoryBytes {
			nc.MinMemoryBytes = v
		}
	}

	// Max
	if cpu, ok := item.Max[corev1.ResourceCPU]; ok {
		v := cpu.AsApproximateFloat64()
		if v < nc.MaxCPUCores {
			nc.MaxCPUCores = v
		}
	}
	if mem, ok := item.Max[corev1.ResourceMemory]; ok {
		v := mem.AsApproximateFloat64()
		if v < nc.MaxMemoryBytes {
			nc.MaxMemoryBytes = v
		}
	}

	// Default (limits)
	if cpu, ok := item.Default[corev1.ResourceCPU]; ok {
		nc.DefaultCPULimit = cpu.AsApproximateFloat64()
	}
	if mem, ok := item.Default[corev1.ResourceMemory]; ok {
		nc.DefaultMemoryLimit = mem.AsApproximateFloat64()
	}

	// DefaultRequest
	if cpu, ok := item.DefaultRequest[corev1.ResourceCPU]; ok {
		nc.DefaultCPURequest = cpu.AsApproximateFloat64()
	}
	if mem, ok := item.DefaultRequest[corev1.ResourceMemory]; ok {
		nc.DefaultMemoryRequest = mem.AsApproximateFloat64()
	}

	// MaxLimitRequestRatio
	if cpu, ok := item.MaxLimitRequestRatio[corev1.ResourceCPU]; ok {
		nc.MaxLimitRequestRatioCPU = cpu.AsApproximateFloat64()
	}
	if mem, ok := item.MaxLimitRequestRatio[corev1.ResourceMemory]; ok {
		nc.MaxLimitRequestRatioMemory = mem.AsApproximateFloat64()
	}
}

func applyPodLimitRangeItem(nc *NamespaceConstraints, item corev1.LimitRangeItem) {
	if cpu, ok := item.Max[corev1.ResourceCPU]; ok {
		v := cpu.AsApproximateFloat64()
		if v < nc.PodMaxCPUCores {
			nc.PodMaxCPUCores = v
		}
	}
	if mem, ok := item.Max[corev1.ResourceMemory]; ok {
		v := mem.AsApproximateFloat64()
		if v < nc.PodMaxMemoryBytes {
			nc.PodMaxMemoryBytes = v
		}
	}
}

// ---- ResourceQuota helpers ----

func applyResourceQuota(nc *NamespaceConstraints, rq corev1.ResourceQuota) {
	// CPU requests remaining
	cpuRemaining := computeRemaining(rq, corev1.ResourceRequestsCPU, corev1.ResourceCPU)
	if cpuRemaining < nc.QuotaCPURemaining {
		nc.QuotaCPURemaining = cpuRemaining
	}

	// Memory requests remaining
	memRemaining := computeRemaining(rq, corev1.ResourceRequestsMemory, corev1.ResourceMemory)
	if memRemaining < nc.QuotaMemoryRemaining {
		nc.QuotaMemoryRemaining = memRemaining
	}
}

// computeRemaining calculates how much headroom the namespace quota has for a specific resource.
// It checks both the explicit "requests.<resource>" key and the plain "<resource>" key.
func computeRemaining(rq corev1.ResourceQuota, requestsKey, plainKey corev1.ResourceName) float64 {
	remaining := math.MaxFloat64

	for _, key := range []corev1.ResourceName{requestsKey, plainKey} {
		hard, hardOK := rq.Status.Hard[key]
		used, usedOK := rq.Status.Used[key]
		if hardOK && usedOK {
			h := hard.AsApproximateFloat64()
			u := used.AsApproximateFloat64()
			r := h - u
			if r < remaining {
				remaining = r
			}
		}
	}

	return remaining
}

// ---- Recommendation clamping ----

// ClampRecommendation adjusts a resource recommendation to comply with namespace
// constraints. It returns the clamped values and a list of human-readable reasons
// describing any adjustments that were made.
func ClampRecommendation(
	cpuReq, cpuLim, memReq, memLim float64,
	nc NamespaceConstraints,
) (newCPUReq, newCPULim, newMemReq, newMemLim float64, reasons []string) {
	newCPUReq = cpuReq
	newCPULim = cpuLim
	newMemReq = memReq
	newMemLim = memLim

	// ---- LimitRange min ----
	if nc.MinCPUCores > 0 && newCPUReq < nc.MinCPUCores {
		reasons = append(reasons, fmt.Sprintf(
			"CPU request raised from %.3f to %.3f cores (LimitRange min)",
			newCPUReq, nc.MinCPUCores))
		newCPUReq = nc.MinCPUCores
	}
	if nc.MinMemoryBytes > 0 && newMemReq < nc.MinMemoryBytes {
		reasons = append(reasons, fmt.Sprintf(
			"memory request raised from %.0f to %.0f bytes (LimitRange min)",
			newMemReq, nc.MinMemoryBytes))
		newMemReq = nc.MinMemoryBytes
	}

	// ---- LimitRange max ----
	if nc.MaxCPUCores < math.MaxFloat64 && newCPULim > nc.MaxCPUCores {
		reasons = append(reasons, fmt.Sprintf(
			"CPU limit capped from %.3f to %.3f cores (LimitRange max)",
			newCPULim, nc.MaxCPUCores))
		newCPULim = nc.MaxCPUCores
	}
	if nc.MaxCPUCores < math.MaxFloat64 && newCPUReq > nc.MaxCPUCores {
		reasons = append(reasons, fmt.Sprintf(
			"CPU request capped from %.3f to %.3f cores (LimitRange max)",
			newCPUReq, nc.MaxCPUCores))
		newCPUReq = nc.MaxCPUCores
	}
	if nc.MaxMemoryBytes < math.MaxFloat64 && newMemLim > nc.MaxMemoryBytes {
		reasons = append(reasons, fmt.Sprintf(
			"memory limit capped from %.0f to %.0f bytes (LimitRange max)",
			newMemLim, nc.MaxMemoryBytes))
		newMemLim = nc.MaxMemoryBytes
	}
	if nc.MaxMemoryBytes < math.MaxFloat64 && newMemReq > nc.MaxMemoryBytes {
		reasons = append(reasons, fmt.Sprintf(
			"memory request capped from %.0f to %.0f bytes (LimitRange max)",
			newMemReq, nc.MaxMemoryBytes))
		newMemReq = nc.MaxMemoryBytes
	}

	// ---- Ensure limit >= request ----
	if newCPULim < newCPUReq {
		newCPULim = newCPUReq
	}
	if newMemLim < newMemReq {
		newMemLim = newMemReq
	}

	// ---- MaxLimitRequestRatio ----
	if nc.MaxLimitRequestRatioCPU > 0 && newCPUReq > 0 {
		maxAllowedLimit := newCPUReq * nc.MaxLimitRequestRatioCPU
		if newCPULim > maxAllowedLimit {
			reasons = append(reasons, fmt.Sprintf(
				"CPU limit capped from %.3f to %.3f cores (LimitRange maxLimitRequestRatio=%.1f)",
				newCPULim, maxAllowedLimit, nc.MaxLimitRequestRatioCPU))
			newCPULim = maxAllowedLimit
		}
	}
	if nc.MaxLimitRequestRatioMemory > 0 && newMemReq > 0 {
		maxAllowedLimit := newMemReq * nc.MaxLimitRequestRatioMemory
		if newMemLim > maxAllowedLimit {
			reasons = append(reasons, fmt.Sprintf(
				"memory limit capped from %.0f to %.0f bytes (LimitRange maxLimitRequestRatio=%.1f)",
				newMemLim, maxAllowedLimit, nc.MaxLimitRequestRatioMemory))
			newMemLim = maxAllowedLimit
		}
	}

	// ---- ResourceQuota headroom ----
	// Note: this is a best-effort pre-check. The actual quota enforcement is
	// done by the API server. We clamp to the remaining headroom so we don't
	// submit a request that is guaranteed to fail.
	if nc.QuotaCPURemaining < math.MaxFloat64 && newCPUReq > nc.QuotaCPURemaining && nc.QuotaCPURemaining > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"CPU request capped from %.3f to %.3f cores (ResourceQuota headroom)",
			newCPUReq, nc.QuotaCPURemaining))
		newCPUReq = nc.QuotaCPURemaining
		if newCPULim > newCPUReq*2 {
			newCPULim = newCPUReq * 2
		}
	}
	if nc.QuotaMemoryRemaining < math.MaxFloat64 && newMemReq > nc.QuotaMemoryRemaining && nc.QuotaMemoryRemaining > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"memory request capped from %.0f to %.0f bytes (ResourceQuota headroom)",
			newMemReq, nc.QuotaMemoryRemaining))
		newMemReq = nc.QuotaMemoryRemaining
		if newMemLim > newMemReq*2 {
			newMemLim = newMemReq * 2
		}
	}

	return newCPUReq, newCPULim, newMemReq, newMemLim, reasons
}

// ToCalculatorConstraints merges NamespaceConstraints with the calculator's
// existing defaults to produce a final Constraints struct. The tightest
// bounds win.
func (nc NamespaceConstraints) ToCalculatorConstraints(
	defaultMinCPU, defaultMaxCPU, defaultMinMem, defaultMaxMem float64,
) (minCPU, maxCPU, minMem, maxMem float64) {
	minCPU = defaultMinCPU
	maxCPU = defaultMaxCPU
	minMem = defaultMinMem
	maxMem = defaultMaxMem

	// LimitRange min overrides if tighter
	if nc.MinCPUCores > minCPU {
		minCPU = nc.MinCPUCores
	}
	if nc.MinMemoryBytes > minMem {
		minMem = nc.MinMemoryBytes
	}

	// LimitRange max overrides if tighter
	if nc.MaxCPUCores < maxCPU {
		maxCPU = nc.MaxCPUCores
	}
	if nc.MaxMemoryBytes < maxMem {
		maxMem = nc.MaxMemoryBytes
	}

	// Ensure min <= max (LimitRange may create impossible constraints; pick max)
	if minCPU > maxCPU {
		minCPU = maxCPU
	}
	if minMem > maxMem {
		minMem = maxMem
	}

	return minCPU, maxCPU, minMem, maxMem
}
