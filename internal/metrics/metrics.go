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

// Package metrics provides custom Prometheus metrics for Podfather and a
// helper to collect pod resource usage from the Kubernetes Metrics API.
//
// All Prometheus metric names are prefixed with "podfather_".
package metrics

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ---- Prometheus Metrics ----

var (
	// PodsMonitored tracks the number of pods currently being monitored.
	PodsMonitored = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "podfather_pods_monitored",
		Help: "Number of pods currently monitored by Podfather.",
	})

	// ResourceAdjustments counts resource adjustments applied per container.
	ResourceAdjustments = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "podfather_resource_adjustments_total",
		Help: "Total number of resource adjustments applied.",
	}, []string{"namespace", "pod", "container", "resource"})

	// CollectionErrors counts metrics collection failures.
	CollectionErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "podfather_metrics_collection_errors_total",
		Help: "Total number of metrics collection errors.",
	})

	// EvaluationDuration observes the time taken for an evaluation cycle.
	EvaluationDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "podfather_evaluation_duration_seconds",
		Help:    "Duration of a single evaluation cycle in seconds.",
		Buckets: prometheus.DefBuckets,
	})

	// StarvationEvents counts starvation events detected per pod.
	StarvationEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "podfather_pod_starvation_events_total",
		Help: "Total number of pod starvation events detected.",
	}, []string{"namespace", "pod"})

	// RecommendationVariance observes the variance distribution of recommendations.
	RecommendationVariance = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "podfather_recommendation_variance_percent",
		Help:    "Distribution of recommendation variance as a percentage.",
		Buckets: []float64{5, 10, 15, 25, 50, 75, 100, 200},
	}, []string{"resource"})
)

// RegisterMetrics registers all custom Podfather metrics with the
// controller-runtime metrics registry. Call this once during startup.
func RegisterMetrics() {
	ctrlmetrics.Registry.MustRegister(
		PodsMonitored,
		ResourceAdjustments,
		CollectionErrors,
		EvaluationDuration,
		StarvationEvents,
		RecommendationVariance,
	)
}

// ---- Metrics API Collector ----

// Collector fetches pod resource metrics from the Kubernetes Metrics API.
type Collector struct {
	client client.Client
}

// NewCollector creates a new Collector using the provided controller-runtime client.
func NewCollector(c client.Client) *Collector {
	return &Collector{client: c}
}

// GetPodMetrics fetches the PodMetrics for a specific pod from the Metrics API.
// It returns an error if the Metrics API is unavailable or the pod has no metrics yet.
func (c *Collector) GetPodMetrics(ctx context.Context, namespace, name string) (*metricsv1beta1.PodMetrics, error) {
	pm := &metricsv1beta1.PodMetrics{}
	err := c.client.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, pm)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod metrics for %s/%s: %w", namespace, name, err)
	}
	return pm, nil
}

// GetCurrentAllocations reads the current resource requests and limits directly
// from a Pod's container specs.
func GetCurrentAllocations(pod *corev1.Pod) map[string]ContainerResources {
	result := make(map[string]ContainerResources)
	for _, c := range pod.Spec.Containers {
		cr := ContainerResources{}
		if req, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cr.CPURequest = req.AsApproximateFloat64()
		}
		if lim, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			cr.CPULimit = lim.AsApproximateFloat64()
		}
		if req, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			cr.MemoryRequest = req.AsApproximateFloat64()
		}
		if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			cr.MemoryLimit = lim.AsApproximateFloat64()
		}
		result[c.Name] = cr
	}
	return result
}

// ContainerResources holds the current allocations for a single container.
type ContainerResources struct {
	CPURequest    float64
	CPULimit      float64
	MemoryRequest float64
	MemoryLimit   float64
}
