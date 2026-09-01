package neatlogs

import (
	"context"
	"fmt"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const defaultMaxExportBytes = 4 * 1024 * 1024

// byteLimitedExporter splits an OTel row batch by a conservative OTLP/protobuf
// byte limit. encodedSpanUpperBound serializes each span as its own complete
// request, so summing sizes over-counts shared resource/scope framing and never
// underestimates the corresponding combined request.
type byteLimitedExporter struct {
	next     sdktrace.SpanExporter
	maxBytes int
	delivery *deliveryDiagnostics
}

func newByteLimitedExporter(next sdktrace.SpanExporter, maxBytes int, delivery *deliveryDiagnostics) (*byteLimitedExporter, error) {
	if next == nil {
		return nil, fmt.Errorf("neatlogs: byte-limited exporter requires a delegate")
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("neatlogs: max export bytes must be greater than zero")
	}
	return &byteLimitedExporter{next: next, maxBytes: maxBytes, delivery: delivery}, nil
}

func (e *byteLimitedExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	batch := make([]sdktrace.ReadOnlySpan, 0, len(spans))
	batchBytes := 0
	accepted := 0
	for _, span := range spans {
		spanBytes := encodedSpanUpperBound(span)
		if len(batch) > 0 && batchBytes+spanBytes > e.maxBytes {
			if err := e.next.ExportSpans(ctx, batch); err != nil {
				e.recordFailure(len(spans) - accepted)
				return err
			}
			accepted += len(batch)
			batch = batch[:0]
			batchBytes = 0
		}
		// An individually oversized span is forwarded intact. The claim-check
		// transport is a later phase; this layer never truncates or drops data.
		batch = append(batch, span)
		batchBytes += spanBytes
	}
	if len(batch) == 0 {
		return nil
	}
	if err := e.next.ExportSpans(ctx, batch); err != nil {
		e.recordFailure(len(spans) - accepted)
		return err
	}
	return nil
}

func (e *byteLimitedExporter) recordFailure(count int) {
	if e.delivery != nil && count > 0 {
		e.delivery.spanExportFailures.Add(uint64(count))
	}
}

func (e *byteLimitedExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}
