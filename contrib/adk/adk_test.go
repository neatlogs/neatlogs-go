package adk

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"sync"
	"testing"

	neatlogs "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

type responseItem struct {
	response *model.LLMResponse
	err      error
}

type fakeModel struct {
	name      string
	responses []responseItem
	seenCtx   context.Context
}

type scriptedModel struct {
	mu        sync.Mutex
	responses []*model.LLMResponse
	next      int
}

func (m *scriptedModel) Name() string { return "gemini-scripted" }

func (m *scriptedModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	index := m.next
	m.next++
	m.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {
		if index >= len(m.responses) {
			yield(&model.LLMResponse{Content: genai.NewContentFromText("done", genai.RoleModel)}, nil)
			return
		}
		yield(m.responses[index], nil)
	}
}

func (m *fakeModel) Name() string { return m.name }

func (m *fakeModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.seenCtx = ctx
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, item := range m.responses {
			if !yield(item.response, item.err) {
				return
			}
		}
	}
}

func TestWrapModelExportsPrivateADKSpanWithContentUsageAndTools(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := neatlogs.NewClient(ctx, neatlogs.Config{WorkflowName: "adk-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)

	inner := &fakeModel{name: "gemini-default", responses: []responseItem{
		{response: &model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "Hel"}}},
			Partial: true,
		}},
		{response: &model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{Text: "lo"},
				{FunctionCall: &genai.FunctionCall{ID: "call-1", Name: "weather", Args: map[string]any{"city": "Paris"}}},
			}},
			ModelVersion: "gemini-2.5-flash-001",
			FinishReason: genai.FinishReasonStop,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     11,
				CandidatesTokenCount: 7,
				TotalTokenCount:      18,
			},
		}},
	}}
	temperature := float32(0.25)
	req := &model.LLMRequest{
		Model:    "gemini-2.5-flash",
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{Temperature: &temperature, MaxOutputTokens: 64},
	}

	for _, callErr := range WrapModel(inner).GenerateContent(client.Context(ctx), req, true) {
		if callErr != nil {
			t.Fatal(callErr)
		}
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	span := findSpan(t, sink, "google.adk.generate_content")
	assertStringAttribute(t, span.Attributes, "neatlogs.span.kind", "llm")
	assertStringAttribute(t, span.Attributes, "neatlogs.llm.provider", "google")
	assertStringAttribute(t, span.Attributes, "neatlogs.llm.model_name", "gemini-2.5-flash-001")
	assertStringAttribute(t, span.Attributes, "neatlogs.llm.input_messages.0.content", "hello")
	assertStringAttribute(t, span.Attributes, "neatlogs.llm.output_messages.0.content", "Hello")
	assertStringAttribute(t, span.Attributes, "neatlogs.llm.tool_calls.0.name", "weather")
	assertIntAttribute(t, span.Attributes, "neatlogs.llm.token_count.total", 18)
	if inner.seenCtx == nil {
		t.Fatal("wrapped model did not receive the Neatlogs-derived context")
	}
	if span.Parent.IsValid() {
		root := findSpan(t, sink, "adk-test")
		if span.Parent.SpanID() != root.SpanContext.SpanID() {
			t.Fatalf("ADK LLM parent = %s, want auto-root %s", span.Parent.SpanID(), root.SpanContext.SpanID())
		}
	} else {
		t.Fatal("ADK LLM span was exported without its workflow root")
	}
}

