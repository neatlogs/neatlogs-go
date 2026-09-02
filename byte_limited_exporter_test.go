package neatlogs

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
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

type planningProbeSpan struct {
	sdktrace.ReadOnlySpan
	cancel   context.CancelFunc
	accessed *atomic.Bool
}

func (s planningProbeSpan) InstrumentationScope() instrumentation.Scope {
	if s.accessed != nil {
		s.accessed.Store(true)
	}
	if s.cancel != nil {
		s.cancel()
	}
	return s.ReadOnlySpan.InstrumentationScope()
}

func TestByteLimitedExporterStopsPlanningWhenContextExpires(t *testing.T) {
	spans := sizedTestSpans(2, 2048)
	ctx, cancel := context.WithCancel(context.Background())
	var secondAccessed atomic.Bool
	probes := []sdktrace.ReadOnlySpan{
		planningProbeSpan{ReadOnlySpan: spans[0], cancel: cancel},
		planningProbeSpan{ReadOnlySpan: spans[1], accessed: &secondAccessed},
	}
	diagnostics := &deliveryDiagnostics{}
	sink := &batchRecordingExporter{}
	exporter, err := newByteLimitedExporter(sink, defaultMaxExportBytes, diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	err = exporter.ExportSpans(ctx, probes)
	var failure *uploadFailure
	if !errors.As(err, &failure) || failure.reasonCode != "cancelled" || failure.retryable {
		t.Fatalf("planning cancellation = %#v, want non-retryable cancelled failure", err)
	}
	if secondAccessed.Load() {
		t.Fatal("planner accessed a later span after cancellation")
	}
	if len(sink.batches) != 0 {
		t.Fatal("cancelled planning reached the ordinary exporter")
	}
	if got := diagnostics.snapshot().SpanExportFailures; got != 2 {
		t.Fatalf("export failures = %d, want all 2 unsent spans", got)
	}
}

type cancellingUploadAuthority struct {
	cancel context.CancelFunc
}

func (a cancellingUploadAuthority) Upload(context.Context, uploadPayload) (uploadReceipt, error) {
	a.cancel()
	return uploadReceipt{}, errors.New("upload cancelled")
}

func TestByteLimitedExporterStopsActionsWhenOverflowCancelsContext(t *testing.T) {
	small := sizedTestSpans(1, 16)[0]
	overflow := sizedTestSpans(1, 4096)[0]
	spans := []sdktrace.ReadOnlySpan{small, overflow, small}
	ctx, cancel := context.WithCancel(context.Background())
	diagnostics := &deliveryDiagnostics{}
	sink := &batchRecordingExporter{}
	exporter, err := newByteLimitedExporter(sink, encodedSpanUpperBound(small), diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	exporter.uploads = cancellingUploadAuthority{cancel: cancel}
	err = exporter.ExportSpans(ctx, spans)
	var failure *uploadFailure
	if !errors.As(err, &failure) || failure.reasonCode != "cancelled" || failure.retryable {
		t.Fatalf("action cancellation = %#v, want non-retryable cancelled failure", err)
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != 1 {
		t.Fatalf("ordinary batches = %d, want only the pre-cancellation batch", len(sink.batches))
	}
	if got := diagnostics.snapshot().SpanExportFailures; got != 2 {
		t.Fatalf("export failures = %d, want overflow plus unattempted span (2)", got)
	}
}

func TestNormalizingExporterStopsCloningWhenContextExpires(t *testing.T) {
	spans := sizedTestSpans(2, 2048)
	ctx, cancel := context.WithCancel(context.Background())
	var secondAccessed atomic.Bool
	probes := []sdktrace.ReadOnlySpan{
		planningProbeSpan{ReadOnlySpan: spans[0], cancel: cancel},
		planningProbeSpan{ReadOnlySpan: spans[1], accessed: &secondAccessed},
	}
	store := internalmedia.NewStore(internalmedia.UploadLimit, internalmedia.MaxPendingItems)
	defer store.Close()
	internalmedia.RegisterSpan(spans[1].SpanContext(), store)
	if _, reason := internalmedia.Stage(spans[1].SpanContext(), []byte("pending media"), "image/png"); reason != "" {
		t.Fatal(reason)
	}
	diagnostics := &deliveryDiagnostics{}
	sink := &batchRecordingExporter{}
	byteLimited, err := newByteLimitedExporter(sink, defaultMaxExportBytes, diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	exporter := &normalizingExporter{next: byteLimited, mapper: attrs.Default(), delivery: diagnostics}
	err = exporter.ExportSpans(ctx, probes)
	var failure *uploadFailure
	if !errors.As(err, &failure) || failure.reasonCode != "cancelled" || failure.retryable {
		t.Fatalf("normalization cancellation = %#v, want non-retryable cancelled failure", err)
	}
	if secondAccessed.Load() {
		t.Fatal("normalizer accessed a later span after cancellation")
	}
	if len(sink.batches) != 0 {
		t.Fatal("cancelled normalization reached the ordinary exporter")
	}
	if got := diagnostics.snapshot().SpanExportFailures; got != 2 {
		t.Fatalf("export failures = %d, want all 2 unsent spans", got)
	}
	if items, retained := store.Snapshot(); items != 0 || retained != 0 {
		t.Fatalf("cancelled normalization retained media = %d items/%d bytes", items, retained)
	}
}
