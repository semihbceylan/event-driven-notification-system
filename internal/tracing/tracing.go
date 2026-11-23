// Package tracing sets up OpenTelemetry distributed tracing.
//
// A notification's journey spans two processes that never talk directly:
// the API stores it, and a worker sends it later, possibly minutes later
// and several times. To keep that in one trace, the API stores the W3C
// trace context ("traceparent") on the notification row, and the worker
// continues the trace from it. One trace in Jaeger then shows the API
// request, its SQL, and every delivery attempt with its provider call.
//
// Tracing is off unless OTEL_EXPORTER_OTLP_ENDPOINT is set (the standard
// OpenTelemetry variable), so tests and local runs need no collector.
package tracing

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Roots decides which spans may start a new trace.
type Roots int

const (
	// ServerRoots: only incoming HTTP requests start traces (the API).
	// Background work such as Prometheus scraping the queue depth does not.
	ServerRoots Roots = iota
	// NoRoots: only continue traces that started elsewhere (the worker).
	// Its idle queue polls and reaper runs would otherwise each be a trace.
	NoRoots
)

// Setup installs the global tracer provider and propagator. The returned
// function flushes pending spans; call it on shutdown.
func Setup(ctx context.Context, service string, roots Roots, logger *slog.Logger) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		logger.Info("tracing disabled: OTEL_EXPORTER_OTLP_ENDPOINT is not set")
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracehttp.New(ctx) // reads the OTEL_EXPORTER_OTLP_* variables
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(rootSampler{roots})),
	)
	otel.SetTracerProvider(tp)
	logger.Info("tracing enabled", "endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	return tp.Shutdown, nil
}

// NewSampler is the sampler Setup uses, exported for tests.
func NewSampler(roots Roots) sdktrace.Sampler {
	return sdktrace.ParentBased(rootSampler{roots})
}

// rootSampler decides for spans without a parent. Spans with a parent
// follow the parent's decision (ParentBased).
type rootSampler struct{ roots Roots }

func (s rootSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if s.roots == ServerRoots && p.Kind == trace.SpanKindServer {
		return sdktrace.AlwaysSample().ShouldSample(p)
	}
	return sdktrace.NeverSample().ShouldSample(p)
}

func (s rootSampler) Description() string {
	if s.roots == ServerRoots {
		return "ServerRootsOnly"
	}
	return "NoRoots"
}

// TraceParent returns the W3C traceparent of the span in ctx, or "" if
// there is no sampled span (for example when tracing is off).
func TraceParent(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsSampled() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// WithTraceParent returns ctx continuing the trace described by
// traceParent. An empty or invalid value returns ctx unchanged.
func WithTraceParent(ctx context.Context, traceParent string) context.Context {
	if traceParent == "" {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": traceParent})
}

// TraceID returns the trace ID of the span in ctx, for log lines, or "".
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
