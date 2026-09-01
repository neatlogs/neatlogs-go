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
	uploads  uploadAuthority
}

type spanExportAction struct {
	batch    []sdktrace.ReadOnlySpan
	overflow sdktrace.ReadOnlySpan
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
	actions := make([]spanExportAction, 0, len(spans))
	batch := make([]sdktrace.ReadOnlySpan, 0, len(spans))
	batchBytes := 0
	for _, span := range spans {
		spanBytes := encodedSpanUpperBound(span)
		if spanBytes > e.maxBytes {
			if len(batch) > 0 {
				actions = append(actions, spanExportAction{batch: batch})
				batch = nil
				batchBytes = 0
			}
			actions = append(actions, spanExportAction{overflow: span})
			continue
		}
		if len(batch) > 0 && batchBytes+spanBytes > e.maxBytes {
			actions = append(actions, spanExportAction{batch: batch})
			batch = nil
			batchBytes = 0
		}
		batch = append(batch, span)
		batchBytes += spanBytes
	}
	if len(batch) > 0 {
		actions = append(actions, spanExportAction{batch: batch})
	}

	var overflowErr error
	for index, action := range actions {
		if action.overflow != nil {
			if err := e.exportOverflow(ctx, action.overflow); err != nil {
				if overflowErr == nil {
					overflowErr = err
				}
			}
			continue
		}
		if err := e.next.ExportSpans(ctx, action.batch); err != nil {
			e.recordFailure(actionCount(actions[index:]))
			return err
		}
	}
	return overflowErr
}

func actionCount(actions []spanExportAction) int {
	count := 0
	for _, action := range actions {
		if action.overflow != nil {
			count++
		} else {
			count += len(action.batch)
		}
	}
	return count
}

func (e *byteLimitedExporter) exportOverflow(ctx context.Context, span sdktrace.ReadOnlySpan) error {
	if err := ctx.Err(); err != nil {
		uploadErr := newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
		e.recordOverflowFailure(uploadErr)
		return uploadErr
	}
	if e.uploads == nil {
		err := newUploadFailure("prepare", uploadUnavailableReason, false)
		if e.delivery != nil {
			e.delivery.otlpOverflowUnavailable.Add(1)
			e.delivery.otlpOverflowFailures.Add(1)
			e.delivery.spanExportFailures.Add(1)
			e.delivery.recordUploadFailure(err)
		}
		return err
	}
	if encodedSpanUpperBound(span) > maxOTLPOverflowBytes {
		err := newUploadFailure("prepare", "payload_too_large", false)
		e.recordOverflowFailure(err)
		return err
	}
	content, err := encodeSpanEnvelope(span)
	if err != nil {
		uploadErr := newUploadFailure("prepare", "encode_failed", false)
		e.recordOverflowFailure(uploadErr)
		return uploadErr
	}
	digestHex, err := uploadDigest(ctx, content)
	if err != nil {
		uploadErr := newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
		e.recordOverflowFailure(uploadErr)
		return uploadErr
	}
	payload := uploadPayload{
		Content: content, Purpose: uploadPurposeOTLPOverflow,
		MIMEType: "application/x-protobuf", ContentEncoding: uploadEncodingIdentity,
		PayloadSchema: uploadSchemaTracesV1,
		IdempotencyKey: uploadIdempotencyKey(
			uploadPurposeOTLPOverflow, "application/x-protobuf", uploadEncodingIdentity, uploadSchemaTracesV1, digestHex,
		),
	}
	receipt, err := e.uploads.Upload(ctx, payload)
	if err != nil {
		e.recordOverflowFailure(err)
		return err
	}
	if !uploadReceiptReady(receipt) {
		err := newUploadFailure("complete", "invalid_receipt", false)
		e.recordOverflowFailure(err)
		return err
	}
	if e.delivery != nil {
		e.delivery.otlpOverflowUploads.Add(1)
	}
	return nil
}

func (e *byteLimitedExporter) recordOverflowFailure(err error) {
	if e.delivery != nil {
		e.delivery.otlpOverflowFailures.Add(1)
		e.delivery.spanExportFailures.Add(1)
		e.delivery.recordUploadFailure(err)
	}
}

func (e *byteLimitedExporter) recordFailure(count int) {
	if e.delivery != nil && count > 0 {
		e.delivery.spanExportFailures.Add(uint64(count))
	}
}

func (e *byteLimitedExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}
