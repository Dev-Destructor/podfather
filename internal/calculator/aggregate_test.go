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
	"math"
	"testing"
)

func TestAggregateUsage(t *testing.T) {
	tests := []struct {
		name     string
		samples  []ResourceUsage
		strategy AggregationStrategy
		wantErr  bool
		check    func(t *testing.T, result ResourceUsage)
	}{
		{
			name:     "empty samples returns error",
			samples:  nil,
			strategy: AggregateAuto,
			wantErr:  true,
		},
		{
			name: "empty container name returns error",
			samples: []ResourceUsage{
				{ContainerName: ""},
			},
			strategy: AggregateAuto,
			wantErr:  true,
		},
		{
			name: "mismatched container names returns error",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.5},
				{ContainerName: "sidecar", CPUCoresAvg: 0.1},
			},
			strategy: AggregateMax,
			wantErr:  true,
		},
		{
			name: "single sample returns as-is for all strategies",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.5, CPUCoresPeak: 0.7, MemoryBytesAvg: 100, MemoryBytesPeak: 150},
			},
			strategy: AggregateAuto,
			check: func(t *testing.T, r ResourceUsage) {
				if r.CPUCoresAvg != 0.5 || r.CPUCoresPeak != 0.7 {
					t.Errorf("single sample should pass through: got cpu avg=%.2f peak=%.2f", r.CPUCoresAvg, r.CPUCoresPeak)
				}
			},
		},
		{
			name: "MaxPod picks highest value for each metric",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.2, CPUCoresPeak: 0.4, MemoryBytesAvg: 100, MemoryBytesPeak: 200},
				{ContainerName: "app", CPUCoresAvg: 0.8, CPUCoresPeak: 1.0, MemoryBytesAvg: 300, MemoryBytesPeak: 500},
				{ContainerName: "app", CPUCoresAvg: 0.5, CPUCoresPeak: 0.6, MemoryBytesAvg: 200, MemoryBytesPeak: 400},
			},
			strategy: AggregateMax,
			check: func(t *testing.T, r ResourceUsage) {
				assertFloat(t, "CPUCoresAvg", r.CPUCoresAvg, 0.8)
				assertFloat(t, "CPUCoresPeak", r.CPUCoresPeak, 1.0)
				assertFloat(t, "MemoryBytesAvg", r.MemoryBytesAvg, 300)
				assertFloat(t, "MemoryBytesPeak", r.MemoryBytesPeak, 500)
			},
		},
		{
			name: "MinPod picks lowest value for each metric",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.2, CPUCoresPeak: 0.4, MemoryBytesAvg: 100, MemoryBytesPeak: 200},
				{ContainerName: "app", CPUCoresAvg: 0.8, CPUCoresPeak: 1.0, MemoryBytesAvg: 300, MemoryBytesPeak: 500},
				{ContainerName: "app", CPUCoresAvg: 0.5, CPUCoresPeak: 0.6, MemoryBytesAvg: 200, MemoryBytesPeak: 400},
			},
			strategy: AggregateMin,
			check: func(t *testing.T, r ResourceUsage) {
				assertFloat(t, "CPUCoresAvg", r.CPUCoresAvg, 0.2)
				assertFloat(t, "CPUCoresPeak", r.CPUCoresPeak, 0.4)
				assertFloat(t, "MemoryBytesAvg", r.MemoryBytesAvg, 100)
				assertFloat(t, "MemoryBytesPeak", r.MemoryBytesPeak, 200)
			},
		},
		{
			name: "Auto (P90) with 5 pods",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.1, CPUCoresPeak: 0.2, MemoryBytesAvg: 100, MemoryBytesPeak: 200},
				{ContainerName: "app", CPUCoresAvg: 0.2, CPUCoresPeak: 0.3, MemoryBytesAvg: 200, MemoryBytesPeak: 300},
				{ContainerName: "app", CPUCoresAvg: 0.3, CPUCoresPeak: 0.4, MemoryBytesAvg: 300, MemoryBytesPeak: 400},
				{ContainerName: "app", CPUCoresAvg: 0.4, CPUCoresPeak: 0.5, MemoryBytesAvg: 400, MemoryBytesPeak: 500},
				{ContainerName: "app", CPUCoresAvg: 0.5, CPUCoresPeak: 0.6, MemoryBytesAvg: 500, MemoryBytesPeak: 600},
			},
			strategy: AggregateAuto,
			check: func(t *testing.T, r ResourceUsage) {
				// P90 of 5 values: ceil(0.9 * 5) - 1 = 4 → index 4 → max value
				assertFloat(t, "CPUCoresAvg", r.CPUCoresAvg, 0.5)
				assertFloat(t, "CPUCoresPeak", r.CPUCoresPeak, 0.6)
				assertFloat(t, "MemoryBytesAvg", r.MemoryBytesAvg, 500)
				assertFloat(t, "MemoryBytesPeak", r.MemoryBytesPeak, 600)
			},
		},
		{
			name: "Auto (P90) with 10 pods picks 9th value",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.1},
				{ContainerName: "app", CPUCoresAvg: 0.2},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.4},
				{ContainerName: "app", CPUCoresAvg: 0.5},
				{ContainerName: "app", CPUCoresAvg: 0.6},
				{ContainerName: "app", CPUCoresAvg: 0.7},
				{ContainerName: "app", CPUCoresAvg: 0.8},
				{ContainerName: "app", CPUCoresAvg: 0.9},
				{ContainerName: "app", CPUCoresAvg: 1.0},
			},
			strategy: AggregateAuto,
			check: func(t *testing.T, r ResourceUsage) {
				// P90 of 10: ceil(0.9 * 10) - 1 = 8 → sorted[8] = 0.9
				assertFloat(t, "CPUCoresAvg", r.CPUCoresAvg, 0.9)
			},
		},
		{
			name: "Auto (P90) with 2 pods degrades to max",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.2, CPUCoresPeak: 0.3, MemoryBytesAvg: 100, MemoryBytesPeak: 200},
				{ContainerName: "app", CPUCoresAvg: 0.8, CPUCoresPeak: 1.0, MemoryBytesAvg: 300, MemoryBytesPeak: 500},
			},
			strategy: AggregateAuto,
			check: func(t *testing.T, r ResourceUsage) {
				// P90 of 2: ceil(0.9 * 2) - 1 = 1 → sorted[1] = max
				assertFloat(t, "CPUCoresAvg", r.CPUCoresAvg, 0.8)
				assertFloat(t, "CPUCoresPeak", r.CPUCoresPeak, 1.0)
				assertFloat(t, "MemoryBytesAvg", r.MemoryBytesAvg, 300)
				assertFloat(t, "MemoryBytesPeak", r.MemoryBytesPeak, 500)
			},
		},
		{
			name: "extreme outlier: P90 ignores only the top outlier",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 0.3},
				{ContainerName: "app", CPUCoresAvg: 10.0}, // outlier
			},
			strategy: AggregateAuto,
			check: func(t *testing.T, r ResourceUsage) {
				// P90 of 10: index 8 → sorted[8] = 0.3 (the outlier at 10.0 is index 9)
				assertFloat(t, "CPUCoresAvg", r.CPUCoresAvg, 0.3)
			},
		},
		{
			name: "identical pods return same value for all strategies",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.5, CPUCoresPeak: 0.5, MemoryBytesAvg: 256, MemoryBytesPeak: 256},
				{ContainerName: "app", CPUCoresAvg: 0.5, CPUCoresPeak: 0.5, MemoryBytesAvg: 256, MemoryBytesPeak: 256},
				{ContainerName: "app", CPUCoresAvg: 0.5, CPUCoresPeak: 0.5, MemoryBytesAvg: 256, MemoryBytesPeak: 256},
			},
			strategy: AggregateMax,
			check: func(t *testing.T, r ResourceUsage) {
				assertFloat(t, "CPUCoresAvg", r.CPUCoresAvg, 0.5)
				assertFloat(t, "MemoryBytesAvg", r.MemoryBytesAvg, 256)
			},
		},
		{
			name: "unknown strategy returns error",
			samples: []ResourceUsage{
				{ContainerName: "app", CPUCoresAvg: 0.5},
				{ContainerName: "app", CPUCoresAvg: 0.6},
			},
			strategy: AggregationStrategy("WrongMode"),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := AggregateUsage(tt.samples, tt.strategy)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AggregateUsage() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.check != nil {
				tt.check(t, result)
			}
		})
	}
}

