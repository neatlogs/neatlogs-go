package neatlogs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

const maskSentinel = "NEATLOGS_MASK_SENTINEL"

func TestMaskRunsOnDetachedNormalizedSnapshot(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	health := &exportHealthState{}
	exporter := &normalizingExporter{
		next: sink, mapper: attrs.Default(), maskTimeout: time.Second,
		masks: newSpanMaskRegistry(), health: health,
		globalMask: func(_ context.Context, span *MaskableSpan) error {
			span.Name = "masked.name"
			redactAttributes(span.Attributes)
			redactAttributes(span.Events[0].Attributes)
			redactAttributes(span.Links[0].Attributes)
			span.Status.Description = "[REDACTED]"
			span.Resource = resource.NewSchemaless(attribute.String("resource.secret", "[REDACTED]"))
			span.Scope.Attributes = attribute.NewSet(attribute.String("scope.secret", "[REDACTED]"))
			return nil
		},
	}
	traceID, _ := trace.TraceIDFromHex("11111111111111111111111111111111")
	spanID, _ := trace.SpanIDFromHex("2222222222222222")
	originalSlice := []string{maskSentinel, "safe"}
	source := tracetest.SpanStub{
		Name:        "original.name",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}),
		Attributes: []attribute.KeyValue{
			attribute.String(attrs.Input, maskSentinel),
			attribute.StringSlice("slice.secret", originalSlice),
		},
		Events:   []sdktrace.Event{{Name: "secret.event", Attributes: []attribute.KeyValue{attribute.String("event.secret", maskSentinel)}}},
		Links:    []sdktrace.Link{{SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}), Attributes: []attribute.KeyValue{attribute.String("link.secret", maskSentinel)}}},
		Status:   sdktrace.Status{Code: codes.Error, Description: maskSentinel},
		Resource: resource.NewSchemaless(attribute.String("resource.secret", maskSentinel)),
		InstrumentationScope: instrumentation.Scope{
			Name: "secret-scope", Attributes: attribute.NewSet(attribute.String("scope.secret", maskSentinel)),
		},
	}
	if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{source.Snapshot()}); err != nil {
		t.Fatal(err)
	}

	if source.Name != "original.name" || originalSlice[0] != maskSentinel || attributeString(source.Attributes, attrs.Input) != maskSentinel {
		t.Fatal("mask mutated application/source data")
	}
	if value, ok := source.InstrumentationScope.Attributes.Value("scope.secret"); !ok || value.AsString() != maskSentinel {
		t.Fatal("mask mutated source instrumentation scope")
	}
	exported := sink.GetSpans()
	if len(exported) != 1 || exported[0].Name != "masked.name" {
		t.Fatalf("exported spans = %#v", exported)
	}
	if containsSentinel(exported[0]) {
		t.Fatal("raw sentinel reached Neatlogs transport")
	}
}

