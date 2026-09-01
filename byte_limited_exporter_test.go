package neatlogs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type batchRecordingExporter struct {
	batches [][]sdktrace.ReadOnlySpan
}

func (e *batchRecordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.batches = append(e.batches, append([]sdktrace.ReadOnlySpan(nil), spans...))
	return nil
}

func (*batchRecordingExporter) Shutdown(context.Context) error { return nil }

func sizedTestSpans(count, payloadBytes int) []sdktrace.ReadOnlySpan {
	spans := make([]sdktrace.ReadOnlySpan, 0, count)
	for index := range count {
		stub := tracetest.SpanStub{
			Name: "span",
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{1}, SpanID: trace.SpanID{byte(index + 1)},
			}),
			Attributes: []attribute.KeyValue{
				attribute.String("neatlogs.llm.input", strings.Repeat("x", payloadBytes)),
			},
		}
		spans = append(spans, stub.Snapshot())
	}
	return spans
}

func TestByteLimitedExporterSplitsOnEncodedProtobufUpperBound(t *testing.T) {
	spans := sizedTestSpans(3, 2048)
	sink := &batchRecordingExporter{}
	exporter, err := newByteLimitedExporter(sink, encodedSpanUpperBound(spans[0])*2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(context.Background(), spans); err != nil {
		t.Fatal(err)
	}
	got := make([]int, len(sink.batches))
	for index := range sink.batches {
		got[index] = len(sink.batches[index])
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("batch sizes = %v, want [2 1]", got)
	}
}

func TestByteLimitedExporterRejectsOversizedSpanWhenUploadsAreDisabled(t *testing.T) {
	spans := sizedTestSpans(1, 16_384)
	sink := &batchRecordingExporter{}
	diagnostics := &deliveryDiagnostics{}
	exporter, err := newByteLimitedExporter(sink, 128, diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(context.Background(), spans); err == nil {
		t.Fatal("oversized export unexpectedly succeeded without upload authority")
	}
	if len(sink.batches) != 0 {
		t.Fatal("oversized span reached ordinary OTLP delegate")
	}
	snapshot := diagnostics.snapshot()
	if snapshot.OTLPOverflowUnavailable != 1 || snapshot.OTLPOverflowFailures != 1 || snapshot.SpanExportFailures != 1 {
		t.Fatalf("overflow diagnostics = %#v", snapshot)
	}
}

type recordingUploadAuthority struct {
	payloads []uploadPayload
	receipt  uploadReceipt
	err      error
}

func (a *recordingUploadAuthority) Upload(_ context.Context, payload uploadPayload) (uploadReceipt, error) {
	a.payloads = append(a.payloads, payload)
	return a.receipt, a.err
}

func readyUploadReceipt() uploadReceipt {
	return uploadReceipt{
		UploadID: "0198f1ea-70ce-7c6d-8bbc-b08a19c58280", State: "ready",
		Reference: uploadReference{ID: "0198f1ea-70ce-7c6d-8bbc-b08a19c58280", State: "ready"},
	}
}

func TestByteLimitedExporterUploadsCompleteOversizedSpanWithoutDuplicateSend(t *testing.T) {
	spans := sizedTestSpans(1, 16_384)
	sink := &batchRecordingExporter{}
	diagnostics := &deliveryDiagnostics{}
	authority := &recordingUploadAuthority{receipt: readyUploadReceipt()}
	exporter, err := newByteLimitedExporter(sink, 128, diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	exporter.uploads = authority
	if err := exporter.ExportSpans(context.Background(), spans); err != nil {
		t.Fatal(err)
	}
	if len(sink.batches) != 0 {
		t.Fatal("uploaded overflow was also sent through ordinary OTLP")
	}
	if len(authority.payloads) != 1 {
		t.Fatalf("upload calls = %d, want 1", len(authority.payloads))
	}
	payload := authority.payloads[0]
	if payload.Purpose != uploadPurposeOTLPOverflow || payload.PayloadSchema != uploadSchemaTracesV1 ||
		payload.MIMEType != "application/x-protobuf" || payload.ContentEncoding != uploadEncodingIdentity {
		t.Fatalf("overflow payload metadata = %#v", payload)
	}
	if len(payload.Content) != encodedSpanUpperBound(spans[0]) {
		t.Fatalf("complete envelope bytes = %d, want %d", len(payload.Content), encodedSpanUpperBound(spans[0]))
	}
	if diagnostics.snapshot().OTLPOverflowUploads != 1 {
		t.Fatalf("overflow diagnostics = %#v", diagnostics.snapshot())
	}
}

type failAfterExporter struct {
	batches   [][]sdktrace.ReadOnlySpan
	successes int
}

func (e *failAfterExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.batches = append(e.batches, append([]sdktrace.ReadOnlySpan(nil), spans...))
	if len(e.batches) > e.successes {
		return errors.New("delegate failed")
	}
	return nil
}

func (*failAfterExporter) Shutdown(context.Context) error { return nil }

func TestByteLimitedExporterCountsOnlyFailedAndUnattemptedSpans(t *testing.T) {
	spans := sizedTestSpans(5, 2048)
	diagnostics := &deliveryDiagnostics{}
	sink := &failAfterExporter{successes: 1}
	exporter, err := newByteLimitedExporter(sink, encodedSpanUpperBound(spans[0])*2, diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(context.Background(), spans); err == nil {
		t.Fatal("split export unexpectedly succeeded")
	}
	if got := diagnostics.snapshot().SpanExportFailures; got != 3 {
		t.Fatalf("export failures = %d, want failed second batch plus unattempted span (3)", got)
	}
	batchSizes := make([]int, len(sink.batches))
	for index := range sink.batches {
		batchSizes[index] = len(sink.batches[index])
	}
	if len(batchSizes) != 2 || batchSizes[0] != 2 || batchSizes[1] != 2 {
		t.Fatalf("attempted batches = %v, want [2 2]", batchSizes)
	}
}
