package neatlogs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

func TestTypedMediaUploadsAfterCanonicalizationAndMaskingWithoutExposingBytes(t *testing.T) {
	original := []byte("must-not-cross-the-public-mask-boundary")
	prefix := "neatlogs.llm.input_messages.0.media.0."
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
	})
	store := internalmedia.NewStore(internalmedia.UploadLimit, internalmedia.MaxPendingItems)
	defer store.Close()
	internalmedia.RegisterSpan(spanContext, store)
	pending, reason := internalmedia.Stage(spanContext, original, "image/png")
	if reason != "" {
		t.Fatal(reason)
	}
	stub := tracetest.SpanStub{
		Name: "media", SpanContext: spanContext,
		Attributes: []attribute.KeyValue{
			attribute.String(prefix+"id", "nl_media_original"),
			attribute.String(prefix+"type", "image"),
			attribute.String(prefix+"source", "inline"),
			attribute.String(prefix+"mime_type", "image/png"),
			attribute.Int(prefix+"byte_length", len(original)),
			attribute.String(prefix+"sha256", pending.SHA256),
			attribute.String(prefix+"purpose", "input"),
			attribute.String(prefix+"state", "pending-upload"),
			attribute.String(prefix+"upload_token", pending.Token),
		},
	}
	authority := &recordingUploadAuthority{receipt: readyMediaUploadReceipt(pending)}
	sink := &batchRecordingExporter{}
	byteLimited, err := newByteLimitedExporter(sink, defaultMaxExportBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	exporter := &normalizingExporter{
		next: byteLimited, mapper: attrs.Default(), uploads: authority,
		mask: func(_ context.Context, data SpanData) (*SpanData, error) {
			if stringAttribute(data.Attributes, prefix+"state") != "pending-upload" ||
				stringAttribute(data.Attributes, prefix+"upload_token") != pending.Token {
				return nil, errors.New("mask did not receive canonical media reference")
			}
			for _, value := range data.Attributes {
				if value.Value.Type() == attribute.BYTESLICE || bytes.Contains([]byte(value.Value.Emit()), original) {
					return nil, errors.New("raw media crossed the public mask boundary")
				}
			}
			return &data, nil
		},
	}
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		t.Fatal(err)
	}
	if len(authority.payloads) != 1 || !bytes.Equal(authority.payloads[0].Content, original) {
		t.Fatalf("authority payload = %#v", authority.payloads)
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != 1 {
		t.Fatalf("ordinary batches = %d", len(sink.batches))
	}
	exported := sink.batches[0][0].Attributes()
	if got := stringAttribute(exported, prefix+"id"); got != authority.receipt.Reference.ID {
		t.Fatalf("uploaded reference id = %q", got)
	}
	if got := stringAttribute(exported, prefix+"source"); got != "uploaded" {
		t.Fatalf("uploaded source = %q", got)
	}
	if got := stringAttribute(exported, prefix+"state"); got != "available" {
		t.Fatalf("uploaded state = %q", got)
	}
	if strings.Contains(fmt.Sprint(exported), internalmedia.UploadTokenPrefix) {
		t.Fatal("private media token reached ordinary telemetry")
	}
	if items, retained := store.Snapshot(); items != 0 || retained != 0 {
		t.Fatalf("pending store = %d items/%d bytes", items, retained)
	}
}

