package neatlogs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestDatasetScopeRequiresEndedAcknowledgedHierarchy(t *testing.T) {
	registry := newDatasetCaptureRegistry()
	scope, _ := registry.begin()
	defer scope.release()
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(registry), sdktrace.WithSyncer(sink))
	defer tp.Shutdown(context.Background())
	ctx := context.WithValue(context.Background(), datasetScopeKey{}, scope)
	rootCtx, root := tp.Tracer("test").Start(ctx, "root")
	_, child := tp.Tracer("test").Start(rootCtx, "tool")
	root.End()
	if _, err := scope.manifest(); err == nil {
		t.Fatal("accepted open child")
	}
	child.End()
	if _, err := scope.manifest(); err == nil {
		t.Fatal("accepted unacknowledged exports")
	}
	registry.acknowledge(sink.GetSpans().Snapshots())
	scope.closeExecution()
	manifest, err := scope.manifest()
	if err != nil || len(manifest) != 1 || len(manifest[0].SpanIDs) != 2 {
		t.Fatalf("%+v %v", manifest, err)
	}
	if registry.allows(ctx) {
		t.Fatal("allows spans after callback returned")
	}
}

func TestDatasetScopeIsolatesConcurrentAndForeignRegistries(t *testing.T) {
	registry := newDatasetCaptureRegistry()
	a, _ := registry.begin()
	defer a.release()
	b, _ := registry.begin()
	defer b.release()
	foreign := newDatasetCaptureRegistry()
	sink := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(registry), sdktrace.WithSpanProcessor(foreign), sdktrace.WithSyncer(sink))
	defer tp.Shutdown(context.Background())
	for _, scope := range []*datasetCaptureScope{a, b} {
		_, span := tp.Tracer("test").Start(context.WithValue(context.Background(), datasetScopeKey{}, scope), "root")
		span.End()
	}
	_, outside := tp.Tracer("test").Start(context.Background(), "outside")
	outside.End()
	registry.acknowledge(sink.GetSpans().Snapshots())
	for _, scope := range []*datasetCaptureScope{a, b} {
		manifest, err := scope.manifest()
		if err != nil || len(manifest) != 1 || len(manifest[0].SpanIDs) != 1 {
			t.Fatalf("%+v %v", manifest, err)
		}
	}
	if len(foreign.bySpan) != 0 {
		t.Fatal("foreign registry captured spans")
	}
}

func TestDatasetScopeRejectsDroppedAndTruncatedEvidence(t *testing.T) {
	for _, attrs := range [][]attribute.KeyValue{{attribute.Bool("neatlogs.input.truncated", true)}, {attribute.String("neatlogs.input.0.state", "upload-failed")}} {
		registry := newDatasetCaptureRegistry()
		scope, _ := registry.begin()
		sink := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(registry), sdktrace.WithSyncer(sink))
		_, span := tp.Tracer("test").Start(context.WithValue(context.Background(), datasetScopeKey{}, scope), "root", trace.WithAttributes(attrs...))
		span.End()
		registry.acknowledge(sink.GetSpans().Snapshots())
		if _, err := scope.manifest(); err == nil {
			t.Fatal("accepted incomplete evidence")
		}
		tp.Shutdown(context.Background())
		scope.release()
	}
}

func TestDatasetScopeBoundsRegistry(t *testing.T) {
	registry := newDatasetCaptureRegistry()
	for range maxDatasetCaptureScopes {
		if _, err := registry.begin(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := registry.begin(); err == nil {
		t.Fatal("accepted unbounded scopes")
	}
}
