package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// InitTracer initialises a global OTel tracer provider that exports spans
// to the given OTLP HTTP endpoint (e.g. "jaeger:4318"). It registers a W3C
// TraceContext propagator and an AlwaysSample sampler.
//
// Returns a shutdown function that flushes pending spans; safe to defer.
// If otlpEndpoint is empty, this is a no-op (returns a no-op shutdown and
// nil error) — useful for unit tests and CI runs without a collector.
func InitTracer(ctx context.Context, serviceName, zone, otlpEndpoint string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	if otlpEndpoint == "" {
		return noop, nil
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(otlpEndpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return noop, fmt.Errorf("otlp exporter: %w", err)
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		attribute.String("zone", zone),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}