func TestTypedMediaFailureExportsMetadataButReturnsFailure(t *testing.T) {
	content := []byte("large-private-media")
	prefix := "neatlogs.llm.input_messages.0.media.0."
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{2}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
	})
	store := internalmedia.NewStore(internalmedia.UploadLimit, internalmedia.MaxPendingItems)
	defer store.Close()
	internalmedia.RegisterSpan(spanContext, store)
	pending, reason := internalmedia.Stage(spanContext, content, "image/png")
	if reason != "" {
		t.Fatal(reason)
	}
	stub := tracetest.SpanStub{Name: "media", SpanContext: spanContext, Attributes: []attribute.KeyValue{
		attribute.String(prefix+"id", "nl_media_"+pending.SHA256[:24]),
		attribute.String(prefix+"mime_type", pending.MIMEType),
		attribute.String(prefix+"sha256", pending.SHA256),
		attribute.String(prefix+"state", "pending-upload"),
		attribute.String(prefix+"upload_token", pending.Token),
	}}
	diagnostics := &deliveryDiagnostics{}
	sink := &batchRecordingExporter{}
	exporter := &normalizingExporter{
		next: sink, mapper: attrs.Default(), delivery: diagnostics,
		uploads: &recordingUploadAuthority{err: newUploadFailure("complete", "MEDIA_SIGNATURE_MISMATCH", false)},
	}
	err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()})
	if err == nil {
		t.Fatal("typed media failure reported exporter success")
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != 1 {
		t.Fatalf("failure metadata was not delegated: %#v", sink.batches)
	}
	exported := sink.batches[0][0].Attributes()
	if got := stringAttribute(exported, prefix+"state"); got != "failed" {
		t.Fatalf("state = %q", got)
	}
	if got := stringAttribute(exported, prefix+"safe_preview"); !strings.Contains(got, "MEDIA_SIGNATURE_MISMATCH") {
		t.Fatalf("safe preview = %q", got)
	}
	if strings.Contains(fmt.Sprint(exported), internalmedia.UploadTokenPrefix) {
		t.Fatal("upload token reached failed telemetry")
	}
	snapshot := diagnostics.snapshot()
	if snapshot.TypedMediaUploadFailures != 1 || snapshot.SpanExportFailures != 1 || snapshot.LastUploadFailure == nil {
		t.Fatalf("diagnostics = %#v", snapshot)
	}
}

func TestMaskCanRemoveCanonicalMediaReferenceBeforeUpload(t *testing.T) {
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{4}, SpanID: trace.SpanID{4}, TraceFlags: trace.FlagsSampled,
	})
	store := internalmedia.NewStore(internalmedia.UploadLimit, internalmedia.MaxPendingItems)
	defer store.Close()
	internalmedia.RegisterSpan(spanContext, store)
	pending, reason := internalmedia.Stage(spanContext, []byte("removed by mask"), "image/png")
	if reason != "" {
		t.Fatal(reason)
	}
	prefix := "neatlogs.llm.input_messages.0.media.0."
	stub := tracetest.SpanStub{Name: "masked-media", SpanContext: spanContext, Attributes: []attribute.KeyValue{
		attribute.String(prefix+"mime_type", pending.MIMEType),
		attribute.String(prefix+"sha256", pending.SHA256),
		attribute.String(prefix+"state", "pending-upload"),
		attribute.String(prefix+"upload_token", pending.Token),
	}}
	authority := &recordingUploadAuthority{receipt: readyMediaUploadReceipt(pending)}
	sink := &batchRecordingExporter{}
	exporter := &normalizingExporter{
		next: sink, mapper: attrs.Default(), uploads: authority,
		mask: func(_ context.Context, data SpanData) (*SpanData, error) {
			data.Attributes = nil
			return &data, nil
		},
	}
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		t.Fatal(err)
	}
	if len(authority.payloads) != 0 {
		t.Fatalf("removed reference triggered %d uploads", len(authority.payloads))
	}
	if items, retained := store.Snapshot(); items != 0 || retained != 0 {
		t.Fatalf("removed reference retained store = %d/%d", items, retained)
	}
}

func TestMaskCannotTurnStagedMediaIntoAvailableWithoutUploading(t *testing.T) {
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{5}, SpanID: trace.SpanID{5}, TraceFlags: trace.FlagsSampled,
	})
	store := internalmedia.NewStore(internalmedia.UploadLimit, internalmedia.MaxPendingItems)
	defer store.Close()
	internalmedia.RegisterSpan(spanContext, store)
	pending, reason := internalmedia.Stage(spanContext, []byte("must remain out of band"), "image/png")
	if reason != "" {
		t.Fatal(reason)
	}
	prefix := "neatlogs.llm.input_messages.0.media.0."
	stub := tracetest.SpanStub{Name: "masked-state", SpanContext: spanContext, Attributes: []attribute.KeyValue{
		attribute.String(prefix+"id", "nl_media_"+pending.SHA256[:24]),
		attribute.String(prefix+"mime_type", pending.MIMEType),
		attribute.String(prefix+"sha256", pending.SHA256),
		attribute.String(prefix+"state", "pending-upload"),
		attribute.String(prefix+"upload_token", pending.Token),
	}}
	authority := &recordingUploadAuthority{receipt: readyMediaUploadReceipt(pending)}
	sink := &batchRecordingExporter{}
	exporter := &normalizingExporter{
		next: sink, mapper: attrs.Default(), uploads: authority,
		mask: func(_ context.Context, data SpanData) (*SpanData, error) {
			data.Attributes = setStringAttribute(data.Attributes, prefix+"state", "available")
			return &data, nil
		},
	}
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()}); err == nil {
		t.Fatal("masked pending state reported exporter success")
	}
	if len(authority.payloads) != 0 {
		t.Fatalf("masked pending state triggered %d uploads", len(authority.payloads))
	}
	exported := sink.batches[0][0].Attributes()
	if got := stringAttribute(exported, prefix+"state"); got != "failed" {
		t.Fatalf("state = %q", got)
	}
	if got := stringAttribute(exported, prefix+"safe_preview"); got != "upload failed: masked_pending_state" {
		t.Fatalf("safe preview = %q", got)
	}
	if strings.Contains(fmt.Sprint(exported), internalmedia.UploadTokenPrefix) {
		t.Fatal("upload token reached failed telemetry")
	}
}

