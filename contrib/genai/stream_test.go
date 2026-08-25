package genai

import (
	"context"
	"errors"
	"iter"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	googlegenai "google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func TestStreamStartsOnFirstConsumptionAndCompletesOnce(t *testing.T) {
	ctx := context.Background()
	client, sink := streamClient(t, ctx)
	models := fakeStreamModels(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) iter.Seq2[*googlegenai.GenerateContentResponse, error] {
		return streamResponses(response("hello ", ""), response("world", googlegenai.FinishReasonStop))
	})
	seq := models.GenerateContentStream(neatlogs.WithClient(ctx, client), "gemini-test", nil, nil)
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.GetSpans()) != 0 {
		t.Fatal("stream span started before first consumption")
	}
	for range seq {
	}
	flushOK(t, ctx, client)
	llm := spanByName(sink.GetSpans(), "google_genai.models.generate_content")
	assertStream(t, llm, "complete", "hello world", 2, false)
	assertSpanCount(t, sink.GetSpans(), "google_genai.models.generate_content", 1)
}

func TestStreamEarlyStopIsCancelledWithBoundedPartialOutput(t *testing.T) {
	ctx := context.Background()
	client, sink := streamClient(t, ctx)
	large := strings.Repeat("界", maxStreamOutputRunes+100)
	models := fakeStreamModels(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) iter.Seq2[*googlegenai.GenerateContentResponse, error] {
		return streamResponses(response(large, ""), response("unconsumed", googlegenai.FinishReasonStop))
	})
	seq := models.GenerateContentStream(neatlogs.WithClient(ctx, client), "gemini-test", nil, nil)
	seq(func(*googlegenai.GenerateContentResponse, error) bool { return false })
	err := client.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	llm := spanByName(sink.GetSpans(), "google_genai.models.generate_content")
	output := attr(llm, attrs.LLMStreamPartialOutput)
	if utf8.RuneCountInString(output) != maxStreamOutputRunes {
		t.Fatalf("partial output runes = %d", utf8.RuneCountInString(output))
	}
	assertStream(t, llm, "consumer_cancelled", output, 1, true)
}

func TestStreamProviderErrorAndContextCancellation(t *testing.T) {
	for _, test := range []struct {
		name      string
		stream    iter.Seq2[*googlegenai.GenerateContentResponse, error]
		configure func(context.CancelFunc) func(*googlegenai.GenerateContentResponse, error) bool
		state     string
	}{
		{
			name: "provider error", state: "provider_error",
			stream: func(yield func(*googlegenai.GenerateContentResponse, error) bool) {
				yield(nil, errors.New("provider failed"))
			},
			configure: func(context.CancelFunc) func(*googlegenai.GenerateContentResponse, error) bool {
				return func(*googlegenai.GenerateContentResponse, error) bool { return true }
			},
		},
		{
			name: "context cancelled", state: "consumer_cancelled", stream: streamResponses(response("partial", ""), response("late", "")),
			configure: func(cancel context.CancelFunc) func(*googlegenai.GenerateContentResponse, error) bool {
				return func(*googlegenai.GenerateContentResponse, error) bool { cancel(); return true }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := context.Background()
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			client, sink := streamClient(t, base)
			models := fakeStreamModels(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) iter.Seq2[*googlegenai.GenerateContentResponse, error] {
				return test.stream
			})
			seq := models.GenerateContentStream(neatlogs.WithClient(ctx, client), "gemini-test", nil, nil)
			seq(test.configure(cancel))
			flushOK(t, base, client)
			llm := spanByName(sink.GetSpans(), "google_genai.models.generate_content")
			if got := attr(llm, attrs.LLMStreamCompletionState); got != test.state {
				t.Fatalf("completion state = %q, want %q", got, test.state)
			}
		})
	}
}

func TestStreamPanicsFinalizeExactlyOnceAndRepanic(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider bool
		state    string
	}{
		{name: "provider panic", provider: true, state: "provider_error"},
		{name: "consumer panic", state: "consumer_cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			client, sink := streamClient(t, ctx)
			models := fakeStreamModels(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) iter.Seq2[*googlegenai.GenerateContentResponse, error] {
				if test.provider {
					return func(func(*googlegenai.GenerateContentResponse, error) bool) { panic("provider") }
				}
				return streamResponses(response("chunk", ""))
			})
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("stream did not repanic")
					}
				}()
				seq := models.GenerateContentStream(neatlogs.WithClient(ctx, client), "gemini-test", nil, nil)
				seq(func(*googlegenai.GenerateContentResponse, error) bool {
					if !test.provider {
						panic("consumer")
					}
					return true
				})
			}()
			flushOK(t, ctx, client)
			llm := spanByName(sink.GetSpans(), "google_genai.models.generate_content")
			if got := attr(llm, attrs.LLMStreamCompletionState); got != test.state {
				t.Fatalf("completion state = %q, want %q", got, test.state)
			}
			assertSpanCount(t, sink.GetSpans(), "google_genai.models.generate_content", 1)
		})
	}
}

