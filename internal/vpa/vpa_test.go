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

package vpa

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestParseVPA(t *testing.T) {
	tests := []struct {
		name              string
		obj               map[string]interface{}
		wantErr           bool
		wantUpdateMode    string
		wantTargetKind    string
		wantTargetName    string
		wantHasRec        bool
		wantContainerRecs int
	}{
		{
			name: "valid VPA with recommendations and Off mode",
			obj: map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "my-vpa",
					"namespace": "default",
				},
				"spec": map[string]interface{}{
					"targetRef": map[string]interface{}{
						"kind":       "Deployment",
						"name":       "my-app",
						"apiVersion": "apps/v1",
					},
					"updatePolicy": map[string]interface{}{
						"updateMode": "Off",
					},
				},
				"status": map[string]interface{}{
					"recommendation": map[string]interface{}{
						"containerRecommendations": []interface{}{
							map[string]interface{}{
								"containerName": "app",
								"target": map[string]interface{}{
									"cpu":    "500m",
									"memory": "256Mi",
								},
								"lowerBound": map[string]interface{}{
									"cpu":    "250m",
									"memory": "128Mi",
								},
								"upperBound": map[string]interface{}{
									"cpu":    "1",
									"memory": "512Mi",
								},
							},
						},
					},
				},
			},
			wantUpdateMode:    "Off",
			wantTargetKind:    "Deployment",
			wantTargetName:    "my-app",
			wantHasRec:        true,
			wantContainerRecs: 1,
		},
		{
			name: "VPA without updatePolicy defaults to Auto",
			obj: map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "auto-vpa",
					"namespace": "default",
				},
				"spec": map[string]interface{}{
					"targetRef": map[string]interface{}{
						"kind": "Deployment",
						"name": "my-app",
					},
				},
			},
			wantUpdateMode:    "Auto",
			wantTargetKind:    "Deployment",
			wantTargetName:    "my-app",
			wantHasRec:        false,
			wantContainerRecs: 0,
		},
		{
			name: "VPA missing targetRef returns error",
			obj: map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "bad-vpa",
					"namespace": "default",
				},
				"spec": map[string]interface{}{},
			},
			wantErr: true,
		},
		{
			name: "VPA with multiple container recommendations",
			obj: map[string]interface{}{
				"metadata": map[string]interface{}{
					"name":      "multi-vpa",
					"namespace": "prod",
				},
				"spec": map[string]interface{}{
					"targetRef": map[string]interface{}{
						"kind": "Deployment",
						"name": "multi-app",
					},
					"updatePolicy": map[string]interface{}{
						"updateMode": "Off",
					},
				},
				"status": map[string]interface{}{
					"recommendation": map[string]interface{}{
						"containerRecommendations": []interface{}{
							map[string]interface{}{
								"containerName": "app",
								"target": map[string]interface{}{
									"cpu":    "200m",
									"memory": "128Mi",
								},
							},
							map[string]interface{}{
								"containerName": "sidecar",
								"target": map[string]interface{}{
									"cpu":    "50m",
									"memory": "64Mi",
								},
							},
						},
					},
				},
			},
			wantUpdateMode:    "Off",
			wantTargetKind:    "Deployment",
			wantTargetName:    "multi-app",
			wantHasRec:        true,
			wantContainerRecs: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: tt.obj}
			info, err := parseVPA(obj)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseVPA() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			if info.UpdateMode != tt.wantUpdateMode {
				t.Errorf("UpdateMode = %q, want %q", info.UpdateMode, tt.wantUpdateMode)
			}
			if info.TargetRefKind != tt.wantTargetKind {
				t.Errorf("TargetRefKind = %q, want %q", info.TargetRefKind, tt.wantTargetKind)
			}
			if info.TargetRefName != tt.wantTargetName {
				t.Errorf("TargetRefName = %q, want %q", info.TargetRefName, tt.wantTargetName)
			}
			if info.HasRecommendation != tt.wantHasRec {
				t.Errorf("HasRecommendation = %v, want %v", info.HasRecommendation, tt.wantHasRec)
			}
			if len(info.ContainerRecommendations) != tt.wantContainerRecs {
				t.Errorf("ContainerRecommendations count = %d, want %d",
					len(info.ContainerRecommendations), tt.wantContainerRecs)
			}
		})
	}
}

func TestValidateVPAMode(t *testing.T) {
	tests := []struct {
		name    string
		vpa     *VPAInfo
		wantErr bool
	}{
		{
			name: "Off mode is valid",
			vpa: &VPAInfo{
				Name:       "test-vpa",
				Namespace:  "default",
				UpdateMode: UpdateModeOff,
			},
			wantErr: false,
		},
		{
			name: "Auto mode is invalid",
			vpa: &VPAInfo{
				Name:       "test-vpa",
				Namespace:  "default",
				UpdateMode: UpdateModeAuto,
			},
			wantErr: true,
		},
		{
			name: "Recreate mode is invalid",
			vpa: &VPAInfo{
				Name:       "test-vpa",
				Namespace:  "default",
				UpdateMode: UpdateModeRecreate,
			},
			wantErr: true,
		},
		{
			name: "Initial mode is invalid",
			vpa: &VPAInfo{
				Name:       "test-vpa",
				Namespace:  "default",
				UpdateMode: UpdateModeInitial,
			},
			wantErr: true,
		},
		{
			name:    "nil VPA is valid (no VPA present)",
			vpa:     nil,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVPAMode(tt.vpa)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateVPAMode() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseResourceList(t *testing.T) {
	crMap := map[string]interface{}{
		"target": map[string]interface{}{
			"cpu":    "500m",
			"memory": "256Mi",
		},
	}

	result := parseResourceList(crMap, "target")

	cpuQ := result[corev1.ResourceCPU]
	expectedCPU := resource.MustParse("500m")
	if cpuQ.Cmp(expectedCPU) != 0 {
		t.Errorf("CPU = %s, want %s", cpuQ.String(), expectedCPU.String())
	}

	memQ := result[corev1.ResourceMemory]
	expectedMem := resource.MustParse("256Mi")
	if memQ.Cmp(expectedMem) != 0 {
		t.Errorf("Memory = %s, want %s", memQ.String(), expectedMem.String())
	}
}

func TestParseResourceListMissing(t *testing.T) {
	crMap := map[string]interface{}{}
	result := parseResourceList(crMap, "target")

	if len(result) != 0 {
		t.Errorf("Expected empty resource list, got %v", result)
	}
}

func TestIsNoMatchError(t *testing.T) {
	tests := []struct {
		name    string
		errStr  string
		wantRes bool
	}{
		{"no matches for kind", "no matches for kind \"VerticalPodAutoscaler\"", true},
		{"server could not find", "the server could not find the requested resource", true},
		{"no match", "no match for GroupVersionResource", true},
		{"random error", "connection refused", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.errStr != "" {
				err = fmt.Errorf("%s", tt.errStr)
			}
			got := isNoMatchError(err)
			if got != tt.wantRes {
				t.Errorf("isNoMatchError(%q) = %v, want %v", tt.errStr, got, tt.wantRes)
			}
		})
	}
}
