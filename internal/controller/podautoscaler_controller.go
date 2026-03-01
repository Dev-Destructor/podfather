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

// Package controller implements the reconciliation loop for PodAutoscaler resources.
//
// The reconciler follows this flow on every cycle:
//
//  1. Fetch the PodAutoscaler CR.
//  2. Handle finalizer lifecycle (add on create, cleanup on delete).
//  3. Discover pods matching the label selector.
//  4. For each pod: collect metrics → get current allocation → calculate ideal → check variance.
//  5. If variance is significant and not dry-run: apply the update (in-place or eviction).
//  6. Update the CR status (phase, conditions, recommendations, counters).
//  7. Requeue after the configured interval.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/des57/podfather/api/v1alpha1"
	"github.com/des57/podfather/internal/calculator"
	"github.com/des57/podfather/internal/metrics"
	"github.com/des57/podfather/internal/namespacelimits"
	"github.com/des57/podfather/internal/updater"
	"github.com/des57/podfather/internal/vpa"
)

const (
	finalizerName       = "podfather.io/finalizer"
	defaultRequeueAfter = 30 * time.Second
)

// PodAutoscalerReconciler reconciles a PodAutoscaler object.
type PodAutoscalerReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	Recorder               record.EventRecorder
	VPAClient              *vpa.Client
	NamespaceLimitsFetcher *namespacelimits.Fetcher
}

// +kubebuilder:rbac:groups=autoscaling.podfather.io,resources=podautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.podfather.io,resources=podautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.podfather.io,resources=podautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch

// Reconcile implements the core reconciliation loop for PodAutoscaler.
func (r *PodAutoscalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// ---- Step 1: Fetch the PodAutoscaler CR ----
	pa := &autoscalingv1alpha1.PodAutoscaler{}
	if err := r.Get(ctx, req.NamespacedName, pa); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PodAutoscaler not found, likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PodAutoscaler: %w", err)
	}

	// ---- Step 2: Finalizer handling ----
	if done, fErr := r.handleFinalizer(ctx, pa); done {
		return ctrl.Result{}, fErr
	}

	// ---- Step 3: Discover pods ----
	runningPods, err := r.discoverRunningPods(ctx, pa)
	if err != nil {
		return ctrl.Result{}, err
	}

	pa.Status.MonitoredPods = int32(len(runningPods))
	metrics.PodsMonitored.Set(float64(len(runningPods)))

	if len(runningPods) == 0 {
		logger.Info("No running pods matching selector")
		pa.Status.Phase = autoscalingv1alpha1.PhasePending
		r.setCondition(pa, "Ready", metav1.ConditionFalse, "NoPodsFound", "No running pods match the selector")
		_ = r.Status().Update(ctx, pa)
		return ctrl.Result{RequeueAfter: r.requeueInterval(pa)}, nil
	}

	// ---- Step 4: VPA Integration Check ----
	vpaResult := r.checkVPAIntegration(ctx, pa)
	pa.Status.VPAStatus = vpaResult.status
	usingVPA := vpaResult.status != nil && vpaResult.status.UsingVPARecommendations

	// If strict mode and VPA conflict detected, stop reconciliation
	if r.shouldHaltForVPAConflict(pa, vpaResult) {
		logger.Info("VPA conflict in strict mode, halting reconciliation",
			"conflict", vpaResult.status.VPAConflict)
		pa.Status.Phase = autoscalingv1alpha1.PhaseDegraded
		r.setCondition(pa, "VPAConflict", metav1.ConditionTrue, "VPAConflict", vpaResult.status.VPAConflict)
		_ = r.Status().Update(ctx, pa)
		r.Recorder.Eventf(pa, corev1.EventTypeWarning, "VPAConflict", "%s", vpaResult.status.VPAConflict)
		return ctrl.Result{RequeueAfter: r.requeueInterval(pa)}, nil
	}

	// ---- Step 5: For each pod: collect, calculate, apply ----
	collector := metrics.NewCollector(r.Client)
	podUpdater := updater.NewPodUpdater(r.Client)
	cfg := buildCalculatorConfig(pa)
	defaultConstraints := calculator.DefaultConstraints()

	// Fetch namespace LimitRange and ResourceQuota constraints
	nsConstraints := namespacelimits.EmptyConstraints()
	if r.NamespaceLimitsFetcher != nil {
		var ncErr error
		nsConstraints, ncErr = r.NamespaceLimitsFetcher.GetNamespaceConstraints(ctx, pa.Namespace)
		if ncErr != nil {
			logger.Error(ncErr, "Failed to fetch namespace constraints, using defaults")
			nsConstraints = namespacelimits.EmptyConstraints()
		}
	}

	// Report namespace constraints in status
	pa.Status.NamespaceConstraints = buildNamespaceConstraintsStatus(nsConstraints)

	// Merge namespace constraints with calculator defaults (tightest bounds win)
	minCPU, maxCPU, minMem, maxMem := nsConstraints.ToCalculatorConstraints(
		defaultConstraints.MinCPUCores, defaultConstraints.MaxCPUCores,
		defaultConstraints.MinMemoryBytes, defaultConstraints.MaxMemoryBytes)
	constraints := calculator.Constraints{
		MinCPUCores:    minCPU,
		MaxCPUCores:    maxCPU,
		MinMemoryBytes: minMem,
		MaxMemoryBytes: maxMem,
	}

	if nsConstraints.LimitRangeFound || nsConstraints.ResourceQuotaFound {
		logger.Info("Namespace constraints applied",
			"limitRange", nsConstraints.LimitRangeFound,
			"resourceQuota", nsConstraints.ResourceQuotaFound,
			"minCPU", fmt.Sprintf("%.3f", minCPU),
			"maxCPU", fmt.Sprintf("%.3f", maxCPU),
			"minMem", fmt.Sprintf("%.0f", minMem),
			"maxMem", fmt.Sprintf("%.0f", maxMem))
	}

	evalStart := time.Now()

	var containerRecs []autoscalingv1alpha1.ContainerRecommendation
	adjustmentsMade := int64(0)
	metricsAvailable := true
	var allClampReasons []string

	for i := range runningPods {
		pod := &runningPods[i]

		// 5a. If using VPA recommendations, convert them directly
		if usingVPA {
			recs, adj, reasons := r.evaluateVPAPod(ctx, pa, pod, vpaResult.recommendations,
				nsConstraints, constraints, cfg, podUpdater)
			containerRecs = append(containerRecs, recs...)
			adjustmentsMade += adj
			allClampReasons = append(allClampReasons, reasons...)
			continue
		}

		// 5b. Collect metrics (Podfather's own algorithm)
		recs, adj, ok, reasons := r.evaluatePodMetrics(ctx, pa, pod, collector,
			podUpdater, cfg, constraints, nsConstraints)
		containerRecs = append(containerRecs, recs...)
		adjustmentsMade += adj
		if !ok {
			metricsAvailable = false
		}
		allClampReasons = append(allClampReasons, reasons...)
	}

	evalDuration := time.Since(evalStart).Seconds()
	metrics.EvaluationDuration.Observe(evalDuration)

	// ---- Step 7: Update CR status ----
	// Persist clamp reasons if namespace constraints were applied
	if pa.Status.NamespaceConstraints != nil && len(allClampReasons) > 0 {
		pa.Status.NamespaceConstraints.ClampReasons = allClampReasons
		for _, reason := range allClampReasons {
			logger.Info("Recommendation clamped by namespace constraint", "reason", reason)
		}
		r.Recorder.Eventf(pa, corev1.EventTypeNormal, "RecommendationClamped",
			"Recommendations adjusted due to namespace constraints: %d adjustments", len(allClampReasons))
	}

	now := metav1.Now()
	pa.Status.LastEvaluationTime = &now
	pa.Status.ObservedGeneration = pa.Generation
	pa.Status.TotalAdjustments += adjustmentsMade

	if adjustmentsMade > 0 {
		pa.Status.LastUpdateTime = &now
	}

	if len(containerRecs) > 0 {
		pa.Status.Recommendation = &autoscalingv1alpha1.Recommendation{
			ContainerRecommendations: containerRecs,
		}
	}

	if metricsAvailable {
		pa.Status.Phase = autoscalingv1alpha1.PhaseActive
		recSource := "podfather"
		if usingVPA {
			recSource = "vpa"
		}
		r.setCondition(pa, "Ready", metav1.ConditionTrue, "Reconciled",
			fmt.Sprintf("Successfully evaluated all pods (source=%s)", recSource))
		r.setCondition(pa, "MetricsAvailable", metav1.ConditionTrue, "MetricsCollected", "Metrics API responsive")
	} else {
		pa.Status.Phase = autoscalingv1alpha1.PhaseDegraded
		r.setCondition(pa, "MetricsAvailable", metav1.ConditionFalse, "MetricsUnavailable",
			"Failed to collect metrics for some pods")
	}

	if err := r.Status().Update(ctx, pa); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	// ---- Step 8: Requeue ----
	return ctrl.Result{RequeueAfter: r.requeueInterval(pa)}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodAutoscalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.PodAutoscaler{}).
		Named("podautoscaler").
		Complete(r)
}