func readyMediaUploadReceipt(payload internalmedia.PendingPayload) uploadReceipt {
	id := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	return uploadReceipt{
		UploadID: id, State: "ready",
		Reference: uploadReference{
			ID: id, Purpose: uploadPurposeTypedMedia, SHA256: payload.SHA256,
			ByteLength: int64(payload.ByteLength), MIMEType: payload.MIMEType,
			ContentEncoding: uploadEncodingIdentity, State: "ready",
		},
	}
}

type deadlineUploadAuthority struct{ calls atomic.Int32 }

func (a *deadlineUploadAuthority) Upload(ctx context.Context, _ uploadPayload) (uploadReceipt, error) {
	a.calls.Add(1)
	<-ctx.Done()
	return uploadReceipt{}, newUploadFailure("upload", contextReason(ctx), contextRetryable(ctx))
}

func TestTypedMediaAggregateDeadlineStopsFurtherUploadAttempts(t *testing.T) {
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{3}, SpanID: trace.SpanID{3}, TraceFlags: trace.FlagsSampled,
	})
	store := internalmedia.NewStore(internalmedia.UploadLimit, internalmedia.MaxPendingItems)
	defer store.Close()
	internalmedia.RegisterSpan(spanContext, store)
	stub := spanStub{Name: "deadline", SpanContext: spanContext}
	for index, content := range [][]byte{[]byte("first payload"), []byte("second payload")} {
		pending, reason := internalmedia.Stage(spanContext, content, "image/png")
		if reason != "" {
			t.Fatal(reason)
		}
		prefix := fmt.Sprintf("neatlogs.llm.input_messages.0.media.%d.", index)
		stub.Attributes = append(stub.Attributes,
			attribute.String(prefix+"id", "nl_media_"+pending.SHA256[:24]),
			attribute.String(prefix+"mime_type", pending.MIMEType),
			attribute.String(prefix+"sha256", pending.SHA256),
			attribute.String(prefix+"state", "pending-upload"),
			attribute.String(prefix+"upload_token", pending.Token),
		)
	}
	authority := &deadlineUploadAuthority{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if uploadTypedMedia(ctx, &stub, authority, nil) {
		t.Fatal("expired media resolution reported success")
	}
	if calls := authority.calls.Load(); calls != 1 {
		t.Fatalf("upload calls after aggregate deadline = %d, want 1", calls)
	}
	if items, retained := store.Snapshot(); items != 0 || retained != 0 {
		t.Fatalf("deadline did not release store = %d/%d", items, retained)
	}
}

func TestClientShutdownReleasesPendingMediaStore(t *testing.T) {
	t.Setenv("NEATLOGS_UPLOADS_ENABLED", "")
	client, err := NewClient(context.Background(), Config{
		APIKey: "project-key", EnableUploads: true,
	}, WithExporter(&batchRecordingExporter{}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithClient(context.Background(), client)
	_, span, _ := StartProviderSpan(ctx, "active-media", "LLM")
	if _, reason := internalmedia.Stage(span.SpanContext(), []byte("retained until shutdown"), "image/png"); reason != "" {
		t.Fatal(reason)
	}
	if items, retained := client.runtime.media.Snapshot(); items != 1 || retained == 0 {
		t.Fatalf("pre-shutdown store = %d/%d", items, retained)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if items, retained := client.runtime.media.Snapshot(); items != 0 || retained != 0 {
		t.Fatalf("post-shutdown store = %d/%d", items, retained)
	}
	if _, reason := internalmedia.Stage(span.SpanContext(), []byte("must not restage"), "image/png"); reason != uploadUnavailableReason {
		t.Fatalf("post-shutdown stage reason = %q", reason)
	}
}
