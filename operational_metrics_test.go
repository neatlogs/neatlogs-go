package neatlogs

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func exportMetricStub(t *testing.T, stub tracetest.SpanStub) []attribute.KeyValue {
	t.Helper()
	sink := &batchRecordingExporter{}
	exporter := &normalizingExporter{next: sink, mapper: attrs.Default()}
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{stub.Snapshot()}); err != nil {
		t.Fatal(err)
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != 1 {
		t.Fatalf("expected one exported span, got %v", sink.batches)
	}
	return sink.batches[0][0].Attributes()
}

func metricValue(t *testing.T, attributes []attribute.KeyValue, key string) (float64, bool) {
	t.Helper()
	for _, kv := range attributes {
		if string(kv.Key) == key {
			return kv.Value.AsFloat64(), true
		}
	}
	return 0, false
}

func metricSpanContext() trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2},
	})
}

func TestOperationalMetricsAddDurationToEverySpan(t *testing.T) {
	start := time.Unix(1700000000, 0)
	stub := tracetest.SpanStub{
		Name: "tool", SpanContext: metricSpanContext(),
		StartTime: start, EndTime: start.Add(50 * time.Millisecond),
	}
	attributes := exportMetricStub(t, stub)
	duration, ok := metricValue(t, attributes, durationMsAttrKey)
	if !ok {
		t.Fatalf("missing %s", durationMsAttrKey)
	}
	if duration != 50 {
		t.Fatalf("duration_ms = %v, want 50", duration)
	}
	if _, ok := metricValue(t, attributes, ttftMsAttrKey); ok {
		t.Fatalf("unexpected %s without chunk events", ttftMsAttrKey)
	}
}

func TestOperationalMetricsComputeTtftAndStreamingTimeFromChunks(t *testing.T) {
	start := time.Unix(1700000000, 0)
	events := []sdktrace.Event{
		{Name: "gen_ai.content.chunk", Time: start.Add(12 * time.Millisecond)},
		{Name: "gen_ai.content.chunk", Time: start.Add(30 * time.Millisecond)},
		{Name: "other.event", Time: start.Add(40 * time.Millisecond)},
	}
	stub := tracetest.SpanStub{
		Name: "llm", SpanContext: metricSpanContext(),
		StartTime: start, EndTime: start.Add(80 * time.Millisecond), Events: events,
	}
	attributes := exportMetricStub(t, stub)
	ttft, ok := metricValue(t, attributes, ttftMsAttrKey)
	if !ok || ttft != 12 {
		t.Fatalf("ttft_ms = %v, %v; want 12, true", ttft, ok)
	}
	stg, ok := metricValue(t, attributes, stgMsAttrKey)
	if !ok || stg != 18 {
		t.Fatalf("streaming_time_to_generate_ms = %v, %v; want 18, true", stg, ok)
	}
}

func TestOperationalMetricsKeepLiveTtftAuthoritative(t *testing.T) {
	start := time.Unix(1700000000, 0)
	events := []sdktrace.Event{
		{Name: "gen_ai.content.chunk", Time: start.Add(12 * time.Millisecond)},
		{Name: "gen_ai.content.chunk", Time: start.Add(30 * time.Millisecond)},
	}
	stub := tracetest.SpanStub{
		Name: "llm", SpanContext: metricSpanContext(),
		StartTime: start, EndTime: start.Add(80 * time.Millisecond), Events: events,
		Attributes: []attribute.KeyValue{attribute.Float64(ttftMsAttrKey, 7.5)},
	}
	attributes := exportMetricStub(t, stub)
	ttft, _ := metricValue(t, attributes, ttftMsAttrKey)
	if ttft != 7.5 {
		t.Fatalf("ttft_ms = %v, want live value 7.5", ttft)
	}
	if _, ok := metricValue(t, attributes, stgMsAttrKey); ok {
		t.Fatal("stg must not be recomputed when live ttft exists, matching py/ts")
	}
}

func TestOperationalMetricsSingleChunkOmitsStreamingTime(t *testing.T) {
	start := time.Unix(1700000000, 0)
	events := []sdktrace.Event{
		{Name: "gen_ai.content.chunk", Time: start.Add(12 * time.Millisecond)},
	}
	stub := tracetest.SpanStub{
		Name: "llm", SpanContext: metricSpanContext(),
		StartTime: start, EndTime: start.Add(80 * time.Millisecond), Events: events,
	}
	attributes := exportMetricStub(t, stub)
	if _, ok := metricValue(t, attributes, ttftMsAttrKey); !ok {
		t.Fatal("missing ttft for single chunk")
	}
	if _, ok := metricValue(t, attributes, stgMsAttrKey); ok {
		t.Fatal("stg must be omitted for a single chunk, matching py/ts")
	}
}