// ---- Reconciliation Sub-Steps ----

// handleFinalizer manages the lifecycle of the PodAutoscaler finalizer.
// It returns (true, err) if the reconciliation should stop after this step.
func (r *PodAutoscalerReconciler) handleFinalizer(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) (bool, error) {
	if pa.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(pa, finalizerName) {
			logf.FromContext(ctx).Info("Running cleanup for PodAutoscaler deletion")
			r.cleanupManagedPods(ctx, pa)
			controllerutil.RemoveFinalizer(pa, finalizerName)
			if err := r.Update(ctx, pa); err != nil {
				return true, fmt.Errorf("failed to remove finalizer: %w", err)
			}
		}
		return true, nil
	}

	if !controllerutil.ContainsFinalizer(pa, finalizerName) {
		controllerutil.AddFinalizer(pa, finalizerName)
		if err := r.Update(ctx, pa); err != nil {
			return true, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}
	return false, nil
}

// discoverRunningPods parses the label selector, lists matching pods, and filters to running only.
func (r *PodAutoscalerReconciler) discoverRunningPods(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) ([]corev1.Pod, error) {
	selector, err := metav1.LabelSelectorAsSelector(pa.Spec.Selector)
	if err != nil {
		r.setCondition(pa, "Ready", metav1.ConditionFalse, "InvalidSelector", err.Error())
		pa.Status.Phase = autoscalingv1alpha1.PhaseError
		_ = r.Status().Update(ctx, pa)
		return nil, fmt.Errorf("invalid label selector: %w", err)
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(pa.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	return filterRunningPods(podList.Items), nil
}

// shouldHaltForVPAConflict reports whether reconciliation should stop due to a
// VPA conflict when strict mode is enabled.
func (r *PodAutoscalerReconciler) shouldHaltForVPAConflict(pa *autoscalingv1alpha1.PodAutoscaler, result vpaCheckResult) bool {
	return result.status != nil &&
		result.status.VPAConflict != "" &&
		pa.Spec.VPAPolicy != nil &&
		pa.Spec.VPAPolicy.StrictMode
}

// evaluateVPAPod applies VPA recommendations for a single pod, clamping to
// namespace constraints and recording metrics. It returns the container
// recommendations, adjustment count, and any clamp reasons.
func (r *PodAutoscalerReconciler) evaluateVPAPod(
	ctx context.Context,
	pa *autoscalingv1alpha1.PodAutoscaler,
	pod *corev1.Pod,
	vpaRecs []vpa.ContainerRecommendation,
	nsConstraints namespacelimits.NamespaceConstraints,
	constraints calculator.Constraints,
	cfg calculator.Config,
	podUpdater *updater.PodUpdater,
) ([]autoscalingv1alpha1.ContainerRecommendation, int64, []string) {
	logger := logf.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)
	containerRecs := make([]autoscalingv1alpha1.ContainerRecommendation, 0, len(vpaRecs))
	var adjustments int64
	var clampReasons []string

	for _, vpaRec := range vpaRecs {
		rec := vpaRecommendationToCalculatorRec(vpaRec, constraints)

		// Clamp VPA recommendation to namespace constraints
		if nsConstraints.LimitRangeFound || nsConstraints.ResourceQuotaFound {
			newCPUReq, newCPULim, newMemReq, newMemLim, reasons := namespacelimits.ClampRecommendation(
				rec.CPUCoresRequest, rec.CPUCoresLimit,
				rec.MemoryBytesRequest, rec.MemoryBytesLimit, nsConstraints)
			rec.CPUCoresRequest = newCPUReq
			rec.CPUCoresLimit = newCPULim
			rec.MemoryBytesRequest = newMemReq
			rec.MemoryBytesLimit = newMemLim
			clampReasons = append(clampReasons, reasons...)
		}

		// Get current allocation for variance calculation
		currentAllocs := metrics.GetCurrentAllocations(pod)
		if alloc, ok := currentAllocs[rec.ContainerName]; ok {
			rec.CPUVariancePercent = variancePercent(rec.CPUCoresRequest, alloc.CPURequest)
			rec.MemoryVariancePercent = variancePercent(rec.MemoryBytesRequest, alloc.MemoryRequest)
			rec.SignificantVariance = rec.CPUVariancePercent >= cfg.VarianceThresholdPercent ||
				rec.MemoryVariancePercent >= cfg.VarianceThresholdPercent
		}

		metrics.RecommendationVariance.WithLabelValues("cpu").Observe(rec.CPUVariancePercent)
		metrics.RecommendationVariance.WithLabelValues("memory").Observe(rec.MemoryVariancePercent)
		containerRecs = append(containerRecs, toStatusRecommendation(rec))

		if rec.SignificantVariance && !pa.Spec.DryRun {
			adj := r.applyRecommendation(ctx, pa, pod, rec, podUpdater, "vpa")
			adjustments += adj
		} else if rec.SignificantVariance && pa.Spec.DryRun {
			logger.Info("Dry-run: would apply VPA recommendation",
				"container", rec.ContainerName,
				"cpuVar", fmt.Sprintf("%.1f%%", rec.CPUVariancePercent),
				"memVar", fmt.Sprintf("%.1f%%", rec.MemoryVariancePercent))
		}
	}

	return containerRecs, adjustments, clampReasons
}

// evaluatePodMetrics runs Podfather's own algorithm for a single pod: collects
// metrics, calculates ideal resources, clamps, and applies updates. It returns
// container recommendations, adjustment count, whether metrics were available,
// and any clamp reasons.
func (r *PodAutoscalerReconciler) evaluatePodMetrics(
	ctx context.Context,
	pa *autoscalingv1alpha1.PodAutoscaler,
	pod *corev1.Pod,
	collector *metrics.Collector,
	podUpdater *updater.PodUpdater,
	cfg calculator.Config,
	constraints calculator.Constraints,
	nsConstraints namespacelimits.NamespaceConstraints,
) ([]autoscalingv1alpha1.ContainerRecommendation, int64, bool, []string) {
	logger := logf.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)
	var adjustments int64
	var clampReasons []string

	podMetrics, mErr := collector.GetPodMetrics(ctx, pod.Namespace, pod.Name)
	if mErr != nil {
		logger.Error(mErr, "Failed to get metrics for pod")
		metrics.CollectionErrors.Inc()
		return nil, 0, false, nil
	}

	currentAllocs := metrics.GetCurrentAllocations(pod)
	containerRecs := make([]autoscalingv1alpha1.ContainerRecommendation, 0, len(podMetrics.Containers))

	for _, containerMetrics := range podMetrics.Containers {
		alloc, ok := currentAllocs[containerMetrics.Name]
		if !ok {
			continue
		}

		usage := calculator.ResourceUsage{
			ContainerName:   containerMetrics.Name,
			CPUCoresAvg:     containerMetrics.Usage.Cpu().AsApproximateFloat64(),
			CPUCoresPeak:    containerMetrics.Usage.Cpu().AsApproximateFloat64(),
			MemoryBytesAvg:  containerMetrics.Usage.Memory().AsApproximateFloat64(),
			MemoryBytesPeak: containerMetrics.Usage.Memory().AsApproximateFloat64(),
		}

		calcAlloc := calculator.ResourceAllocation{
			ContainerName:      containerMetrics.Name,
			CPUCoresRequest:    alloc.CPURequest,
			CPUCoresLimit:      alloc.CPULimit,
			MemoryBytesRequest: alloc.MemoryRequest,
			MemoryBytesLimit:   alloc.MemoryLimit,
		}

		rec, calcErr := calculator.Calculate(usage, calcAlloc, cfg, constraints)
		if calcErr != nil {
			logger.Error(calcErr, "Calculation failed", "container", containerMetrics.Name)
			continue
		}

		// Clamp recommendation to namespace constraints (LimitRange + ResourceQuota)
		if nsConstraints.LimitRangeFound || nsConstraints.ResourceQuotaFound {
			newCPUReq, newCPULim, newMemReq, newMemLim, reasons := namespacelimits.ClampRecommendation(
				rec.CPUCoresRequest, rec.CPUCoresLimit,
				rec.MemoryBytesRequest, rec.MemoryBytesLimit, nsConstraints)
			rec.CPUCoresRequest = newCPUReq
			rec.CPUCoresLimit = newCPULim
			rec.MemoryBytesRequest = newMemReq
			rec.MemoryBytesLimit = newMemLim
			clampReasons = append(clampReasons, reasons...)
		}

		metrics.RecommendationVariance.WithLabelValues("cpu").Observe(rec.CPUVariancePercent)
		metrics.RecommendationVariance.WithLabelValues("memory").Observe(rec.MemoryVariancePercent)
		containerRecs = append(containerRecs, toStatusRecommendation(rec))

		if rec.SignificantVariance && !pa.Spec.DryRun {
			adj := r.applyRecommendation(ctx, pa, pod, rec, podUpdater, "podfather")
			adjustments += adj
		} else if rec.SignificantVariance && pa.Spec.DryRun {
			logger.Info("Dry-run: would adjust resources",
				"container", rec.ContainerName,
				"cpuVar", fmt.Sprintf("%.1f%%", rec.CPUVariancePercent),
				"memVar", fmt.Sprintf("%.1f%%", rec.MemoryVariancePercent))
			r.Recorder.Eventf(pa, corev1.EventTypeNormal, "DryRunRecommendation",
				"Would update pod %s container %s (cpuVar=%.1f%%, memVar=%.1f%%)",
				pod.Name, rec.ContainerName,
				rec.CPUVariancePercent, rec.MemoryVariancePercent)
		}
	}

	return containerRecs, adjustments, true, clampReasons
}

