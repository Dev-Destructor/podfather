# GitHub Copilot Instructions: Podfather Project

## Project Overview
Podfather is a Kubernetes Custom Controller (Operator) designed to optimize pod resource allocation dynamically. Its primary function is to watch pods, collect resource usage metrics, and seamlessly adjust resource requests and limits based on actual needs (over-allocation or under-allocation) to maximize cluster efficiency.

Our target is to achieve **Level 5 Capability (Auto Pilot)** on the Operator Capability Model.

## Technology Stack
* **Language**: Golang (Go 1.21+)
* **Framework**: Operator SDK
* **Metrics & Observability**: OpenTelemetry, Prometheus, Grafana
* **Communication**: gRPC (for any internal microservice communications)
* **Kubernetes APIs**: `client-go`, `controller-runtime`

## Target Capabilities (Level 5)
Generate code and suggest architectures that fulfill the following maturity phases:
1.  **Phase I (Basic Install)**: Use Operator SDK to scaffold the project. Support seamless installation via OLM (Operator Lifecycle Manager) and Helm.
2.  **Phase II (Seamless Upgrades)**: Implement CRD versioning and conversion webhooks for seamless patch and minor version upgrades. Support safe rollbacks.
3.  **Phase III (Full Lifecycle)**: Integrate automated backup and recovery mechanisms for the operator's state. 
4.  **Phase IV (Deep Insights)**: Instrument the controller using OpenTelemetry. Expose deep, custom metrics (e.g., allocation adjustments made, pod starvation events) to Prometheus and generate alert structures and Grafana dashboard configurations to analyze system performance under heavy load.
5.  **Phase V (Auto Pilot)**: Implement auto-scaling, auto-healing, and abnormality detection. The system must autonomously detect anomalous resource spikes and heal starved pods without manual intervention.

## Reconciliation Loop Logic (Strict Adherence)
When generating or refactoring the `Reconcile` function, follow this specific flowchart logic:
1.  **Watch Event Trigger**: Start reconciliation loop on Pod creation or update.
2.  **New Pod Check**: Determine if the pod is newly managed.
    * *If Yes*: Allocate default baseline requests/limits and register the pod into the monitoring list.
3.  **Metrics Collection**: Periodically fetch and aggregate resource usage metrics for all monitored pods.
4.  **Evaluation Phase**: Compare current metrics against current allocations. Calculate the ideal new resource requests and limits.
5.  **Over/Under Allocation Check**: 
    * *If significant variance exists*: Proceed to update.
    * *If optimal*: Requeue after a standard interval.
6.  **Update Execution**: Update the Pod Spec with the newly calculated resources. 
    * *Implementation Note*: Prefer Kubernetes' native In-Place Pod Vertical Scaling (if cluster version supports it), otherwise implement a safe eviction/recreation strategy ensuring zero downtime via PodDisruptionBudgets.
7.  **Error Handling**: If the update fails, handle the error gracefully with exponential backoff and retry the operation. If successful, loop back to continuous monitoring.

## Golang & Operator Best Practices
When writing code, adhere to the following industry standards:

### 1. Code Quality & Structure
* **Clean Architecture**: Separate business logic (calculating ideal resources) from Kubernetes API interactions.
* **Idiomatic Go**: Follow standard Go formatting (`gofmt`), utilize effective Go principles, and ensure zero linting errors (`golangci-lint`).
* **Contexts**: Always pass `context.Context` down the call stack. Honor context cancellations and timeouts, especially during external metric fetching.
* **Concurrency**: Use goroutines and channels safely. Prevent race conditions and resource leaks when running parallel metric collection.

### 2. Controller-Runtime Usage
* **Client**: Use the cached `client.Client` provided by `controller-runtime` for READ operations to reduce API server load. Use live API calls only when absolutely necessary.
* **Status Updates**: Always update the Custom Resource (CR) status using the `.Status().Update()` or `.Status().Patch()` methods.
* **Event Recording**: Emit rich Kubernetes Events (`record.EventRecorder`) for every significant action (e.g., "PodResourceAdjusted", "MetricsCollectionFailed") to aid in debugging.

### 3. Error Handling & Logging
* **Meaningful Errors**: Wrap errors with context using `fmt.Errorf("failed to calculate metrics for pod %s: %w", podName, err)`.
* **Structured Logging**: Use `logr.Logger` (standard in controller-runtime). Log keys and values explicitly (e.g., `logger.Info("Updating pod spec", "pod", pod.Name, "namespace", pod.Namespace)`).

### 4. Testing
* **Unit Tests**: Write comprehensive table-driven unit tests for the core calculation logic (evaluating under/over allocation).
* **EnvTest**: Use `controller-runtime/pkg/envtest` for integration testing the reconciliation loop against a localized API server.