func TestWrapModelRecordsErrorsAndClosesSpan(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := neatlogs.NewClient(ctx, neatlogs.Config{WorkflowName: "adk-error"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)

	wantErr := errors.New("provider unavailable")
	inner := &fakeModel{name: "gemini", responses: []responseItem{{err: wantErr}}}
	for _, gotErr := range WrapModel(inner).GenerateContent(client.Context(ctx), &model.LLMRequest{}, false) {
		if !errors.Is(gotErr, wantErr) {
			t.Fatalf("error = %v, want %v", gotErr, wantErr)
		}
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	span := findSpan(t, sink, "google.adk.generate_content")
	if span.Status.Code != codes.Error {
		t.Fatalf("status = %s, want Error", span.Status.Code)
	}
}

type weatherArgs struct {
	City string `json:"city"`
}

type weatherResult struct {
	Forecast string `json:"forecast"`
}

func TestInstrumentConfigAndRunCaptureWorkflowModelAndToolSpans(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := neatlogs.NewClient(ctx, neatlogs.Config{WorkflowName: "adk-runner"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)

	weather, err := functiontool.New(
		functiontool.Config{Name: "weather", Description: "Return a forecast."},
		func(_ tool.Context, args weatherArgs) (weatherResult, error) {
			return weatherResult{Forecast: "sunny in " + args.City}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedModel{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
			ID: "tool-1", Name: "weather", Args: map[string]any{"city": "Paris"},
		}}}}, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5, CandidatesTokenCount: 2, TotalTokenCount: 7}},
		{Content: genai.NewContentFromText("It is sunny in Paris.", genai.RoleModel), UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 8, CandidatesTokenCount: 6, TotalTokenCount: 14}},
	}}
	adkAgent, err := llmagent.New(InstrumentConfig(llmagent.Config{
		Name:  "weather_agent",
		Model: provider,
		Tools: []tool.Tool{weather},
	}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{
		AppName: "adk-runner", Agent: adkAgent,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, callErr := range Run(
		client.Context(ctx), r, "user-1", "session-1",
		genai.NewContentFromText("weather in Paris", genai.RoleUser),
		agent.RunConfig{},
	) {
		if callErr != nil {
			t.Fatal(callErr)
		}
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	root := findSpan(t, sink, "google.adk.run")
	assertStringAttribute(t, root.Attributes, "neatlogs.span.kind", "workflow")
	assertStringAttribute(t, root.Attributes, "neatlogs.trace.output", `"It is sunny in Paris."`)
	toolSpan := findSpan(t, sink, "weather")
	assertStringAttribute(t, toolSpan.Attributes, "neatlogs.span.kind", "tool")
	assertStringAttribute(t, toolSpan.Attributes, "neatlogs.tool.input", `{"city":"Paris"}`)
	assertStringAttribute(t, toolSpan.Attributes, "neatlogs.tool.output", `{"forecast":"sunny in Paris"}`)
	if toolSpan.SpanContext.TraceID() != root.SpanContext.TraceID() {
		t.Fatalf("tool trace %s differs from workflow trace %s", toolSpan.SpanContext.TraceID(), root.SpanContext.TraceID())
	}
	llmSpans := findSpans(sink, "google.adk.generate_content")
	if len(llmSpans) != 2 {
		t.Fatalf("LLM span count = %d, want 2", len(llmSpans))
	}
}

func TestA2ATransportInjectsPrivateContextWithoutMutatingRequest(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := neatlogs.NewClient(ctx, neatlogs.Config{WorkflowName: "a2a-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)

	traceCtx, _, end := neatlogs.Trace(client.Context(ctx), "a2a-root")
	defer end()
	req, err := http.NewRequestWithContext(traceCtx, http.MethodPost, "https://agent.example/invoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := injectingTransport{base: roundTripFunc(func(clone *http.Request) (*http.Response, error) {
		if clone.Header.Get("traceparent") == "" {
			t.Fatal("private traceparent was not injected")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("traceparent"); got != "" {
		t.Fatalf("original request header was mutated: %q", got)
	}
}

func TestActiveCallRegistryBoundsAbandonedCallbacks(t *testing.T) {
	var registry activeCallRegistry
	span := trace.SpanFromContext(context.Background())
	ended := 0
	for i := 0; i <= maxActiveCalls; i++ {
		evicted := registry.put(fmt.Sprintf("call-%d", i), activeCall{
			span: span,
			end:  func() { ended++ },
		})
		endEvicted(evicted, "test eviction")
	}
	if ended != 1 {
		t.Fatalf("ended calls = %d, want 1", ended)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func findSpan(t *testing.T, sink *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, span := range sink.GetSpans() {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("span %q not found in %#v", name, sink.GetSpans())
	return tracetest.SpanStub{}
}

func findSpans(sink *tracetest.InMemoryExporter, name string) []tracetest.SpanStub {
	var matches []tracetest.SpanStub
	for _, span := range sink.GetSpans() {
		if span.Name == name {
			matches = append(matches, span)
		}
	}
	return matches
}

func assertStringAttribute(t *testing.T, attributes []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, item := range attributes {
		if string(item.Key) == key {
			if got := item.Value.AsString(); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("attribute %q not found", key)
}

func assertIntAttribute(t *testing.T, attributes []attribute.KeyValue, key string, want int64) {
	t.Helper()
	for _, item := range attributes {
		if string(item.Key) == key {
			if got := item.Value.AsInt64(); got != want {
				t.Fatalf("%s = %d, want %d", key, got, want)
			}
			return
		}
	}
	t.Fatalf("attribute %q not found", key)
}