// applyRecommendation applies a single container resource recommendation to a pod.
// It returns 1 on success or 0 on failure.
func (r *PodAutoscalerReconciler) applyRecommendation(
	ctx context.Context,
	pa *autoscalingv1alpha1.PodAutoscaler,
	pod *corev1.Pod,
	rec calculator.ResourceRecommendation,
	podUpdater *updater.PodUpdater,
	source string,
) int64 {
	logger := logf.FromContext(ctx).WithValues("pod", pod.Name, "namespace", pod.Namespace)
	mode := updater.ModeAuto
	if pa.Spec.UpdatePolicy != nil {
		mode = updater.UpdateMode(pa.Spec.UpdatePolicy.UpdateMode)
	}

	strategy, updateErr := podUpdater.ApplyRecommendation(ctx, pod, rec, mode)
	if updateErr != nil {
		logger.Error(updateErr, "Failed to apply recommendation",
			"container", rec.ContainerName, "source", source)
		r.Recorder.Eventf(pa, corev1.EventTypeWarning, "UpdateFailed",
			"Failed to update pod %s container %s: %v",
			pod.Name, rec.ContainerName, updateErr)
		return 0
	}

	logger.Info("Applied resource recommendation",
		"container", rec.ContainerName,
		"strategy", strategy,
		"source", source,
		"cpuReq", fmt.Sprintf("%.3f", rec.CPUCoresRequest),
		"memReq", fmt.Sprintf("%.0f", rec.MemoryBytesRequest))
	r.Recorder.Eventf(pa, corev1.EventTypeNormal, "PodResourceAdjusted",
		"Applied %s recommendation to pod %s container %s via %s (cpu=%.3f, mem=%.0fMi)",
		source, pod.Name, rec.ContainerName, strategy,
		rec.CPUCoresRequest, rec.MemoryBytesRequest/(1024*1024))
	metrics.ResourceAdjustments.WithLabelValues(
		pod.Namespace, pod.Name, rec.ContainerName, "cpu").Inc()
	metrics.ResourceAdjustments.WithLabelValues(
		pod.Namespace, pod.Name, rec.ContainerName, "memory").Inc()
	return 1
}

