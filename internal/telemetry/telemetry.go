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

// Package telemetry provides OpenTelemetry initialization for Podfather.
//
// It configures an OTLP gRPC trace exporter that sends spans to a
// collector (Jaeger, Tempo, etc.) at a user-specified endpoint.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/des57/podfather"

// InitProvider initialises an OTLP gRPC tracing pipeline and registers
// it as the global TracerProvider. The returned shutdown function must be
// called during graceful termination (it flushes pending spans).
func InitProvider(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("podfather"),
			semconv.ServiceVersionKey.String("0.1.0"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// Tracer returns a named tracer for creating spans in the Podfather
// reconciliation loop.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}
