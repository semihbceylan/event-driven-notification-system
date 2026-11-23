package tracing

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceParentRoundTrip(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("test").Start(context.Background(), "api request", trace.WithSpanKind(trace.SpanKindServer))
	traceParent := TraceParent(ctx)
	span.End()
	if traceParent == "" {
		t.Fatal("expected a traceparent for a sampled span")
	}

	// Another process continues the trace from the stored value.
	ctx2 := WithTraceParent(context.Background(), traceParent)
	_, child := tp.Tracer("test").Start(ctx2, "deliver")
	child.End()

	spans := rec.Ended()
	if spans[1].SpanContext().TraceID() != spans[0].SpanContext().TraceID() {
		t.Error("continued span is in a different trace")
	}
	if spans[1].Parent().SpanID() != spans[0].SpanContext().SpanID() {
		t.Error("continued span's parent is not the original span")
	}
}

func TestTraceParentEmptyWithoutSpan(t *testing.T) {
	if got := TraceParent(context.Background()); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if ctx := WithTraceParent(context.Background(), "garbage"); trace.SpanContextFromContext(ctx).IsValid() {
		t.Error("an invalid traceparent must not produce a span context")
	}
}

func TestSampler(t *testing.T) {
	start := func(roots Roots, kind trace.SpanKind, parent context.Context) bool {
		tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(NewSampler(roots)))
		_, span := tp.Tracer("test").Start(parent, "span", trace.WithSpanKind(kind))
		defer span.End()
		return span.SpanContext().IsSampled()
	}

	if !start(ServerRoots, trace.SpanKindServer, context.Background()) {
		t.Error("API: incoming requests must start traces")
	}
	if start(ServerRoots, trace.SpanKindClient, context.Background()) {
		t.Error("API: a DB query without a request (e.g. a metrics scrape) must not start a trace")
	}
	if start(NoRoots, trace.SpanKindInternal, context.Background()) {
		t.Error("worker: an idle queue poll must not start a trace")
	}

	// With a sampled parent (the stored traceparent), the worker records.
	rec := sdktrace.NewTracerProvider()
	parentCtx, parent := rec.Tracer("test").Start(context.Background(), "api")
	defer parent.End()
	if !start(NoRoots, trace.SpanKindInternal, WithTraceParent(context.Background(), TraceParent(parentCtx))) {
		t.Error("worker: a delivery continuing a sampled trace must be recorded")
	}
}
