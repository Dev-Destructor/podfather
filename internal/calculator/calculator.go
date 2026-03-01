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

// Package calculator provides pure business logic for computing optimal pod
// resource recommendations. It has zero Kubernetes dependencies — it operates
// entirely on plain Go structs — making it trivially unit-testable.
//
// The central function is [Calculate], which takes current resource usage
// metrics and current allocations and returns a recommendation (new requests
// and limits) together with a boolean indicating whether the deviation from
// the current allocation is significant enough to warrant an update.
//
// # Algorithm Overview
//
//	cpuBase  = max(averageUsage, peakUsage × PeakDampeningCPU)
//	cpuReq   = cpuBase × (1 + CPURequestMarginPercent / 100)
//	cpuReq   = clamp(cpuReq, minCPU, maxCPU)
//	cpuLimit = cpuReq  × LimitToRequestRatio
//	cpuLimit = clamp(cpuLimit, minCPU, maxCPU)
//
// Memory uses the same formula with different dampening and margin values.
package calculator

import (
	"fmt"
	"math"
)

// ---- Input / Output Structs ----

// ResourceUsage holds observed resource consumption for a single container.
type ResourceUsage struct {
	// ContainerName identifies the container these metrics belong to.
	ContainerName string
	// CPUCoresAvg is the average CPU usage in cores over the evaluation window.
	CPUCoresAvg float64
	// CPUCoresPeak is the peak CPU usage in cores observed in the window.
	CPUCoresPeak float64
	// MemoryBytesAvg is the average memory usage in bytes.
	MemoryBytesAvg float64
	// MemoryBytesPeak is the peak memory usage in bytes.
	MemoryBytesPeak float64
}

// ResourceAllocation represents the current resource requests/limits for a container.
type ResourceAllocation struct {
	// ContainerName identifies the container.
	ContainerName string
	// CPUCoresRequest is the current CPU request in cores.
	CPUCoresRequest float64
	// CPUCoresLimit is the current CPU limit in cores.
	CPUCoresLimit float64
	// MemoryBytesRequest is the current memory request in bytes.
	MemoryBytesRequest float64
	// MemoryBytesLimit is the current memory limit in bytes.
	MemoryBytesLimit float64
}

// ResourceRecommendation is the output of [Calculate]: optimal requests/limits
// and the variance (% deviation) from the current allocation.
type ResourceRecommendation struct {
	// ContainerName identifies the container.
	ContainerName string
	// CPUCoresRequest is the recommended CPU request in cores.
	CPUCoresRequest float64
	// CPUCoresLimit is the recommended CPU limit in cores.
	CPUCoresLimit float64
	// MemoryBytesRequest is the recommended memory request in bytes.
	MemoryBytesRequest float64
	// MemoryBytesLimit is the recommended memory limit in bytes.
	MemoryBytesLimit float64
	// CPUVariancePercent is the deviation of the recommendation from current request.
	CPUVariancePercent float64
	// MemoryVariancePercent is the deviation of the recommendation from current request.
	MemoryVariancePercent float64
	// SignificantVariance is true when either variance exceeds the configured threshold.
	SignificantVariance bool
}

// ---- Configuration ----

// Config tunes the calculation algorithm.
type Config struct {
	// CPURequestMarginPercent adds a safety margin on top of the CPU base calculation.
	// Default: 20 (= 20 %).
	CPURequestMarginPercent float64
	// MemoryRequestMarginPercent adds a safety margin on top of the memory base.
	// Default: 25 (memory is inelastic — OOMKill is worse than CPU throttle).
	MemoryRequestMarginPercent float64
	// LimitToRequestRatio sets the limit as a multiple of the request.
	// Default: 1.5 (limit = 150 % of request).
	LimitToRequestRatio float64
	// PeakDampeningCPU scales down the peak before comparing with the average.
	// Default: 0.8 (use 80 % of peak CPU; bursty workloads have extreme but brief spikes).
	PeakDampeningCPU float64
	// PeakDampeningMemory scales down the peak for memory.
	// Default: 0.9 (less aggressive than CPU because OOMKill is destructive).
	PeakDampeningMemory float64
	// VarianceThresholdPercent is the minimum % deviation to consider significant.
	// Default: 15.
	VarianceThresholdPercent float64
}

