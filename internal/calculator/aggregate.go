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

package calculator

import (
	"fmt"
	"math"
	"sort"
)

// AggregationStrategy mirrors the CRD RemediationStrategy for use in the
// calculator package (no Kubernetes dependencies).
type AggregationStrategy string

const (
	// AggregateMax uses the maximum observed value across all pods for each
	// metric field. Conservative: prevents starvation of the busiest replica.
	AggregateMax AggregationStrategy = "MaxPod"
	// AggregateMin uses the minimum observed value across all pods.
	// Aggressive: saves resources but risks throttling busier pods.
	AggregateMin AggregationStrategy = "MinPod"
	// AggregateAuto uses the 90th percentile (P90) of observed values.
	// Balanced: covers most workloads without extreme over/under-provisioning.
	AggregateAuto AggregationStrategy = "Auto"
)

// AggregateUsage takes per-pod ResourceUsage samples for the **same container**
// and produces a single aggregated ResourceUsage using the given strategy.
//
// All samples must have the same ContainerName. If len(samples) == 0 an error
// is returned. For a single sample the input is returned as-is regardless of
// strategy.
func AggregateUsage(samples []ResourceUsage, strategy AggregationStrategy) (ResourceUsage, error) {
	if len(samples) == 0 {
		return ResourceUsage{}, fmt.Errorf("no samples to aggregate")
	}

	containerName := samples[0].ContainerName
	if containerName == "" {
		return ResourceUsage{}, fmt.Errorf("container name must not be empty")
	}

	// Degenerate case — single pod, nothing to aggregate.
	if len(samples) == 1 {
		return samples[0], nil
	}

	// Validate all samples are for the same container.
	for i := 1; i < len(samples); i++ {
		if samples[i].ContainerName != containerName {
			return ResourceUsage{}, fmt.Errorf(
				"mismatched container names in aggregation: %q vs %q",
				containerName, samples[i].ContainerName)
		}
	}

	switch strategy {
	case AggregateMax:
		return aggregateMax(samples, containerName), nil
	case AggregateMin:
		return aggregateMin(samples, containerName), nil
	case AggregateAuto:
		return aggregateP90(samples, containerName), nil
	default:
		return ResourceUsage{}, fmt.Errorf("unknown aggregation strategy: %q", strategy)
	}
}

// AggregateAllocations takes per-pod ResourceAllocation samples for the same
// container and returns the allocation that best represents the "current state"
// for variance comparison. We use the max of each field — this represents the
// highest currently-provisioned replica, which is the baseline the workload
// template already has.
func AggregateAllocations(samples []ResourceAllocation) (ResourceAllocation, error) {
	if len(samples) == 0 {
		return ResourceAllocation{}, fmt.Errorf("no allocation samples to aggregate")
	}
	if len(samples) == 1 {
		return samples[0], nil
	}
	result := samples[0]
	for i := 1; i < len(samples); i++ {
		result.CPUCoresRequest = math.Max(result.CPUCoresRequest, samples[i].CPUCoresRequest)
		result.CPUCoresLimit = math.Max(result.CPUCoresLimit, samples[i].CPUCoresLimit)
		result.MemoryBytesRequest = math.Max(result.MemoryBytesRequest, samples[i].MemoryBytesRequest)
		result.MemoryBytesLimit = math.Max(result.MemoryBytesLimit, samples[i].MemoryBytesLimit)
	}
	return result, nil
}

// ---- aggregation implementations ----

func aggregateMax(samples []ResourceUsage, containerName string) ResourceUsage {
	result := ResourceUsage{ContainerName: containerName}
	for _, s := range samples {
		result.CPUCoresAvg = math.Max(result.CPUCoresAvg, s.CPUCoresAvg)
		result.CPUCoresPeak = math.Max(result.CPUCoresPeak, s.CPUCoresPeak)
		result.MemoryBytesAvg = math.Max(result.MemoryBytesAvg, s.MemoryBytesAvg)
		result.MemoryBytesPeak = math.Max(result.MemoryBytesPeak, s.MemoryBytesPeak)
	}
	return result
}

func aggregateMin(samples []ResourceUsage, containerName string) ResourceUsage {
	result := ResourceUsage{
		ContainerName:   containerName,
		CPUCoresAvg:     math.MaxFloat64,
		CPUCoresPeak:    math.MaxFloat64,
		MemoryBytesAvg:  math.MaxFloat64,
		MemoryBytesPeak: math.MaxFloat64,
	}
	for _, s := range samples {
		result.CPUCoresAvg = math.Min(result.CPUCoresAvg, s.CPUCoresAvg)
		result.CPUCoresPeak = math.Min(result.CPUCoresPeak, s.CPUCoresPeak)
		result.MemoryBytesAvg = math.Min(result.MemoryBytesAvg, s.MemoryBytesAvg)
		result.MemoryBytesPeak = math.Min(result.MemoryBytesPeak, s.MemoryBytesPeak)
	}
	return result
}

func aggregateP90(samples []ResourceUsage, containerName string) ResourceUsage {
	cpuAvgs := make([]float64, len(samples))
	cpuPeaks := make([]float64, len(samples))
	memAvgs := make([]float64, len(samples))
	memPeaks := make([]float64, len(samples))

	for i, s := range samples {
		cpuAvgs[i] = s.CPUCoresAvg
		cpuPeaks[i] = s.CPUCoresPeak
		memAvgs[i] = s.MemoryBytesAvg
		memPeaks[i] = s.MemoryBytesPeak
	}

	return ResourceUsage{
		ContainerName:   containerName,
		CPUCoresAvg:     percentile(cpuAvgs, 0.90),
		CPUCoresPeak:    percentile(cpuPeaks, 0.90),
		MemoryBytesAvg:  percentile(memAvgs, 0.90),
		MemoryBytesPeak: percentile(memPeaks, 0.90),
	}
}

// percentile returns the p-th percentile of a sorted copy of vals.
// Uses the nearest-rank method: index = ceil(p * n) - 1, clamped to [0, n-1].
func percentile(vals []float64, p float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	sort.Float64s(sorted)

	rank := int(math.Ceil(p*float64(n))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= n {
		rank = n - 1
	}
	return sorted[rank]
}
