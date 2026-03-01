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

package namespacelimits

import (
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ---- ClampRecommendation tests ----

func TestClampRecommendation_NoConstraints(t *testing.T) {
	nc := EmptyConstraints()
	cpuReq, cpuLim, memReq, memLim, reasons := ClampRecommendation(
		0.5, 1.0, 256*1024*1024, 512*1024*1024, nc)

	if cpuReq != 0.5 || cpuLim != 1.0 {
		t.Errorf("CPU values changed unexpectedly: req=%.3f lim=%.3f", cpuReq, cpuLim)
	}
	if memReq != 256*1024*1024 || memLim != 512*1024*1024 {
		t.Errorf("Memory values changed unexpectedly: req=%.0f lim=%.0f", memReq, memLim)
	}
	if len(reasons) != 0 {
		t.Errorf("Expected no reasons, got %v", reasons)
	}
}

func TestClampRecommendation_LimitRangeMin(t *testing.T) {
	nc := EmptyConstraints()
	nc.MinCPUCores = 0.1                 // 100m
	nc.MinMemoryBytes = 64 * 1024 * 1024 // 64Mi

	cpuReq, _, memReq, _, reasons := ClampRecommendation(
		0.05, 0.5, 32*1024*1024, 128*1024*1024, nc)

	if cpuReq != 0.1 {
		t.Errorf("CPU request not raised to LimitRange min: got %.3f, want 0.1", cpuReq)
	}
	if memReq != 64*1024*1024 {
		t.Errorf("Memory request not raised to LimitRange min: got %.0f", memReq)
	}
	if len(reasons) != 2 {
		t.Errorf("Expected 2 reasons, got %d: %v", len(reasons), reasons)
	}
}

func TestClampRecommendation_LimitRangeMax(t *testing.T) {
	nc := EmptyConstraints()
	nc.MaxCPUCores = 2.0
	nc.MaxMemoryBytes = 1024 * 1024 * 1024 // 1Gi

	cpuReq, cpuLim, memReq, memLim, reasons := ClampRecommendation(
		4.0, 6.0, 2*1024*1024*1024, 4*1024*1024*1024, nc)

	if cpuReq != 2.0 {
		t.Errorf("CPU request not capped: got %.3f, want 2.0", cpuReq)
	}
	if cpuLim != 2.0 {
		t.Errorf("CPU limit not capped: got %.3f, want 2.0", cpuLim)
	}
	if memReq != 1024*1024*1024 {
		t.Errorf("Memory request not capped: got %.0f", memReq)
	}
	if memLim != 1024*1024*1024 {
		t.Errorf("Memory limit not capped: got %.0f", memLim)
	}
	if len(reasons) < 2 {
		t.Errorf("Expected at least 2 reasons, got %d: %v", len(reasons), reasons)
	}
}

func TestClampRecommendation_MaxLimitRequestRatio(t *testing.T) {
	nc := EmptyConstraints()
	nc.MaxLimitRequestRatioCPU = 2.0
	nc.MaxLimitRequestRatioMemory = 3.0

	// CPU: request=0.5, limit=2.0 → ratio 4.0, exceeds 2.0 → limit capped to 1.0
	// Mem: request=100Mi, limit=500Mi → ratio 5.0, exceeds 3.0 → limit capped to 300Mi
	cpuReq, cpuLim, memReq, memLim, reasons := ClampRecommendation(
		0.5, 2.0, 100*1024*1024, 500*1024*1024, nc)

	if cpuReq != 0.5 {
		t.Errorf("CPU request should not change: got %.3f", cpuReq)
	}
	expectedCPULim := 0.5 * 2.0 // 1.0
	if cpuLim != expectedCPULim {
		t.Errorf("CPU limit not capped by ratio: got %.3f, want %.3f", cpuLim, expectedCPULim)
	}

	if memReq != 100*1024*1024 {
		t.Errorf("Memory request should not change: got %.0f", memReq)
	}
	expectedMemLim := float64(100*1024*1024) * 3.0
	if memLim != expectedMemLim {
		t.Errorf("Memory limit not capped by ratio: got %.0f, want %.0f", memLim, expectedMemLim)
	}
	if len(reasons) != 2 {
		t.Errorf("Expected 2 reasons, got %d: %v", len(reasons), reasons)
	}
}

func TestClampRecommendation_QuotaHeadroom(t *testing.T) {
	nc := EmptyConstraints()
	nc.QuotaCPURemaining = 1.0
	nc.QuotaMemoryRemaining = 512 * 1024 * 1024

	cpuReq, _, memReq, _, reasons := ClampRecommendation(
		3.0, 5.0, 2*1024*1024*1024, 4*1024*1024*1024, nc)

	if cpuReq != 1.0 {
		t.Errorf("CPU request not capped by quota: got %.3f, want 1.0", cpuReq)
	}
	if memReq != 512*1024*1024 {
		t.Errorf("Memory request not capped by quota: got %.0f", memReq)
	}
	if len(reasons) < 2 {
		t.Errorf("Expected at least 2 reasons, got %d: %v", len(reasons), reasons)
	}
}

func TestClampRecommendation_LimitNotBelowRequest(t *testing.T) {
	nc := EmptyConstraints()
	nc.MaxCPUCores = 0.5
	// Request 0.4, limit 0.3 after capping → should be raised to match request
	_, cpuLim, _, _, _ := ClampRecommendation(0.4, 0.3, 100*1024*1024, 200*1024*1024, nc)
	if cpuLim < 0.4 {
		t.Errorf("CPU limit should be >= request: got %.3f, req=0.4", cpuLim)
	}
}

// ---- ToCalculatorConstraints tests ----

func TestToCalculatorConstraints(t *testing.T) {
	nc := EmptyConstraints()
	nc.MinCPUCores = 0.05
	nc.MaxCPUCores = 4.0
	nc.MinMemoryBytes = 32 * 1024 * 1024
	nc.MaxMemoryBytes = 2 * 1024 * 1024 * 1024

	minCPU, maxCPU, minMem, maxMem := nc.ToCalculatorConstraints(
		0.01, 16.0, 16*1024*1024, 64*1024*1024*1024)

	if minCPU != 0.05 {
		t.Errorf("MinCPU should be tighter LimitRange value: got %.3f, want 0.05", minCPU)
	}
	if maxCPU != 4.0 {
		t.Errorf("MaxCPU should be tighter LimitRange value: got %.3f, want 4.0", maxCPU)
	}
	if minMem != 32*1024*1024 {
		t.Errorf("MinMem should be tighter LimitRange value: got %.0f", minMem)
	}
	if maxMem != 2*1024*1024*1024 {
		t.Errorf("MaxMem should be tighter LimitRange value: got %.0f", maxMem)
	}
}

func TestToCalculatorConstraints_DefaultsTighter(t *testing.T) {
	nc := EmptyConstraints() // all zeros / MaxFloat64

	minCPU, maxCPU, minMem, maxMem := nc.ToCalculatorConstraints(
		0.01, 16.0, 16*1024*1024, 64*1024*1024*1024)

	// Defaults should be returned unmodified when namespace has no constraints
	if minCPU != 0.01 {
		t.Errorf("MinCPU should be default: got %.3f, want 0.01", minCPU)
	}
	if maxCPU != 16.0 {
		t.Errorf("MaxCPU should be default: got %.3f, want 16.0", maxCPU)
	}
	if minMem != 16*1024*1024 {
		t.Errorf("MinMem should be default: got %.0f", minMem)
	}
	if maxMem != 64*1024*1024*1024 {
		t.Errorf("MaxMem should be default: got %.0f", maxMem)
	}
}

func TestToCalculatorConstraints_MinExceedsMax(t *testing.T) {
	nc := EmptyConstraints()
	nc.MinCPUCores = 5.0
	nc.MaxCPUCores = 2.0 // impossible LimitRange - min > max

	minCPU, maxCPU, _, _ := nc.ToCalculatorConstraints(0.01, 16.0, 16*1024*1024, 64*1024*1024*1024)

	// Should collapse min to max
	if minCPU > maxCPU {
		t.Errorf("MinCPU (%f) should not exceed MaxCPU (%f)", minCPU, maxCPU)
	}
}

// ---- LimitRange parsing tests ----

func TestApplyContainerLimitRangeItem(t *testing.T) {
	nc := EmptyConstraints()

	item := corev1.LimitRangeItem{
		Type: corev1.LimitTypeContainer,
		Min: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Max: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
		Default: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		DefaultRequest: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		MaxLimitRequestRatio: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("3"),
		},
	}

	applyContainerLimitRangeItem(&nc, item)

	if nc.MinCPUCores != 0.1 {
		t.Errorf("MinCPUCores = %.3f, want 0.1", nc.MinCPUCores)
	}
	if nc.MaxCPUCores != 4.0 {
		t.Errorf("MaxCPUCores = %.3f, want 4.0", nc.MaxCPUCores)
	}
	if nc.DefaultCPULimit != 0.5 {
		t.Errorf("DefaultCPULimit = %.3f, want 0.5", nc.DefaultCPULimit)
	}
	if nc.DefaultCPURequest != 0.2 {
		t.Errorf("DefaultCPURequest = %.3f, want 0.2", nc.DefaultCPURequest)
	}
	if nc.MaxLimitRequestRatioCPU != 3.0 {
		t.Errorf("MaxLimitRequestRatioCPU = %.1f, want 3.0", nc.MaxLimitRequestRatioCPU)
	}
}

