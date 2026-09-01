package neatlogs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

func TestTypedMediaUploadsAfterCanonicalizationAndMasking(t *testing.T) {
	original := []byte("must-not-cross-upload-authority")
	masked := []byte("masked-media-content")
	digest := sha256.Sum256(original)
	prefix := "neatlogs.llm.input_messages.0.media.0."
	stub := tracetest.SpanStub{
		Name: "media", SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
		}),
		Attributes: []attribute.KeyValue{
			attribute.String(prefix+"id", "nl_media_original"),
			attribute.String(prefix+"type", "image"),
			attribute.String(prefix+"source", "inline"),
			attribute.String(prefix+"mime_type", "image/png"),
			attribute.Int(prefix+"byte_length", len(original)),
			attribute.String(prefix+"sha256", fmt.Sprintf("%x", digest)),
			attribute.String(prefix+"purpose", "input"),
			attribute.String(prefix+"state", "pending-upload"),
			internalmedia.PayloadAttribute(prefix, original),
		},
	}
	authority := &recordingUploadAuthority{receipt: readyUploadReceipt()}
	sink := &batchRecordingExporter{}
	byteLimited, err := newByteLimitedExporter(sink, defaultMaxExportBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	exporter := &normalizingExporter{
		next: byteLimited, mapper: attrs.Default(), uploads: authority,
		mask: func(_ context.Context, data SpanData) (*SpanData, error) {
			if stringAttribute(data.Attributes, prefix+"state") != "pending-upload" {
				t.Fatal("mask did not receive canonical media reference")
			}
			for index := range data.Attributes {
				if strings.HasPrefix(string(data.Attributes[index].Key), internalmedia.PayloadPrefix) {
					data.Attributes[index] = attribute.ByteSlice(string(data.Attributes[index].Key), masked)
				}
			}
			return &data, nil
		},
	}
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		t.Fatal(err)
	}
	if len(authority.payloads) != 1 || !bytes.Equal(authority.payloads[0].Content, masked) {
		t.Fatalf("authority payload = %#v", authority.payloads)
	}
	if bytes.Contains(authority.payloads[0].Content, original) {
		t.Fatal("unmasked media crossed upload authority")
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != 1 {
		t.Fatalf("ordinary batches = %d", len(sink.batches))
	}
	exported := sink.batches[0][0].Attributes()
	if got := stringAttribute(exported, prefix+"id"); got != readyUploadReceipt().Reference.ID {
		t.Fatalf("uploaded reference id = %q", got)
	}
	if got := stringAttribute(exported, prefix+"source"); got != "uploaded" {
		t.Fatalf("uploaded source = %q", got)
	}
	if got := stringAttribute(exported, prefix+"state"); got != "available" {
		t.Fatalf("uploaded state = %q", got)
	}
	for _, value := range exported {
		if strings.HasPrefix(string(value.Key), internalmedia.PayloadPrefix) {
			t.Fatal("private media payload reached ordinary telemetry")
		}
	}
}

func TestTypedMediaDisabledExportsExplicitFailureWithoutRawBytes(t *testing.T) {
	content := []byte("large-private-media")
	digest := sha256.Sum256(content)
	prefix := "neatlogs.llm.input_messages.0.media.0."
	stub := spanStub{Attributes: []attribute.KeyValue{
		attribute.String(prefix+"mime_type", "image/png"),
		attribute.String(prefix+"sha256", fmt.Sprintf("%x", digest)),
		attribute.String(prefix+"state", "pending-upload"),
		internalmedia.PayloadAttribute(prefix, content),
	}}
	diagnostics := &deliveryDiagnostics{}
	uploadTypedMedia(context.Background(), &stub, nil, diagnostics)
	if got := stringAttribute(stub.Attributes, prefix+"state"); got != "failed" {
		t.Fatalf("state = %q", got)
	}
	if got := stringAttribute(stub.Attributes, prefix+"safe_preview"); !strings.Contains(got, uploadUnavailableReason) {
		t.Fatalf("safe preview = %q", got)
	}
	if snapshot := diagnostics.snapshot(); snapshot.TypedMediaUploadFailures != 1 || snapshot.LastUploadFailure == nil {
		t.Fatalf("diagnostics = %#v", snapshot)
	}
	for _, value := range stub.Attributes {
		if value.Value.Type() == attribute.BYTESLICE {
			t.Fatal("raw media remained after disabled upload")
		}
	}
}