func TestPerSpanMaskTakesPrecedenceOverGlobalMask(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	var globalCalls, localCalls atomic.Int64
	globalMask := func(_ context.Context, span *MaskableSpan) error {
		globalCalls.Add(1)
		setAttribute(span.Attributes, attrs.Input, "global")
		return nil
	}
	localMask := func(_ context.Context, span *MaskableSpan) error {
		localCalls.Add(1)
		setAttribute(span.Attributes, attrs.Input, "local")
		return nil
	}
	client, err := NewClient(ctx, Config{WorkflowName: "precedence", Mask: globalMask}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	maskedCtx := WithMask(WithClient(ctx, client), localMask)
	_, span, end := StartSpan(maskedCtx, "precedence.root", "WORKFLOW", attribute.String(attrs.Input, maskSentinel))
	if !span.IsRecording() {
		t.Fatal("span is not recording")
	}
	end()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	root := byName(sink, "precedence.root")
	if got := attributeString(root.Attributes, attrs.Input); got != "local" {
		t.Fatalf("masked input = %q, want local", got)
	}
	if localCalls.Load() != 1 {
		t.Fatalf("local mask calls = %d, want 1", localCalls.Load())
	}
	if globalCalls.Load() != 1 { // completion marker only
		t.Fatalf("global mask calls = %d, want completion marker only", globalCalls.Load())
	}
}

func TestMaskFailuresDropClosedAndFlushReportsHealth(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	mask := func(ctx context.Context, span *MaskableSpan) error {
		switch span.Name {
		case "panic":
			panic(maskSentinel)
		case "error":
			return errors.New(maskSentinel)
		case "timeout":
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	client, err := NewClient(ctx, Config{WorkflowName: "mask-failures", Mask: mask, MaskTimeout: 10 * time.Millisecond}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	bound, _, rootEnd := Trace(WithClient(ctx, client), "safe-root")
	for _, name := range []string{"panic", "error", "timeout"} {
		_, _, end := StartSpan(bound, name, "TOOL", attribute.String(attrs.Input, maskSentinel))
		end()
	}
	rootEnd()
	err = client.Flush(ctx)
	if !errors.Is(err, ErrMaskingFailed) {
		t.Fatalf("Flush error = %v", err)
	}
	if strings.Contains(err.Error(), maskSentinel) {
		t.Fatal("Flush exposed callback secret")
	}
	if got := len(sink.GetSpans()); got != 2 { // safe root + completion marker
		t.Fatalf("exported spans = %d, want safe root and marker", got)
	}
	for _, span := range sink.GetSpans() {
		if containsSentinel(span) {
			t.Fatal("failed mask leaked sentinel through another span")
		}
	}
	health := client.ExportHealth()
	if health.DroppedSpans != 3 || health.MaskFailures != 3 || health.ExportFailures != 0 {
		t.Fatalf("health = %+v", health)
	}
}

func TestInvalidMaskOutputDropsClosed(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := NewClient(ctx, Config{
		WorkflowName: "invalid-mask-output",
		Mask: func(_ context.Context, span *MaskableSpan) error {
			span.Attributes = append(span.Attributes, attribute.KeyValue{})
			return nil
		},
	}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	remoteTraceID, _ := trace.TraceIDFromHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	remoteSpanID, _ := trace.SpanIDFromHex("bbbbbbbbbbbbbbbb")
	remote := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: remoteTraceID, SpanID: remoteSpanID, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	remoteCtx := withPrivateTraceContext(WithClient(ctx, client), remote, nil)
	_, _, end := StartSpan(remoteCtx, "invalid.mask", "TOOL", attribute.String(attrs.Input, maskSentinel))
	end()
	err = client.Flush(ctx)
	if !errors.Is(err, ErrMaskingFailed) || len(sink.GetSpans()) != 0 {
		t.Fatalf("Flush error=%v spans=%d", err, len(sink.GetSpans()))
	}
}

func TestMaskingIsConcurrencySafe(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	health := &exportHealthState{}
	var calls atomic.Int64
	exporter := &normalizingExporter{
		next: sink, mapper: attrs.Default(), maskTimeout: time.Second,
		masks: newSpanMaskRegistry(), health: health,
		globalMask: func(_ context.Context, span *MaskableSpan) error {
			calls.Add(1)
			redactAttributes(span.Attributes)
			return nil
		},
	}
	const count = 128
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			traceID := trace.TraceID{byte(i + 1)}
			spanID := trace.SpanID{byte(i + 1)}
			stub := tracetest.SpanStub{
				Name:        "concurrent",
				SpanContext: trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}),
				Attributes:  []attribute.KeyValue{attribute.String(attrs.Input, maskSentinel)},
			}
			if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
				t.Errorf("ExportSpans: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != count || len(sink.GetSpans()) != count {
		t.Fatalf("calls=%d spans=%d, want %d", calls.Load(), len(sink.GetSpans()), count)
	}
	for _, span := range sink.GetSpans() {
		if containsSentinel(span) {
			t.Fatal("concurrent export leaked sentinel")
		}
	}
}

func TestExporterFailureIsSanitizedAndReportedByFlush(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(ctx, Config{WorkflowName: "export-failure"}, WithExporter(&secretFailingExporter{}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, end := Trace(WithClient(ctx, client), "export.failure")
	end()
	err = client.Flush(ctx)
	if !errors.Is(err, ErrExportFailed) {
		t.Fatalf("Flush error = %v", err)
	}
	if strings.Contains(err.Error(), maskSentinel) {
		t.Fatal("Flush exposed exporter secret")
	}
	if health := client.ExportHealth(); health.ExportFailures == 0 {
		t.Fatalf("health = %+v", health)
	}
	if err := client.Shutdown(ctx); !errors.Is(err, ErrExportFailed) {
		t.Fatalf("Shutdown error = %v", err)
	}
}

func TestNegativeMaskTimeoutIsRejected(t *testing.T) {
	_, err := NewClient(context.Background(), Config{DisableExport: true, MaskTimeout: -time.Nanosecond})
	if err == nil || !strings.Contains(err.Error(), "mask timeout must not be negative") {
		t.Fatalf("NewClient error = %v", err)
	}
}

type secretFailingExporter struct{}

func (*secretFailingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New(maskSentinel)
}
func (*secretFailingExporter) Shutdown(context.Context) error { return errors.New(maskSentinel) }

func redactAttributes(values []attribute.KeyValue) {
	for i := range values {
		switch values[i].Value.Type() {
		case attribute.STRING:
			if strings.Contains(values[i].Value.AsString(), maskSentinel) {
				values[i] = attribute.String(string(values[i].Key), "[REDACTED]")
			}
		case attribute.STRINGSLICE:
			items := append([]string(nil), values[i].Value.AsStringSlice()...)
			for j := range items {
				if strings.Contains(items[j], maskSentinel) {
					items[j] = "[REDACTED]"
				}
			}
			values[i] = attribute.StringSlice(string(values[i].Key), items)
		}
	}
}

func setAttribute(values []attribute.KeyValue, key, value string) {
	for i := range values {
		if string(values[i].Key) == key {
			values[i] = attribute.String(key, value)
			return
		}
	}
}

func attributeString(values []attribute.KeyValue, key string) string {
	for _, value := range values {
		if string(value.Key) == key {
			return value.Value.AsString()
		}
	}
	return ""
}

func containsSentinel(span tracetest.SpanStub) bool {
	if strings.Contains(span.Name, maskSentinel) || strings.Contains(span.Status.Description, maskSentinel) {
		return true
	}
	resourceAttributes := []attribute.KeyValue(nil)
	if span.Resource != nil {
		resourceAttributes = span.Resource.Attributes()
	}
	for _, values := range [][]attribute.KeyValue{span.Attributes, resourceAttributes, span.InstrumentationScope.Attributes.ToSlice()} {
		for _, value := range values {
			if strings.Contains(value.Value.Emit(), maskSentinel) {
				return true
			}
		}
	}
	for _, event := range span.Events {
		for _, value := range event.Attributes {
			if strings.Contains(value.Value.Emit(), maskSentinel) {
				return true
			}
		}
	}
	for _, link := range span.Links {
		for _, value := range link.Attributes {
			if strings.Contains(value.Value.Emit(), maskSentinel) {
				return true
			}
		}
	}
	return false
}
