package adk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
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
	"google.golang.org/adk/model/gemini"
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

	// ADK's SSE mode yields each delta as a partial response and then one
	// aggregated response that repeats the whole text.
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
			Partial: true,
		}},
		{response: &model.LLMResponse{
			Content: &genai.Content{Role: "model", Parts: []*genai.Part{
				{Text: "Hello"},
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
	assertAttributeAbsent(t, span.Attributes, "neatlogs.llm.tool_calls.1.name")
	assertIntAttribute(t, span.Attributes, "neatlogs.llm.token_count.total", 18)
	if span.Status.Code != codes.Ok {
		t.Fatalf("completed stream status = %s, want Ok", span.Status.Code)
	}
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

func textResponse(text string, partial bool) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
		Partial: partial,
	}
}

func newTestClient(t *testing.T, workflow string) (*neatlogs.Client, *tracetest.InMemoryExporter) {
	t.Helper()
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := neatlogs.NewClient(ctx, neatlogs.Config{WorkflowName: workflow}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(ctx) })
	return client, sink
}

func assertBoolAttribute(t *testing.T, attributes []attribute.KeyValue, key string, want bool) {
	t.Helper()
	for _, item := range attributes {
		if string(item.Key) == key {
			if got := item.Value.AsBool(); got != want {
				t.Fatalf("%s = %v, want %v", key, got, want)
			}
			return
		}
	}
	t.Fatalf("attribute %q not found", key)
}

func assertAttributeAbsent(t *testing.T, attributes []attribute.KeyValue, key string) {
	t.Helper()
	for _, item := range attributes {
		if string(item.Key) == key {
			t.Fatalf("%s = %v, want it absent", key, item.Value.AsInterface())
		}
	}
}

func assertExceptionRecorded(t *testing.T, span tracetest.SpanStub) {
	t.Helper()
	for _, event := range span.Events {
		if event.Name == "exception" {
			return
		}
	}
	t.Fatalf("no exception event recorded on %q: %v", span.Name, span.Events)
}

func TestWrapModelStreamOutcomes(t *testing.T) {
	providerErr := errors.New("connection reset by peer")
	deadlineErr := fmt.Errorf("read response body: %w", context.DeadlineExceeded)
	for _, test := range []struct {
		name      string
		responses []responseItem
		stream    bool
		// stopAfter stops the consumer after that many items; zero drains.
		stopAfter      int
		wantStatus     codes.Code
		wantMessage    string
		wantOutput     string
		wantCancelled  bool
		wantIncomplete bool
	}{
		{
			// The ADK flow stops iterating as soon as the model yields an error.
			name:       "error_after_partial_text",
			responses:  []responseItem{{response: textResponse("Hel", true)}, {err: providerErr}},
			stream:     true,
			stopAfter:  2,
			wantStatus: codes.Error, wantMessage: providerErr.Error(), wantOutput: "Hel",
		},
		{
			name:       "deadline_exceeded",
			responses:  []responseItem{{response: textResponse("Hel", true)}, {err: deadlineErr}},
			stream:     true,
			wantStatus: codes.Error, wantMessage: deadlineErr.Error(), wantOutput: "Hel",
		},
		{
			name:       "caller_cancels_context",
			responses:  []responseItem{{response: textResponse("Hel", true)}, {err: context.Canceled}},
			stream:     true,
			wantStatus: codes.Unset, wantOutput: "Hel", wantCancelled: true,
		},
		{
			name:       "caller_stops_early",
			responses:  []responseItem{{response: textResponse("Hel", true)}, {response: textResponse("lo", true)}},
			stream:     true,
			stopAfter:  1,
			wantStatus: codes.Unset, wantOutput: "Hel", wantCancelled: true,
		},
		{
			name:       "stream_ends_without_finish_reason",
			responses:  []responseItem{{response: textResponse("Hel", true)}, {response: textResponse("lo", true)}},
			stream:     true,
			wantStatus: codes.Unset, wantOutput: "Hello", wantIncomplete: true,
		},
		{
			// Only a stream can end early; a single response has no finish
			// reason with models that do not report one.
			name:       "single_response_without_finish_reason",
			responses:  []responseItem{{response: textResponse("Hello", false)}},
			wantStatus: codes.Ok, wantOutput: "Hello",
		},
		{
			name:       "response_error_fields",
			responses:  []responseItem{{response: &model.LLMResponse{ErrorCode: "SAFETY", ErrorMessage: "response blocked"}}},
			wantStatus: codes.Error, wantMessage: "SAFETY: response blocked",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client, sink := newTestClient(t, "adk-stream-outcomes")
			inner := &fakeModel{name: "gemini", responses: test.responses}
			seen := 0
			for range WrapModel(inner).GenerateContent(client.Context(ctx), &model.LLMRequest{}, test.stream) {
				if seen++; seen == test.stopAfter {
					break
				}
			}
			if err := client.Flush(ctx); err != nil {
				t.Fatal(err)
			}

			span := findSpan(t, sink, "google.adk.generate_content")
			if span.Status.Code != test.wantStatus {
				t.Fatalf("status = %s, want %s", span.Status.Code, test.wantStatus)
			}
			if span.Status.Description != test.wantMessage {
				t.Fatalf("status message = %q, want %q", span.Status.Description, test.wantMessage)
			}
			if test.wantStatus == codes.Error {
				assertExceptionRecorded(t, span)
			}
			if test.wantOutput != "" {
				assertStringAttribute(t, span.Attributes, "neatlogs.llm.output_messages.0.content", test.wantOutput)
			}
			if test.wantCancelled {
				assertBoolAttribute(t, span.Attributes, "neatlogs.stream.cancelled", true)
			} else {
				assertAttributeAbsent(t, span.Attributes, "neatlogs.stream.cancelled")
			}
			if test.wantIncomplete {
				assertBoolAttribute(t, span.Attributes, "neatlogs.stream.incomplete", true)
			} else {
				assertAttributeAbsent(t, span.Attributes, "neatlogs.stream.incomplete")
			}
		})
	}
}

