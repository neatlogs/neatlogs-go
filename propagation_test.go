package neatlogs

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestInitLeavesGlobalOpenTelemetryStateUntouched(t *testing.T) {
	ctx := context.Background()
	globalProvider := otel.GetTracerProvider()
	globalPropagator := otel.GetTextMapPropagator()
	sink := tracetest.NewInMemoryExporter()

	shutdown, err := Init(ctx, Config{WorkflowName: "private"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	if got := otel.GetTracerProvider(); got != globalProvider {
		t.Fatal("Init replaced the process-global tracer provider")
	}
	if got := otel.GetTextMapPropagator(); !reflect.DeepEqual(got, globalPropagator) {
		t.Fatal("Init replaced the process-global text-map propagator")
	}
}

func TestPrivateTraceContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := Init(ctx, Config{WorkflowName: "private"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	parentCtx, parent, endParent := StartSpan(ctx, "typescript.tool", "TOOL")
	headers := http.Header{}
	InjectTraceContext(parentCtx, propagation.HeaderCarrier(headers))
	if headers.Get("traceparent") == "" {
		t.Fatal("expected traceparent header")
	}

	remoteCtx := ExtractTraceContext(
		context.Background(),
		propagation.HeaderCarrier(headers),
	)
	_, child, endChild := StartSpan(remoteCtx, "go.tool.list_formulas", "TOOL")
	childContext := child.SpanContext()
	parentContext := parent.SpanContext()
	endChild()
	endParent()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if childContext.TraceID() != parentContext.TraceID() {
		t.Fatalf(
			"child trace id %s does not match parent %s",
			childContext.TraceID(),
			parentContext.TraceID(),
		)
	}
	childStub := byName(sink, "go.tool.list_formulas")
	if childStub.Parent.SpanID() != parentContext.SpanID() {
		t.Fatalf(
			"child parent span id %s does not match %s",
			childStub.Parent.SpanID(),
			parentContext.SpanID(),
		)
	}
}

func TestPrivateProviderRejectsForeignContext(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := Init(ctx, Config{WorkflowName: "private"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	foreignTraceID, _ := trace.TraceIDFromHex("11111111111111111111111111111111")
	foreignSpanID, _ := trace.SpanIDFromHex("2222222222222222")
	foreign := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    foreignTraceID,
		SpanID:     foreignSpanID,
		TraceFlags: trace.FlagsSampled,
	})
	foreignCtx := trace.ContextWithSpanContext(ctx, foreign)

	privateCtx, span, end := StartSpan(foreignCtx, "private.workflow", "workflow")
	privateSpanContext := span.SpanContext()
	end()

	_, llmSpan, endLLM := StartProviderSpan(foreignCtx, "private.llm", "llm")
	llmSpanContext := llmSpan.SpanContext()
	endLLM()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if privateSpanContext.TraceID() == foreignTraceID {
		t.Fatal("private span adopted a foreign trace id")
	}
	if got := byName(sink, "private.workflow").Parent; got.IsValid() {
		t.Fatalf("private root retained foreign parent %s", got.SpanID())
	}
	if llmSpanContext.TraceID() == foreignTraceID {
		t.Fatal("auto-rooted provider span adopted a foreign trace id")
	}
	llmStub := byName(sink, "private.llm")
	if !llmStub.Parent.IsValid() {
		t.Fatal("provider span did not receive a private workflow auto-root")
	}
	if llmStub.Parent.TraceID() != llmSpanContext.TraceID() {
		t.Fatal("provider span parent belongs to a different trace")
	}
	if got := trace.SpanContextFromContext(privateCtx); got.TraceID() != foreign.TraceID() ||
		got.SpanID() != foreign.SpanID() {
		t.Fatal("Neatlogs replaced the foreign active span visible to other instrumentation")
	}

	foreignHeaders := http.Header{}
	InjectTraceContext(foreignCtx, propagation.HeaderCarrier(foreignHeaders))
	if got := foreignHeaders.Get("traceparent"); got != "" {
		t.Fatalf("injected foreign trace context: %s", got)
	}

	privateHeaders := http.Header{}
	InjectTraceContext(privateCtx, propagation.HeaderCarrier(privateHeaders))
	if got := privateHeaders.Get("traceparent"); got == "" {
		t.Fatal("private trace context was not injected")
	}
}

func TestStaleShutdownCannotStopReinitializedProvider(t *testing.T) {
	ctx := context.Background()
	firstSink := tracetest.NewInMemoryExporter()
	firstShutdown, err := Init(ctx, Config{WorkflowName: "first"}, WithExporter(firstSink))
	if err != nil {
		t.Fatal(err)
	}
	if err := firstShutdown(ctx); err != nil {
		t.Fatal(err)
	}

	secondSink := tracetest.NewInMemoryExporter()
	secondShutdown, err := Init(ctx, Config{WorkflowName: "second"}, WithExporter(secondSink))
	if err != nil {
		t.Fatal(err)
	}
	defer secondShutdown(ctx)

	if err := firstShutdown(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, end := Trace(ctx, "second.trace")
	end()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if byName(secondSink, "second.trace").Name != "second.trace" {
		t.Fatal("stale shutdown stopped the reinitialized provider")
	}
}
