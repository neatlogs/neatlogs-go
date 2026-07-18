package neatlogs

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func attrInt(kvs []attribute.KeyValue, key string) (int64, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value.AsInt64(), true
		}
	}
	return 0, false
}

// StartToolSpanFromHeaders continues an upstream trace, opens a TOOL child, and
// records input; SetOutput lands on the same span.
func TestStartToolSpanFromHeaders_ContinuesTraceAndRecordsIO(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	// Upstream (e.g. the TypeScript runtime) starts a span and injects headers.
	parentCtx, parent, endParent := StartSpan(ctx, "ts.workflow", "workflow")
	headers := http.Header{}
	InjectTraceContext(parentCtx, propagation.HeaderCarrier(headers))

	_, tool := StartToolSpanFromHeaders(
		context.Background(), headers, "list_formulas", `{"orgId":1}`,
		IdentifyOptions{SessionID: "conv_1", EndUserID: "user_1"},
	)
	tool.SetOutput(`{"formulas":[]}`)
	tool.End()
	endParent()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	child := byName(sink, "list_formulas")
	if child.Name != "list_formulas" {
		t.Fatal("expected a list_formulas tool span")
	}
	if child.SpanContext.TraceID() != parent.SpanContext().TraceID() {
		t.Error("tool span must share the upstream trace id")
	}
	if child.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Error("tool span must be parented on the upstream span")
	}
	if v, _ := attrString(child.Attributes, attrs.SpanKind); v != attrs.KindTool {
		t.Errorf("span kind = %q, want tool", v)
	}
	if v, _ := attrString(child.Attributes, attrs.ToolInput); v != `{"orgId":1}` {
		t.Errorf("tool input = %q", v)
	}
	if v, _ := attrString(child.Attributes, attrs.ToolOutput); v != `{"formulas":[]}` {
		t.Errorf("tool output = %q", v)
	}
}

// SetError marks the tool span failed and records the error as output.
func TestStartToolSpanFromHeaders_SetError(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	_, tool := StartToolSpanFromHeaders(
		ctx, http.Header{}, "boom", "{}", IdentifyOptions{},
	)
	tool.SetError(errors.New("kaboom"))
	tool.End()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	span := byName(sink, "boom")
	if span.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", span.Status.Code)
	}
	if v, _ := attrString(span.Attributes, attrs.ToolOutput); v != "kaboom" {
		t.Errorf("tool output = %q, want error text", v)
	}
	var sawRoot bool
	for _, candidate := range sink.GetSpans() {
		if spanKindOf(candidate.Attributes) == attrs.KindWorkflow && !candidate.Parent.IsValid() {
			sawRoot = true
		}
	}
	if !sawRoot {
		t.Error("tool span without incoming trace context must get an auto-root")
	}
}

// StartLLMSpan auto-roots a bare provider call and records provider/model,
// input+output messages, usage and finish reason.
func TestStartLLMSpan_RecordsLLMSemantics(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	_, llm := StartLLMSpan(ctx, LLMCallOptions{
		Provider:  "openai",
		Model:     "gpt-5.5",
		MaxTokens: 256,
		Messages: []LLMMessage{
			{Role: "system", Content: "be concise"},
			{Role: "user", Content: "hi"},
		},
	})
	llm.SetOutputMessage("assistant", "hello")
	llm.SetUsage(10, 5, 15)
	llm.SetFinishReason("stop")
	llm.End()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	span := byName(sink, "openai.chat")
	if v, _ := attrString(span.Attributes, attrs.LLMProvider); v != "openai" {
		t.Errorf("provider = %q", v)
	}
	if v, _ := attrString(span.Attributes, attrs.LLMModelName); v != "gpt-5.5" {
		t.Errorf("model = %q", v)
	}
	if v, _ := attrString(span.Attributes, attrs.LLMInputMessagePrefix+"1.content"); v != "hi" {
		t.Errorf("input msg[1] = %q", v)
	}
	if v, _ := attrString(span.Attributes, attrs.LLMOutputMessagePrefix+"0.content"); v != "hello" {
		t.Errorf("output = %q", v)
	}
	if v, _ := attrInt(span.Attributes, attrs.LLMTokenTotal); v != 15 {
		t.Errorf("total tokens = %d, want 15", v)
	}
	if v, _ := attrString(span.Attributes, attrs.LLMFinishReason); v != "stop" {
		t.Errorf("finish = %q", v)
	}

	// A bare LLM call must get a synthetic WORKFLOW root (finalizer requirement).
	var sawRoot bool
	for _, s := range sink.GetSpans() {
		if spanKindOf(s.Attributes) == attrs.KindWorkflow && !s.Parent.IsValid() {
			sawRoot = true
		}
	}
	if !sawRoot {
		t.Error("expected an auto-root workflow span above the bare llm span")
	}
}

// StartRetrieverSpan records query/top-k and document count.
func TestStartRetrieverSpan_RecordsRetrieval(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	_, r := StartRetrieverSpan(ctx, "inkeep_rag", "how do runways work", 5)
	r.SetDocuments([]map[string]any{{"title": "Runways"}}, 1)
	r.End()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	span := byName(sink, "inkeep_rag")
	if v, _ := attrString(span.Attributes, attrs.SpanKind); v != attrs.KindRetriever {
		t.Errorf("kind = %q, want retriever", v)
	}
	if v, _ := attrString(span.Attributes, attrs.RetrieverQuery); v != "how do runways work" {
		t.Errorf("query = %q", v)
	}
	if v, _ := attrInt(span.Attributes, attrs.RetrieverTopK); v != 5 {
		t.Errorf("top_k = %d, want 5", v)
	}
	if v, _ := attrInt(span.Attributes, attrs.DocumentsCount); v != 1 {
		t.Errorf("doc count = %d, want 1", v)
	}
	if v, _ := attrString(span.Attributes, attrs.Output); v != `[{"title":"Runways"}]` {
		t.Errorf("output = %q", v)
	}
}

func TestRetrieverSpanSetDocumentsRecordsExplicitEmptyOutput(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	_, r := StartRetrieverSpan(ctx, "empty_retrieval", "missing query", 5)
	r.SetDocuments([]map[string]any{}, 0)
	r.End()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	span := byName(sink, "empty_retrieval")
	if v, _ := attrString(span.Attributes, attrs.RetrieverDocuments); v != "[]" {
		t.Errorf("documents = %q, want []", v)
	}
	if v, _ := attrString(span.Attributes, attrs.Output); v != "[]" {
		t.Errorf("output = %q, want []", v)
	}
}