// This test drives ADK's own Gemini model against a local server, so it
// covers what ADK's SSE mode actually hands the wrapper: partial deltas, then
// one aggregate, and a stream that the pinned provider SDK may end silently.
func TestWrapModelOverGeminiSSEStream(t *testing.T) {
	const first = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}` + "\n\n"
	const last = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}` + "\n\n"
	for _, test := range []struct {
		name string
		// cut drops the connection before the last chunk.
		cut        bool
		wantOutput string
	}{
		{name: "complete", wantOutput: "Hello"},
		{name: "cut_by_the_server", cut: true, wantOutput: "Hel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, first)
				w.(http.Flusher).Flush()
				if test.cut {
					panic(http.ErrAbortHandler)
				}
				_, _ = io.WriteString(w, last)
			}))
			defer server.Close()
			llm, err := gemini.NewModel(ctx, "gemini-test", &genai.ClientConfig{
				APIKey:      "test-google-key",
				Backend:     genai.BackendGeminiAPI,
				HTTPOptions: genai.HTTPOptions{BaseURL: server.URL},
			})
			if err != nil {
				t.Fatal(err)
			}
			client, sink := newTestClient(t, "adk-gemini-sse")
			req := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)}}
			for range WrapModel(llm).GenerateContent(client.Context(ctx), req, true) {
			}
			if err := client.Flush(ctx); err != nil {
				t.Fatal(err)
			}

			span := findSpan(t, sink, "google.adk.generate_content")
			assertStringAttribute(t, span.Attributes, "neatlogs.llm.output_messages.0.content", test.wantOutput)
			assertAttributeAbsent(t, span.Attributes, "neatlogs.stream.cancelled")
			if !test.cut {
				if span.Status.Code != codes.Ok {
					t.Fatalf("completed stream status = %s, want Ok", span.Status.Code)
				}
				assertIntAttribute(t, span.Attributes, "neatlogs.llm.token_count.total", 5)
				return
			}
			// Provider SDK releases differ: older ones end the stream silently,
			// newer ones yield the read error. Neither may be a success.
			switch span.Status.Code {
			case codes.Error:
				assertExceptionRecorded(t, span)
			case codes.Unset:
				assertBoolAttribute(t, span.Attributes, "neatlogs.stream.incomplete", true)
			default:
				t.Fatalf("cut stream status = %s, want Error or Unset", span.Status.Code)
			}
		})
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

func TestToolCallArgumentsJSONNormalizesNilToEmptyObject(t *testing.T) {
	if got := toolCallArgumentsJSON(nil); got != `{}` {
		t.Fatalf("toolCallArgumentsJSON(nil) = %s, want {}", got)
	}
	if got := toolCallArgumentsJSON(map[string]any{}); got != `{}` {
		t.Fatalf("toolCallArgumentsJSON(empty) = %s, want {}", got)
	}
	if got := toolCallArgumentsJSON(map[string]any{"city": "Paris"}); got != `{"city":"Paris"}` {
		t.Fatalf("toolCallArgumentsJSON(populated) = %s", got)
	}
}