// ---- Helpers ----

// cleanupManagedPods removes Podfather annotations from managed pods during CR deletion.
func (r *PodAutoscalerReconciler) cleanupManagedPods(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) {
	logger := logf.FromContext(ctx)
	selector, err := metav1.LabelSelectorAsSelector(pa.Spec.Selector)
	if err != nil {
		logger.Error(err, "Failed to parse selector during cleanup")
		return
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(pa.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		logger.Error(err, "Failed to list pods during cleanup")
		return
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if _, ok := pod.Annotations["podfather.io/last-recommendation"]; ok {
			patch := client.MergeFrom(pod.DeepCopy())
			delete(pod.Annotations, "podfather.io/last-recommendation")
			if pErr := r.Patch(ctx, pod, patch); pErr != nil {
				logger.Error(pErr, "Failed to remove annotation from pod", "pod", pod.Name)
			}
		}
	}
}

// setCondition sets a metav1.Condition on the PodAutoscaler status.
func (r *PodAutoscalerReconciler) setCondition(pa *autoscalingv1alpha1.PodAutoscaler, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range pa.Status.Conditions {
		if c.Type == condType {
			if c.Status != status || c.Reason != reason {
				pa.Status.Conditions[i].Status = status
				pa.Status.Conditions[i].Reason = reason
				pa.Status.Conditions[i].Message = message
				pa.Status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}
	pa.Status.Conditions = append(pa.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// requeueInterval derives the requeue duration from the CR spec.
func (r *PodAutoscalerReconciler) requeueInterval(pa *autoscalingv1alpha1.PodAutoscaler) time.Duration {
	if pa.Spec.MetricsCollectionIntervalSeconds != nil && *pa.Spec.MetricsCollectionIntervalSeconds >= 10 {
		return time.Duration(*pa.Spec.MetricsCollectionIntervalSeconds) * time.Second
	}
	return defaultRequeueAfter
}

// filterRunningPods returns only pods in Running phase.
func filterRunningPods(pods []corev1.Pod) []corev1.Pod {
	var running []corev1.Pod
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	return running
}

// buildCalculatorConfig constructs a calculator.Config from the PodAutoscaler spec.
func buildCalculatorConfig(pa *autoscalingv1alpha1.PodAutoscaler) calculator.Config {
	cfg := calculator.DefaultConfig()
	if pa.Spec.VarianceThresholdPercent != nil {
		cfg.VarianceThresholdPercent = float64(*pa.Spec.VarianceThresholdPercent)
	}
	return cfg
}

// toStatusRecommendation converts a calculator recommendation to the CRD status type.
func toStatusRecommendation(rec calculator.ResourceRecommendation) autoscalingv1alpha1.ContainerRecommendation {
	return autoscalingv1alpha1.ContainerRecommendation{
		ContainerName: rec.ContainerName,
		Target: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(rec.CPUCoresRequest*1000), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(rec.MemoryBytesRequest), resource.BinarySI),
		},
		UpperBound: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(rec.CPUCoresLimit*1000), resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(int64(rec.MemoryBytesLimit), resource.BinarySI),
		},
	}
}

// Ensure labels import is used (consumed by LabelSelectorAsSelector).
var _ = labels.Everything

// ---- Namespace Constraints Helpers ----

// buildNamespaceConstraintsStatus converts internal namespace constraints to the CRD status type.
func buildNamespaceConstraintsStatus(nc namespacelimits.NamespaceConstraints) *autoscalingv1alpha1.NamespaceConstraintsStatus {
	if !nc.LimitRangeFound && !nc.ResourceQuotaFound {
		return nil
	}

	status := &autoscalingv1alpha1.NamespaceConstraintsStatus{
		LimitRangeFound:    nc.LimitRangeFound,
		ResourceQuotaFound: nc.ResourceQuotaFound,
	}

	if nc.MinCPUCores > 0 {
		status.EffectiveMinCPU = resource.NewMilliQuantity(int64(nc.MinCPUCores*1000), resource.DecimalSI).String()
	}
	if nc.MaxCPUCores < 1e300 {
		status.EffectiveMaxCPU = resource.NewMilliQuantity(int64(nc.MaxCPUCores*1000), resource.DecimalSI).String()
	}
	if nc.MinMemoryBytes > 0 {
		status.EffectiveMinMemory = resource.NewQuantity(int64(nc.MinMemoryBytes), resource.BinarySI).String()
	}
	if nc.MaxMemoryBytes < 1e300 {
		status.EffectiveMaxMemory = resource.NewQuantity(int64(nc.MaxMemoryBytes), resource.BinarySI).String()
	}

	return status
}

// ---- VPA Integration Helpers ----

// vpaCheckResult holds the internal result of a VPA integration check.
// It contains both the CRD status and the raw recommendations for internal use.
type vpaCheckResult struct {
	status          *autoscalingv1alpha1.VPAIntegrationStatus
	recommendations []vpa.ContainerRecommendation
}

// checkVPAIntegration detects VPA presence and fetches recommendations if applicable.
// It returns a vpaCheckResult with the CRD status to persist and recommendations for the loop.
func (r *PodAutoscalerReconciler) checkVPAIntegration(ctx context.Context, pa *autoscalingv1alpha1.PodAutoscaler) vpaCheckResult {
	logger := logf.FromContext(ctx)

	// If VPA client is not configured or VPA integration is explicitly disabled, skip
	if r.VPAClient == nil {
		return vpaCheckResult{}
	}

	// Check if VPA policy is disabled
	if pa.Spec.VPAPolicy != nil && pa.Spec.VPAPolicy.Enabled != nil && !*pa.Spec.VPAPolicy.Enabled {
		logger.V(1).Info("VPA integration disabled by policy")
		return vpaCheckResult{
			status: &autoscalingv1alpha1.VPAIntegrationStatus{
				VPAInstalled: false,
			},
		}
	}

	// Check if VPA CRDs are installed
	if !r.VPAClient.IsVPAInstalled(ctx) {
		logger.V(1).Info("VPA CRDs not found in cluster")
		return vpaCheckResult{
			status: &autoscalingv1alpha1.VPAIntegrationStatus{
				VPAInstalled: false,
			},
		}
	}

	status := &autoscalingv1alpha1.VPAIntegrationStatus{
		VPAInstalled: true,
	}

	// Find a matching VPA — either by explicit name or by targetRef matching
	var matchedVPA *vpa.VPAInfo
	var err error

	if pa.Spec.VPAPolicy != nil && pa.Spec.VPAPolicy.VPAName != "" {
		matchedVPA, err = r.VPAClient.GetVPA(ctx, pa.Namespace, pa.Spec.VPAPolicy.VPAName)
		if err != nil {
			logger.Error(err, "Failed to get explicitly named VPA", "vpaName", pa.Spec.VPAPolicy.VPAName)
		}
	} else if pa.Spec.TargetRef != nil {
		matchedVPA, err = r.VPAClient.FindMatchingVPA(ctx, pa.Namespace, pa.Spec.TargetRef.Kind, pa.Spec.TargetRef.Name)
		if err != nil {
			logger.Error(err, "Failed to search for matching VPA")
		}
	}

	if matchedVPA == nil {
		logger.V(1).Info("No matching VPA found")
		return vpaCheckResult{status: status}
	}

	status.MatchingVPAName = matchedVPA.Name
	status.VPAUpdateMode = matchedVPA.UpdateMode

	// Validate that VPA is in "Off" mode to prevent conflicts
	if validationErr := vpa.ValidateVPAMode(matchedVPA); validationErr != nil {
		logger.Info("VPA conflict detected", "error", validationErr.Error())
		status.VPAConflict = validationErr.Error()
		r.Recorder.Eventf(pa, corev1.EventTypeWarning, "VPAModeConflict",
			"VPA %s has updateMode=%s (must be Off to avoid conflicts with Podfather)",
			matchedVPA.Name, matchedVPA.UpdateMode)
		return vpaCheckResult{status: status}
	}

	// VPA is in Off mode — clear any previous conflict
	r.setCondition(pa, "VPAConflict", metav1.ConditionFalse, "NoConflict", "VPA is in Off mode")

	// Check if VPA has recommendations
	if !matchedVPA.HasRecommendation || len(matchedVPA.ContainerRecommendations) == 0 {
		logger.Info("Matching VPA found but has no recommendations yet, using Podfather algorithm",
			"vpa", matchedVPA.Name)
		r.Recorder.Eventf(pa, corev1.EventTypeNormal, "VPANoRecommendation",
			"VPA %s found but has no recommendations yet; falling back to Podfather algorithm",
			matchedVPA.Name)
		return vpaCheckResult{status: status}
	}

	// Use VPA recommendations
	status.UsingVPARecommendations = true
	logger.Info("Using VPA recommendations instead of Podfather algorithm",
		"vpa", matchedVPA.Name,
		"containerCount", len(matchedVPA.ContainerRecommendations))
	r.Recorder.Eventf(pa, corev1.EventTypeNormal, "UsingVPARecommendations",
		"Using recommendations from VPA %s for %d containers",
		matchedVPA.Name, len(matchedVPA.ContainerRecommendations))

	return vpaCheckResult{
		status:          status,
		recommendations: matchedVPA.ContainerRecommendations,
	}
}

// vpaRecommendationToCalculatorRec converts a VPA container recommendation to
// a calculator.ResourceRecommendation so it can flow through the existing update pipeline.
func vpaRecommendationToCalculatorRec(rec vpa.ContainerRecommendation, constraints calculator.Constraints) calculator.ResourceRecommendation {
	result := calculator.ResourceRecommendation{
		ContainerName: rec.ContainerName,
	}

	// Use VPA target as request
	if cpu, ok := rec.Target[corev1.ResourceCPU]; ok {
		result.CPUCoresRequest = cpu.AsApproximateFloat64()
	}
	if mem, ok := rec.Target[corev1.ResourceMemory]; ok {
		result.MemoryBytesRequest = mem.AsApproximateFloat64()
	}

	// Use VPA upperBound as limit; fall back to target × 1.5 if upperBound absent
	if cpu, ok := rec.UpperBound[corev1.ResourceCPU]; ok {
		result.CPUCoresLimit = cpu.AsApproximateFloat64()
	} else {
		result.CPUCoresLimit = result.CPUCoresRequest * 1.5
	}
	if mem, ok := rec.UpperBound[corev1.ResourceMemory]; ok {
		result.MemoryBytesLimit = mem.AsApproximateFloat64()
	} else {
		result.MemoryBytesLimit = result.MemoryBytesRequest * 1.5
	}

	// Clamp to constraints
	result.CPUCoresRequest = clampFloat(result.CPUCoresRequest, constraints.MinCPUCores, constraints.MaxCPUCores)
	result.CPUCoresLimit = clampFloat(result.CPUCoresLimit, constraints.MinCPUCores, constraints.MaxCPUCores)
	result.MemoryBytesRequest = clampFloat(result.MemoryBytesRequest, constraints.MinMemoryBytes, constraints.MaxMemoryBytes)
	result.MemoryBytesLimit = clampFloat(result.MemoryBytesLimit, constraints.MinMemoryBytes, constraints.MaxMemoryBytes)

	return result
}

// variancePercent computes the percentage deviation between recommended and current values.
func variancePercent(recommended, current float64) float64 {
	if current == 0 {
		return 100
	}
	diff := recommended - current
	if diff < 0 {
		diff = -diff
	}
	return diff / current * 100
}

// clampFloat clamps a value between min and max.
func clampFloat(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