func TestApplyPodLimitRangeItem(t *testing.T) {
	nc := EmptyConstraints()

	item := corev1.LimitRangeItem{
		Type: corev1.LimitTypePod,
		Max: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("8"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
	}

	applyPodLimitRangeItem(&nc, item)

	if nc.PodMaxCPUCores != 8.0 {
		t.Errorf("PodMaxCPUCores = %.1f, want 8.0", nc.PodMaxCPUCores)
	}
	if nc.PodMaxMemoryBytes != 16*1024*1024*1024 {
		t.Errorf("PodMaxMemoryBytes = %.0f, want %.0f", nc.PodMaxMemoryBytes, float64(16*1024*1024*1024))
	}
}

// ---- ResourceQuota parsing tests ----

func TestComputeRemaining(t *testing.T) {
	rq := corev1.ResourceQuota{
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:    resource.MustParse("10"),
				corev1.ResourceRequestsMemory: resource.MustParse("20Gi"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU:    resource.MustParse("7"),
				corev1.ResourceRequestsMemory: resource.MustParse("15Gi"),
			},
		},
	}

	cpuRemaining := computeRemaining(rq, corev1.ResourceRequestsCPU, corev1.ResourceCPU)
	if math.Abs(cpuRemaining-3.0) > 0.001 {
		t.Errorf("CPU remaining = %.3f, want 3.0", cpuRemaining)
	}

	memRemaining := computeRemaining(rq, corev1.ResourceRequestsMemory, corev1.ResourceMemory)
	expectedMem := float64(5 * 1024 * 1024 * 1024) // 5Gi
	if math.Abs(memRemaining-expectedMem) > 1024 {
		t.Errorf("Memory remaining = %.0f, want %.0f", memRemaining, expectedMem)
	}
}

func TestComputeRemaining_NoQuota(t *testing.T) {
	rq := corev1.ResourceQuota{}
	remaining := computeRemaining(rq, corev1.ResourceRequestsCPU, corev1.ResourceCPU)
	if remaining != math.MaxFloat64 {
		t.Errorf("Expected MaxFloat64 for missing quota, got %.3f", remaining)
	}
}

// ---- EmptyConstraints sanity ----

func TestEmptyConstraints(t *testing.T) {
	nc := EmptyConstraints()
	if nc.MinCPUCores != 0 {
		t.Errorf("MinCPUCores should be 0, got %.3f", nc.MinCPUCores)
	}
	if nc.MaxCPUCores != math.MaxFloat64 {
		t.Errorf("MaxCPUCores should be MaxFloat64, got %.3f", nc.MaxCPUCores)
	}
	if nc.QuotaCPURemaining != math.MaxFloat64 {
		t.Errorf("QuotaCPURemaining should be MaxFloat64")
	}
}
