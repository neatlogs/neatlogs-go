package neatlogs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// completionMarkerName is the internal span the backend uses to finalize a
// trace. The Python and TypeScript SDKs emit the same marker; without it the
// backend receives spans but never surfaces the completed trace.
const completionMarkerName = "neatlogs.trace.complete"

// completionProcessor emits a neatlogs.trace.complete marker span whenever a
// root span (one with no parent) ends. The marker shares the root's trace and
// is parented to it, signalling the backend that the trace is complete.
//
// This runs as a SpanProcessor rather than in the exporter because it must
// create a new span on the same provider; the marker then flows through the
// normalizing exporter like any other span.
type completionProcessor struct {
	tracer trace.Tracer
}

var _ sdktrace.SpanProcessor = (*completionProcessor)(nil)

func (p *completionProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (p *completionProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	// Only root spans complete a trace; skip the marker itself to avoid
	// recursing (the marker is parented to the root, so it is not itself root,
	// but guard by name regardless).
	if s.Parent().HasSpanID() || s.Name() == completionMarkerName {
		return
	}

	// Re-parent a new span onto the ending root's context so the marker shares
	// its trace ID and points at the root as parent.
	rootCtx := trace.ContextWithSpanContext(context.Background(), s.SpanContext())
	attrs := []attribute.KeyValue{
		attribute.Bool(completionMarkerName, true),
		attribute.Bool("neatlogs.internal", true),
		attribute.String("neatlogs.span.kind", "Neatlogs.INTERNAL"),
	}
	// Carry forward trace-level tags from the resource, mirroring the TS SDK.
	if res := s.Resource(); res != nil {
		if v, ok := res.Set().Value("neatlogs.tags"); ok {
			attrs = append(attrs, attribute.String("neatlogs.tags", v.AsString()))
		}
	}

	_, marker := p.tracer.Start(rootCtx, completionMarkerName, trace.WithAttributes(attrs...))
	marker.End()
}

func (p *completionProcessor) Shutdown(context.Context) error   { return nil }
func (p *completionProcessor) ForceFlush(context.Context) error { return nil }
