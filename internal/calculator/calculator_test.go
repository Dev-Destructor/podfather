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
	"testing"
)

func TestCalculate(t *testing.T) {
	cfg := DefaultConfig()
	constr := DefaultConstraints()

	tests := []struct {
		name            string
		usage           ResourceUsage
		alloc           ResourceAllocation
		cfg             Config
		constraints     Constraints
		wantSignificant bool
		wantErr         bool
	}{
		{
			name: "significant over-allocation triggers update",
			usage: ResourceUsage{
				ContainerName:   "app",
				CPUCoresAvg:     0.1,
				CPUCoresPeak:    0.15,
				MemoryBytesAvg:  50 * 1024 * 1024,
				MemoryBytesPeak: 60 * 1024 * 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "app",
				CPUCoresRequest:    1.0,
				CPUCoresLimit:      2.0,
				MemoryBytesRequest: 512 * 1024 * 1024,
				MemoryBytesLimit:   1024 * 1024 * 1024,
			},
			cfg:             cfg,
			constraints:     constr,
			wantSignificant: true,
		},
		{
			name: "significant under-allocation triggers update",
			usage: ResourceUsage{
				ContainerName:   "app",
				CPUCoresAvg:     3.5,
				CPUCoresPeak:    4.0,
				MemoryBytesAvg:  900 * 1024 * 1024,
				MemoryBytesPeak: 1000 * 1024 * 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "app",
				CPUCoresRequest:    0.5,
				CPUCoresLimit:      1.0,
				MemoryBytesRequest: 256 * 1024 * 1024,
				MemoryBytesLimit:   512 * 1024 * 1024,
			},
			cfg:             cfg,
			constraints:     constr,
			wantSignificant: true,
		},
		{
			name: "optimal allocation does not trigger update",
			usage: ResourceUsage{
				ContainerName:   "app",
				CPUCoresAvg:     0.8,
				CPUCoresPeak:    0.9,
				MemoryBytesAvg:  400 * 1024 * 1024,
				MemoryBytesPeak: 420 * 1024 * 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "app",
				CPUCoresRequest:    0.96,
				CPUCoresLimit:      1.44,
				MemoryBytesRequest: 500 * 1024 * 1024,
				MemoryBytesLimit:   750 * 1024 * 1024,
			},
			cfg:             cfg,
			constraints:     constr,
			wantSignificant: false,
		},
		{
			name: "empty container name returns error",
			usage: ResourceUsage{
				ContainerName: "",
				CPUCoresAvg:   0.1,
			},
			alloc:       ResourceAllocation{},
			cfg:         cfg,
			constraints: constr,
			wantErr:     true,
		},
		{
			name: "zero current allocation gives 100% variance",
			usage: ResourceUsage{
				ContainerName:   "new",
				CPUCoresAvg:     0.1,
				CPUCoresPeak:    0.1,
				MemoryBytesAvg:  50 * 1024 * 1024,
				MemoryBytesPeak: 50 * 1024 * 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "new",
				CPUCoresRequest:    0,
				MemoryBytesRequest: 0,
			},
			cfg:             cfg,
			constraints:     constr,
			wantSignificant: true,
		},
		{
			name: "respects min constraints",
			usage: ResourceUsage{
				ContainerName:   "tiny",
				CPUCoresAvg:     0.001,
				CPUCoresPeak:    0.001,
				MemoryBytesAvg:  1024,
				MemoryBytesPeak: 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "tiny",
				CPUCoresRequest:    0.001,
				MemoryBytesRequest: 1024,
			},
			cfg:             cfg,
			constraints:     constr,
			wantSignificant: true,
		},
		{
			name: "respects max constraints",
			usage: ResourceUsage{
				ContainerName:   "huge",
				CPUCoresAvg:     100,
				CPUCoresPeak:    200,
				MemoryBytesAvg:  200 * 1024 * 1024 * 1024,
				MemoryBytesPeak: 300 * 1024 * 1024 * 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "huge",
				CPUCoresRequest:    100,
				MemoryBytesRequest: 200 * 1024 * 1024 * 1024,
			},
			cfg:             cfg,
			constraints:     constr,
			wantSignificant: true,
		},
		{
			name: "peak usage drives recommendation when higher",
			usage: ResourceUsage{
				ContainerName:   "bursty",
				CPUCoresAvg:     0.5,
				CPUCoresPeak:    2.0,
				MemoryBytesAvg:  100 * 1024 * 1024,
				MemoryBytesPeak: 500 * 1024 * 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "bursty",
				CPUCoresRequest:    0.5,
				MemoryBytesRequest: 100 * 1024 * 1024,
			},
			cfg:             cfg,
			constraints:     constr,
			wantSignificant: true,
		},
		{
			name: "custom variance threshold triggers on small deviation",
			usage: ResourceUsage{
				ContainerName:   "app",
				CPUCoresAvg:     0.5,
				CPUCoresPeak:    0.6,
				MemoryBytesAvg:  200 * 1024 * 1024,
				MemoryBytesPeak: 250 * 1024 * 1024,
			},
			alloc: ResourceAllocation{
				ContainerName:      "app",
				CPUCoresRequest:    0.55,
				CPUCoresLimit:      0.83,
				MemoryBytesRequest: 250 * 1024 * 1024,
				MemoryBytesLimit:   375 * 1024 * 1024,
			},
			cfg: Config{
				CPURequestMarginPercent:    20,
				MemoryRequestMarginPercent: 25,
				LimitToRequestRatio:        1.5,
				PeakDampeningCPU:           0.8,
				PeakDampeningMemory:        0.9,
				VarianceThresholdPercent:   1, // very tight threshold
			},
			constraints:     constr,
			wantSignificant: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := Calculate(tt.usage, tt.alloc, tt.cfg, tt.constraints)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Calculate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if rec.SignificantVariance != tt.wantSignificant {
				t.Errorf("SignificantVariance = %v, want %v (cpuVar=%.2f%%, memVar=%.2f%%)",
					rec.SignificantVariance, tt.wantSignificant,
					rec.CPUVariancePercent, rec.MemoryVariancePercent)
			}
			// Verify requests are within constraints
			if rec.CPUCoresRequest < tt.constraints.MinCPUCores || rec.CPUCoresRequest > tt.constraints.MaxCPUCores {
				t.Errorf("CPUCoresRequest %.4f outside constraints [%.4f, %.4f]",
					rec.CPUCoresRequest, tt.constraints.MinCPUCores, tt.constraints.MaxCPUCores)
			}
			if rec.MemoryBytesRequest < tt.constraints.MinMemoryBytes || rec.MemoryBytesRequest > tt.constraints.MaxMemoryBytes {
				t.Errorf("MemoryBytesRequest %.0f outside constraints [%.0f, %.0f]",
					rec.MemoryBytesRequest, tt.constraints.MinMemoryBytes, tt.constraints.MaxMemoryBytes)
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CPURequestMarginPercent != 20 {
		t.Errorf("CPURequestMarginPercent = %v, want 20", cfg.CPURequestMarginPercent)
	}
	if cfg.LimitToRequestRatio != 1.5 {
		t.Errorf("LimitToRequestRatio = %v, want 1.5", cfg.LimitToRequestRatio)
	}
}
