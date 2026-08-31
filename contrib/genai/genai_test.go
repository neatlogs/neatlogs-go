package genai

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	google "google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func TestResponseAccumulatorPreservesChoicesAndEveryStreamChunk(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "genai-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "stream", attrs.KindLLM)
	acc := newResponseAccumulator()
	recordStreamChunk(span, acc, &google.GenerateContentResponse{Candidates: []*google.Candidate{
		{Index: 0, Content: &google.Content{Role: "model", Parts: []*google.Part{{Text: "A"}}}},
		{Index: 1, Content: &google.Content{Role: "model", Parts: []*google.Part{
			{Text: "X"},
			{FunctionCall: &google.FunctionCall{ID: "call-1", Name: "weather", Args: map[string]any{"city": "Paris"}}},
		}}},
	}})
	recordStreamChunk(span, acc, &google.GenerateContentResponse{Candidates: []*google.Candidate{
		{Index: 0, FinishReason: google.FinishReasonStop, Content: &google.Content{Role: "model", Parts: []*google.Part{{Text: "B"}}}},
		{Index: 1, FinishReason: google.FinishReasonMaxTokens, Content: &google.Content{Role: "model", Parts: []*google.Part{
			{Text: "Y"},
			{FunctionCall: &google.FunctionCall{ID: "call-1", Name: "weather", Args: map[string]any{"unit": "C"}}},
		}}},
	}})
	finalizeStream(span, acc, true, false)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	var got *tracetest.SpanStub
	for i := range sink.GetSpans() {
		candidate := sink.GetSpans()[i]
		if candidate.Name == "stream" {
			got = &candidate
			break
		}
	}
	if got == nil {
		t.Fatal("stream span not exported")
	}
	assertStringAttribute(t, got.Attributes, attrs.LLMOutputMessagePrefix+"0.content", "AB")
	assertStringAttribute(t, got.Attributes, attrs.LLMOutputMessagePrefix+"1.content", "XY")
	assertStringAttribute(t, got.Attributes, attrs.LLMChoicePrefix+"0.finish_reason", string(google.FinishReasonStop))
	assertStringAttribute(t, got.Attributes, attrs.LLMChoicePrefix+"1.finish_reason", string(google.FinishReasonMaxTokens))
	assertStringAttribute(t, got.Attributes, attrs.LLMToolCallPrefix+"0.id", "call-1")
	assertIntAttribute(t, got.Attributes, attrs.LLMToolCallPrefix+"0.choice_index", 1)
	assertStringAttribute(t, got.Attributes, attrs.LLMToolCallPrefix+"0.arguments", `{"city":"Paris","unit":"C"}`)
	if _, ok := findAttribute(got.Attributes, attrs.LLMToolCallPrefix+"1.name"); ok {
		t.Fatal("streamed fragments of one tool call were emitted as duplicate calls")
	}
	assertIntAttribute(t, got.Attributes, attrs.StreamChunkCount, 2)
	assertBoolAttribute(t, got.Attributes, attrs.StreamCancelled, true)
	if got.Status.Code != codes.Unset {
		t.Fatalf("cancelled stream status = %v, want UNSET", got.Status.Code)
	}
	if len(got.Events) != 2 {
		t.Fatalf("stream events = %d, want one per chunk", len(got.Events))
	}
	for index, event := range got.Events {
		if event.Name != attrs.StreamChunkEvent {
			t.Fatalf("event %d name = %q", index, event.Name)
		}
		assertIntAttribute(t, event.Attributes, attrs.StreamChunkIndex, index)
		if value, ok := findAttribute(event.Attributes, attrs.StreamChunkValue); !ok || value.AsString() == "" {
			t.Fatalf("event %d did not preserve serialized chunk", index)
		}
	}
}

func findAttribute(values []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, value := range values {
		if string(value.Key) == key {
			return value.Value, true
		}
	}
	return attribute.Value{}, false
}

func assertStringAttribute(t *testing.T, values []attribute.KeyValue, key, want string) {
	t.Helper()
	value, ok := findAttribute(values, key)
	if !ok || value.AsString() != want {
		t.Fatalf("attribute %s = %q, %v; want %q", key, value.AsString(), ok, want)
	}
}

func assertIntAttribute(t *testing.T, values []attribute.KeyValue, key string, want int) {
	t.Helper()
	value, ok := findAttribute(values, key)
	if !ok || value.AsInt64() != int64(want) {
		t.Fatalf("attribute %s = %d, %v; want %d", key, value.AsInt64(), ok, want)
	}
}

func assertBoolAttribute(t *testing.T, values []attribute.KeyValue, key string, want bool) {
	t.Helper()
	value, ok := findAttribute(values, key)
	if !ok || value.AsBool() != want {
		t.Fatalf("attribute %s = %v, %v; want %v", key, value.AsBool(), ok, want)
	}
}
