package neatlogs

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

type retainingSpanProcessor struct {
	ended int
}

func TestBoundedProcessorReleasesMediaWhenQueueDropsSpan(t *testing.T) {
	diagnostics := &deliveryDiagnostics{}
	queue := newDeliveryQueue(1, diagnostics)
	store := internalmedia.NewStore(1024, 4)
	defer store.Close()
	processor := &boundedSpanProcessor{next: &retainingSpanProcessor{}, queue: queue, media: store}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
	tracer := provider.Tracer("media-queue-test")

	_, first := tracer.Start(context.Background(), "first")
	if _, reason := internalmedia.Stage(first.SpanContext(), []byte("first"), "image/png"); reason != "" {
		t.Fatal(reason)
	}
	first.End()
	_, second := tracer.Start(context.Background(), "second")
	if _, reason := internalmedia.Stage(second.SpanContext(), []byte("second"), "image/png"); reason != "" {
		t.Fatal(reason)
	}
	second.End()

	if items, retained := store.Snapshot(); items != 1 || retained != len("first") {
		t.Fatalf("queue drop retained media = %d/%d", items, retained)
	}
}

func (*retainingSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (p *retainingSpanProcessor) OnEnd(sdktrace.ReadOnlySpan)                   { p.ended++ }
func (*retainingSpanProcessor) ForceFlush(context.Context) error                { return nil }
func (*retainingSpanProcessor) Shutdown(context.Context) error                  { return nil }

func TestBoundedProcessorExposesQueueSaturation(t *testing.T) {
	diagnostics := &deliveryDiagnostics{}
	queue := newDeliveryQueue(1, diagnostics)
	next := &retainingSpanProcessor{}
	processor := &boundedSpanProcessor{next: next, queue: queue}
	span := tracetest.SpanStub{SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
	})}.Snapshot()

	processor.OnEnd(span)
	processor.OnEnd(span)

	if next.ended != 1 {
		t.Fatalf("forwarded spans = %d, want 1", next.ended)
	}
	if got := diagnostics.snapshot().SpanQueueDrops; got != 1 {
		t.Fatalf("queue drops = %d, want 1", got)
	}
}