func TestFlushLeavesActiveStreamOpenUntilContextCancellation(t *testing.T) {
	base := context.Background()
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	client, sink := streamClient(t, base)
	models := fakeStreamModels(func(streamCtx context.Context, _ string, _ []*googlegenai.Content, _ *googlegenai.GenerateContentConfig) iter.Seq2[*googlegenai.GenerateContentResponse, error] {
		return func(yield func(*googlegenai.GenerateContentResponse, error) bool) {
			if !yield(response("partial", ""), nil) {
				return
			}
			<-streamCtx.Done()
			yield(nil, streamCtx.Err())
		}
	})
	first := make(chan struct{})
	done := make(chan struct{})
	seq := models.GenerateContentStream(neatlogs.WithClient(ctx, client), "gemini-test", nil, nil)
	go func() {
		defer close(done)
		seq(func(*googlegenai.GenerateContentResponse, error) bool {
			select {
			case <-first:
			default:
				close(first)
			}
			return true
		})
	}()
	<-first
	if err := client.Flush(base); err != nil {
		t.Fatal(err)
	}
	if len(sink.GetSpans()) != 0 {
		t.Fatal("Flush ended or exported an active stream")
	}
	cancel()
	<-done
	flushOK(t, base, client)
	llm := spanByName(sink.GetSpans(), "google_genai.models.generate_content")
	if got := attr(llm, attrs.LLMStreamCompletionState); got != "consumer_cancelled" {
		t.Fatalf("completion state = %q", got)
	}
	assertSpanCount(t, sink.GetSpans(), "google_genai.models.generate_content", 1)
}

func TestShutdownFinalizesBlockedActiveStreamExactlyOnce(t *testing.T) {
	ctx := context.Background()
	sink := &retainingSpanExporter{}
	client, err := neatlogs.NewClient(ctx, neatlogs.Config{WorkflowName: "active-shutdown"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	first := make(chan struct{})
	done := make(chan struct{})
	models := fakeStreamModels(func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) iter.Seq2[*googlegenai.GenerateContentResponse, error] {
		return func(yield func(*googlegenai.GenerateContentResponse, error) bool) {
			if !yield(response("partial", ""), nil) {
				return
			}
			<-release
		}
	})
	seq := models.GenerateContentStream(neatlogs.WithClient(ctx, client), "gemini-test", nil, nil)
	go func() {
		defer close(done)
		seq(func(*googlegenai.GenerateContentResponse, error) bool {
			select {
			case <-first:
			default:
				close(first)
			}
			return true
		})
	}()
	<-first
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	spans := sink.spans()
	assertSpanCount(t, spans, "google_genai.models.generate_content", 1)
	llm := spanByName(spans, "google_genai.models.generate_content")
	if got := attr(llm, "neatlogs.trace.interrupted"); got != "true" {
		t.Fatalf("interrupted = %q", got)
	}
	if got := attr(llm, attrs.LLMStreamCompletionState); got != "consumer_cancelled" {
		t.Fatalf("completion state = %q", got)
	}
	if got := attr(llm, attrs.LLMStreamPartialOutput); got != "partial" {
		t.Fatalf("partial output = %q", got)
	}
}

func fakeStreamModels(stream func(context.Context, string, []*googlegenai.Content, *googlegenai.GenerateContentConfig) iter.Seq2[*googlegenai.GenerateContentResponse, error]) *GenAIModels {
	return &GenAIModels{provider: geminiProvider, system: geminiSystem, stream: stream}
}

func streamResponses(responses ...*googlegenai.GenerateContentResponse) iter.Seq2[*googlegenai.GenerateContentResponse, error] {
	return func(yield func(*googlegenai.GenerateContentResponse, error) bool) {
		for _, item := range responses {
			if !yield(item, nil) {
				return
			}
		}
	}
}

func response(text string, finish googlegenai.FinishReason) *googlegenai.GenerateContentResponse {
	return &googlegenai.GenerateContentResponse{Candidates: []*googlegenai.Candidate{{
		Content: &googlegenai.Content{Role: "model", Parts: []*googlegenai.Part{{Text: text}}}, FinishReason: finish,
	}}}
}

func streamClient(t *testing.T, ctx context.Context) (*neatlogs.Client, *tracetest.InMemoryExporter) {
	t.Helper()
	sink := tracetest.NewInMemoryExporter()
	client, err := neatlogs.NewClient(ctx, neatlogs.Config{WorkflowName: "stream-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(ctx) })
	return client, sink
}

func flushOK(t *testing.T, ctx context.Context, client *neatlogs.Client) {
	t.Helper()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func spanByName(spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	for _, span := range spans {
		if span.Name == name {
			return span
		}
	}
	return tracetest.SpanStub{}
}

func attr(span tracetest.SpanStub, key string) string {
	for _, value := range span.Attributes {
		if string(value.Key) == key {
			return value.Value.Emit()
		}
	}
	return ""
}

func assertStream(t *testing.T, span tracetest.SpanStub, state, output string, chunks int, truncated bool) {
	t.Helper()
	if got := attr(span, attrs.LLMStreamCompletionState); got != state {
		t.Errorf("completion state = %q, want %q", got, state)
	}
	if got := attr(span, attrs.LLMStreamPartialOutput); got != output {
		t.Errorf("partial output = %q, want %q", got, output)
	}
	if got := attr(span, attrs.LLMStreamChunkCount); got != strconv.Itoa(chunks) {
		t.Errorf("chunk count = %q, want %d", got, chunks)
	}
	if got := attr(span, attrs.LLMStreamOutputTruncated); got != map[bool]string{true: "true", false: "false"}[truncated] {
		t.Errorf("truncated = %q, want %v", got, truncated)
	}
}

func assertSpanCount(t *testing.T, spans tracetest.SpanStubs, name string, want int) {
	t.Helper()
	got := 0
	for _, span := range spans {
		if span.Name == name {
			got++
		}
	}
	if got != want {
		t.Fatalf("%s spans = %d, want %d", name, got, want)
	}
}

type retainingSpanExporter struct {
	mu   sync.Mutex
	seen tracetest.SpanStubs
}

func (e *retainingSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, span := range spans {
		e.seen = append(e.seen, tracetest.SpanStubFromReadOnlySpan(span))
	}
	return nil
}
func (*retainingSpanExporter) Shutdown(context.Context) error { return nil }
func (e *retainingSpanExporter) spans() tracetest.SpanStubs {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append(tracetest.SpanStubs(nil), e.seen...)
}
