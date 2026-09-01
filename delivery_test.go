package neatlogs

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type retainingSpanProcessor struct {
	ended int
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
