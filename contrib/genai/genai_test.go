package genai

import (
	"context"
	"crypto/sha256"
	"fmt"
	"iter"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	google "google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func TestResponseAccumulatorPreservesChoicesAndSemanticStreamEvidence(t *testing.T) {
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
		if value, ok := findAttribute(event.Attributes, attrs.StreamChunkSummary); !ok || value.AsString() == "" {
			t.Fatalf("event %d did not preserve its semantic summary", index)
		} else if value.AsString() == "A" || value.AsString() == "X" {
			t.Fatalf("event %d contains raw chunk content", index)
		}
	}
}

func TestTypedMediaPreservesInlineDigestAndFileReference(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "media-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	raw := []byte("full-image-bytes")
	content := &google.Content{Role: "user", Parts: []*google.Part{
		{Text: "inspect both"},
		{InlineData: &google.Blob{Data: raw, MIMEType: "image/png"}},
		{FileData: &google.FileData{FileURI: "gs://bucket/report.pdf", MIMEType: "application/pdf"}},
	}}
	_, span, end := neatlogs.StartProviderSpan(ctx, "media", attrs.KindLLM)
	setInputMessages(span, []*google.Content{content}, nil)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	var got *tracetest.SpanStub
	for _, candidate := range sink.GetSpans() {
		if candidate.Name == "media" {
			copy := candidate
			got = &copy
		}
	}
	if got == nil {
		t.Fatal("media span not exported")
	}
	digest := sha256.Sum256(raw)
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.0.type", "image")
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.0.sha256", fmt.Sprintf("%x", digest))
	assertIntAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.0.byte_length", len(raw))
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.1.type", "document")
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.1.reference", "gs://bucket/report.pdf")
}

func TestMissingToolCallIDGetsStableExplicitlyMarkedIdentity(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "tool-id-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "tool-id", attrs.KindLLM)
	acc := newResponseAccumulator()
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			Name: "lookup", Args: map[string]any{"query": "weather"},
		}}}},
	}}})
	acc.apply(span)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "tool-id" {
			continue
		}
		id, ok := findAttribute(candidate.Attributes, attrs.LLMToolCallPrefix+"0.id")
		if !ok || !strings.HasPrefix(id.AsString(), "nl_") {
			t.Fatalf("synthetic tool ID = %q, %v", id.AsString(), ok)
		}
		assertBoolAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.id_synthetic", true)
		assertIntAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.tool_call_index", 0)
		return
	}
	t.Fatal("tool span not exported")
}

func TestMissingToolCallIDsNeverMergeByPositionAlone(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "tool-separation-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "tool-separation", attrs.KindLLM)
	acc := newResponseAccumulator()
	for _, name := range []string{"first", "second"} {
		acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
			Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
				Name: name, Args: map[string]any{"value": name},
			}}}},
		}}})
	}
	acc.apply(span)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "tool-separation" {
			continue
		}
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.name", "first")
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"1.name", "second")
		assertBoolAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.id_synthetic", true)
		assertBoolAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"1.id_synthetic", true)
		return
	}
	t.Fatal("tool separation span not exported")
}

func TestToolDefinitionsPreserveTypeAndCompleteConfiguration(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "tool-definition-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "tool-definitions", attrs.KindLLM)
	setToolDefinitions(span, &google.GenerateContentConfig{Tools: []*google.Tool{{
		FunctionDeclarations: []*google.FunctionDeclaration{{Name: "lookup", Description: "Find a record"}},
		GoogleSearch:         &google.GoogleSearch{},
	}}})
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "tool-definitions" {
			continue
		}
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"0.type", "function")
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"0.name", "lookup")
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"1.type", "google_search")
		if definition, ok := findAttribute(candidate.Attributes, attrs.LLMToolPrefix+"1.definition"); !ok || definition.AsString() == "" {
			t.Fatal("google_search definition was not preserved")
		}
		return
	}
	t.Fatal("tool definition span not exported")
}

func TestGenerateContentStreamIsLazyUntilFirstConsumption(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "lazy-stream"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	streamCalls := 0
	models := &GenAIModels{
		provider: geminiProvider,
		system:   geminiSystem,
		stream: func(context.Context, string, []*google.Content, *google.GenerateContentConfig) iter.Seq2[*google.GenerateContentResponse, error] {
			streamCalls++
			return func(yield func(*google.GenerateContentResponse, error) bool) {
				yield(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
					Index: 0, Content: &google.Content{Parts: []*google.Part{{Text: "done"}}},
				}}}, nil)
			}
		},
	}

	sequence := models.GenerateContentStream(ctx, "gemini", nil, nil)
	if streamCalls != 0 {
		t.Fatalf("underlying stream started %d times before consumption", streamCalls)
	}
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.GetSpans()); got != 0 {
		t.Fatalf("never-consumed stream exported %d spans", got)
	}

	for _, streamErr := range sequence {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}
	if streamCalls != 1 {
		t.Fatalf("underlying stream started %d times after consumption", streamCalls)
	}
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, span := range sink.GetSpans() {
		if span.Name == "google_genai.models.generate_content" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("consumed stream did not export its LLM span")
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
