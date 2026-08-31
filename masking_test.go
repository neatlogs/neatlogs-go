package neatlogs

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestMaskRunsAfterNormalizationAndCoversFullSpanSurface(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	mask := func(_ context.Context, span SpanData) (*SpanData, error) {
		if span.Name != "secret-name" {
			return &span, nil
		}
		if _, ok := attributeValue(span.Attributes, "neatlogs.llm.model_name"); !ok {
			t.Fatal("mask did not receive normalized neatlogs.* attributes")
		}
		span.Name = "masked-name"
		span.Attributes = replaceAttribute(span.Attributes, "neatlogs.input.value", "***")
		span.Events[0].Attributes = replaceAttribute(span.Events[0].Attributes, "secret", "***")
		span.Resource = append(span.Resource, attribute.String("neatlogs.masked", "true"))
		span.Links[0].Attributes = replaceAttribute(span.Links[0].Attributes, "secret", "***")
		return &span, nil
	}
	shutdown, err := Init(ctx, Config{WorkflowName: "mask-test", Mask: mask}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	linked := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
	ctx, span, end := StartSpan(ctx, "secret-name", "llm",
		attribute.String("gen_ai.request.model", "gemini"),
		attribute.String("input.value", "secret-input"),
	)
	_ = ctx
	span.AddEvent("chunk", trace.WithAttributes(attribute.String("secret", "event-secret")))
	span.AddLink(trace.Link{SpanContext: linked, Attributes: []attribute.KeyValue{attribute.String("secret", "link-secret")}})
	end()
	if err := Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	var got spanStub
	found := false
	for _, exported := range sink.GetSpans() {
		if exported.Name == "masked-name" {
			got, found = exported, true
			break
		}
	}
	if !found {
		t.Fatal("masked span was not exported")
	}
	assertAttribute(t, got.Attributes, "neatlogs.input.value", "***")
	assertAttribute(t, got.Events[0].Attributes, "secret", "***")
	assertAttribute(t, got.Resource.Attributes(), "neatlogs.masked", "true")
	assertAttribute(t, got.Links[0].Attributes, "secret", "***")
}

func TestMaskFailureAndNilResultFailClosed(t *testing.T) {
	for name, mask := range map[string]MaskFunc{
		"error": func(context.Context, SpanData) (*SpanData, error) {
			return nil, errors.New("secret must not escape")
		},
		"drop": func(context.Context, SpanData) (*SpanData, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			sink := tracetest.NewInMemoryExporter()
			shutdown, err := Init(ctx, Config{WorkflowName: "mask-test", Mask: mask}, WithExporter(sink))
			if err != nil {
				t.Fatal(err)
			}
			_, span, end := StartSpan(ctx, "secret", "tool", attribute.String("input.value", "must-not-export"))
			_ = span
			end()
			if err := Flush(ctx); err != nil {
				t.Fatal(err)
			}
			if got := len(sink.GetSpans()); got != 0 {
				t.Fatalf("mask failure exported %d spans, want 0", got)
			}
			if err := shutdown(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func replaceAttribute(values []attribute.KeyValue, key, value string) []attribute.KeyValue {
	for i := range values {
		if string(values[i].Key) == key {
			values[i] = attribute.String(key, value)
			return values
		}
	}
	return append(values, attribute.String(key, value))
}

func attributeValue(values []attribute.KeyValue, key string) (string, bool) {
	for _, value := range values {
		if string(value.Key) == key {
			return value.Value.AsString(), true
		}
	}
	return "", false
}

func assertAttribute(t *testing.T, values []attribute.KeyValue, key, want string) {
	t.Helper()
	got, ok := attributeValue(values, key)
	if !ok || got != want {
		t.Fatalf("attribute %s = %q, %v; want %q", key, got, ok, want)
	}
}
