package neatlogs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// A bare provider span (kind=llm) with no surrounding span must get a synthetic
// WORKFLOW root, otherwise the backend finalizer drops the trace.
func TestAutoRoot_WrapsBareLLMSpan(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf-test"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	_, span, end := StartProviderSpan(ctx, "google_genai.models.generate_content", attrs.KindLLM)
	span.SetAttributes(attribute.String(attrs.SpanKind, attrs.KindLLM))
	end()

	Flush(ctx)

	var sawWorkflowRoot, sawLLM bool
	var llmHasParent bool
	for _, s := range sink.GetSpans() {
		kind := spanKindOf(s.Attributes)
		switch kind {
		case attrs.KindWorkflow:
			if !s.Parent.IsValid() {
				sawWorkflowRoot = true
			}
		case attrs.KindLLM:
			sawLLM = true
			llmHasParent = s.Parent.IsValid()
		}
	}
	if !sawWorkflowRoot {
		t.Error("expected a parentless workflow root span")
	}
	if !sawLLM || !llmHasParent {
		t.Error("expected the llm span to be nested under the workflow root")
	}
}

// When the caller already has a recording span active, auto-root must NOT fire
// (the existing span anchors the trace); double-rooting would distort it.
func TestAutoRoot_SkipsWhenParentRecording(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf-test"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	// Simulate an existing Neatlogs parent (e.g. an explicit framework root).
	parentCtx, _, endParent := StartSpan(ctx, "user-root", "chain")

	_, span, end := StartProviderSpan(parentCtx, "google_genai.models.generate_content", attrs.KindLLM)
	span.SetAttributes(attribute.String(attrs.SpanKind, attrs.KindLLM))
	end()
	endParent()

	Flush(ctx)

	workflowRoots := 0
	for _, s := range sink.GetSpans() {
		if spanKindOf(s.Attributes) == attrs.KindWorkflow {
			workflowRoots++
		}
	}
	if workflowRoots != 0 {
		t.Errorf("auto-root must not fire under a recording parent; got %d workflow spans", workflowRoots)
	}
}

func spanKindOf(kvs []attribute.KeyValue) string {
	for _, kv := range kvs {
		if string(kv.Key) == attrs.SpanKind {
			return kv.Value.AsString()
		}
	}
	return ""
}