// DefaultConfig returns sensible production defaults.
func DefaultConfig() Config {
	return Config{
		CPURequestMarginPercent:    20,
		MemoryRequestMarginPercent: 25,
		LimitToRequestRatio:        1.5,
		PeakDampeningCPU:           0.8,
		PeakDampeningMemory:        0.9,
		VarianceThresholdPercent:   15,
	}
}

// Constraints define the hard boundaries for recommendations.
type Constraints struct {
	MinCPUCores    float64
	MaxCPUCores    float64
	MinMemoryBytes float64
	MaxMemoryBytes float64
}

// DefaultConstraints returns default min/max constraints.
func DefaultConstraints() Constraints {
	return Constraints{
		MinCPUCores:    0.01,                    // 10m
		MaxCPUCores:    16.0,                    // 16 cores
		MinMemoryBytes: 16 * 1024 * 1024,        // 16 MiB
		MaxMemoryBytes: 64 * 1024 * 1024 * 1024, // 64 GiB
	}
}

// ---- Core Calculation ----

// Calculate computes the optimal resource recommendation for a single container.
//
// It returns a [ResourceRecommendation] and an error. The recommendation always
// contains the calculated values even if SignificantVariance is false.
//
// An error is returned only for invalid inputs (empty ContainerName).
func Calculate(usage ResourceUsage, alloc ResourceAllocation, cfg Config, constraints Constraints) (ResourceRecommendation, error) {
	if usage.ContainerName == "" {
		return ResourceRecommendation{}, fmt.Errorf("container name must not be empty")
	}

	// --- CPU ---
	cpuBase := math.Max(usage.CPUCoresAvg, usage.CPUCoresPeak*cfg.PeakDampeningCPU)
	cpuReq := cpuBase * (1 + cfg.CPURequestMarginPercent/100)
	cpuReq = clamp(cpuReq, constraints.MinCPUCores, constraints.MaxCPUCores)
	cpuLim := cpuReq * cfg.LimitToRequestRatio
	cpuLim = clamp(cpuLim, constraints.MinCPUCores, constraints.MaxCPUCores)

	// --- Memory ---
	memBase := math.Max(usage.MemoryBytesAvg, usage.MemoryBytesPeak*cfg.PeakDampeningMemory)
	memReq := memBase * (1 + cfg.MemoryRequestMarginPercent/100)
	memReq = clamp(memReq, constraints.MinMemoryBytes, constraints.MaxMemoryBytes)
	memLim := memReq * cfg.LimitToRequestRatio
	memLim = clamp(memLim, constraints.MinMemoryBytes, constraints.MaxMemoryBytes)

	// --- Variance ---
	cpuVar := variancePercent(cpuReq, alloc.CPUCoresRequest)
	memVar := variancePercent(memReq, alloc.MemoryBytesRequest)

	sig := cpuVar >= cfg.VarianceThresholdPercent || memVar >= cfg.VarianceThresholdPercent

	return ResourceRecommendation{
		ContainerName:         usage.ContainerName,
		CPUCoresRequest:       cpuReq,
		CPUCoresLimit:         cpuLim,
		MemoryBytesRequest:    memReq,
		MemoryBytesLimit:      memLim,
		CPUVariancePercent:    cpuVar,
		MemoryVariancePercent: memVar,
		SignificantVariance:   sig,
	}, nil
}

// ---- Helpers ----

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func variancePercent(recommended, current float64) float64 {
	if current == 0 {
		return 100 // treat zero allocation as maximally variant
	}
	return math.Abs(recommended-current) / current * 100
}
