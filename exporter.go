package neatlogs

import (
	"context"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/neatlogs/neatlogs-go/internal/attributes"
)

// normalizingExporter wraps a SpanExporter and rewrites every span's attributes
// into the neatlogs.* namespace before delegating the actual export.
//
// Go's OTel SpanProcessor.OnEnd receives a read-only span, so attribute
// normalization cannot happen in a processor (unlike the JS/Python SDKs). We
// normalize at the exporter boundary instead, round-tripping each span through
// a tracetest.SpanStub whose attributes we can edit.
//
// This is what makes spans from any OTel-native source — notably Google ADK,
// which emits OpenTelemetry GenAI semantic-convention attributes through the
// global TracerProvider — arrive already keyed by the neatlogs.* contract, with
// no per-call instrumentation.
type normalizingExporter struct {
	next   trace.SpanExporter
	mapper *attributes.Mapper
}

var _ trace.SpanExporter = (*normalizingExporter)(nil)

func (e *normalizingExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	rewritten := make([]trace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		stub := tracetest.SpanStubFromReadOnlySpan(s)
		stub.Attributes = e.mapper.Normalize(stub.Attributes)
		rewritten[i] = stub.Snapshot()
	}
	return e.next.ExportSpans(ctx, rewritten)
}

func (e *normalizingExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}