func TestAggregateAllocations(t *testing.T) {
	tests := []struct {
		name    string
		samples []ResourceAllocation
		wantErr bool
		check   func(t *testing.T, result ResourceAllocation)
	}{
		{
			name:    "empty returns error",
			samples: nil,
			wantErr: true,
		},
		{
			name: "single sample passthrough",
			samples: []ResourceAllocation{
				{ContainerName: "app", CPUCoresRequest: 0.5, MemoryBytesRequest: 256},
			},
			check: func(t *testing.T, r ResourceAllocation) {
				assertFloat(t, "CPUCoresRequest", r.CPUCoresRequest, 0.5)
				assertFloat(t, "MemoryBytesRequest", r.MemoryBytesRequest, 256)
			},
		},
		{
			name: "takes max of each field",
			samples: []ResourceAllocation{
				{ContainerName: "app", CPUCoresRequest: 0.2, CPUCoresLimit: 0.4, MemoryBytesRequest: 100, MemoryBytesLimit: 200},
				{ContainerName: "app", CPUCoresRequest: 0.8, CPUCoresLimit: 1.6, MemoryBytesRequest: 500, MemoryBytesLimit: 1000},
				{ContainerName: "app", CPUCoresRequest: 0.5, CPUCoresLimit: 1.0, MemoryBytesRequest: 300, MemoryBytesLimit: 600},
			},
			check: func(t *testing.T, r ResourceAllocation) {
				assertFloat(t, "CPUCoresRequest", r.CPUCoresRequest, 0.8)
				assertFloat(t, "CPUCoresLimit", r.CPUCoresLimit, 1.6)
				assertFloat(t, "MemoryBytesRequest", r.MemoryBytesRequest, 500)
				assertFloat(t, "MemoryBytesLimit", r.MemoryBytesLimit, 1000)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := AggregateAllocations(tt.samples)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AggregateAllocations() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.check != nil {
				tt.check(t, result)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name string
		vals []float64
		p    float64
		want float64
	}{
		{"empty", nil, 0.9, 0},
		{"single", []float64{42}, 0.9, 42},
		{"P50 of 4", []float64{1, 2, 3, 4}, 0.50, 2},
		{"P90 of 10", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.90, 9},
		{"P99 of 3", []float64{1, 2, 3}, 0.99, 3},
		{"P0 of 3", []float64{5, 10, 15}, 0.0, 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentile(tt.vals, tt.p)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("percentile() = %v, want %v", got, tt.want)
			}
		})
	}
}

// assertFloat is a helper for comparing float64 values with a small tolerance.
func assertFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.001 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
