package neatlogs

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func TestSetTraceOutputWritesOwnedRootOnly(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := NewClient(ctx, Config{WorkflowName: "root-output"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	rootCtx, _, rootEnd := Trace(WithClient(ctx, client), "owned.root")
	childCtx, _, childEnd := StartSpan(rootCtx, "owned.child", "TOOL")
	if err := SetTraceOutput(childCtx, map[string]any{"answer": 42}); err != nil {
		t.Fatal(err)
	}
	childEnd()
	rootEnd()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := attributeString(byName(sink, "owned.root").Attributes, attrs.Output); got != `{"answer":42}` {
		t.Fatalf("root output = %q", got)
	}
	if got := attributeString(byName(sink, "owned.child").Attributes, attrs.Output); got != "" {
		t.Fatalf("child unexpectedly owns trace output %q", got)
	}
}

func TestSetTraceOutputFindsOwnedAutoRootFromProviderContext(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := NewClient(ctx, Config{WorkflowName: "auto-output"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	providerCtx, _, end := StartProviderSpan(WithClient(ctx, client), "provider.llm", "LLM")
	if err := SetTraceOutput(providerCtx, "final answer"); err != nil {
		t.Fatal(err)
	}
	end()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := attributeString(byName(sink, "auto-output").Attributes, attrs.Output); got != "final answer" {
		t.Fatalf("auto-root output = %q", got)
	}
	if got := attributeString(byName(sink, "provider.llm").Attributes, attrs.Output); got != "" {
		t.Fatalf("provider child unexpectedly owns output %q", got)
	}
}

func TestSetTraceOutputRejectsForeignRemoteAndCrossClientContexts(t *testing.T) {
	ctx := context.Background()
	clientA, err := NewClient(ctx, Config{DisableExport: true, WorkflowName: "a"})
	if err != nil {
		t.Fatal(err)
	}
	defer clientA.Shutdown(ctx)
	clientB, err := NewClient(ctx, Config{DisableExport: true, WorkflowName: "b"})
	if err != nil {
		t.Fatal(err)
	}
	defer clientB.Shutdown(ctx)
	rootA, _, endA := Trace(WithClient(ctx, clientA), "a.root")
	defer endA()
	if err := SetTraceOutput(WithClient(rootA, clientB), "wrong owner"); !errors.Is(err, ErrNoActiveTrace) {
		t.Fatalf("cross-client SetTraceOutput error = %v", err)
	}
	remote := withPrivateTraceContext(WithClient(ctx, clientA), privateSpanContext(rootA), nil)
	if err := SetTraceOutput(remote, "remote"); !errors.Is(err, ErrNoActiveTrace) {
		t.Fatalf("remote SetTraceOutput error = %v", err)
	}
}

func TestSetTraceOutputFailsAfterRootEndsAndOnEncodingError(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(ctx, Config{DisableExport: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	rootCtx, _, end := Trace(WithClient(ctx, client), "ended.root")
	if err := SetTraceOutput(rootCtx, func() {}); err == nil {
		t.Fatal("SetTraceOutput accepted an unencodable value")
	}
	end()
	if err := SetTraceOutput(rootCtx, "late"); !errors.Is(err, ErrNoActiveTrace) {
		t.Fatalf("late SetTraceOutput error = %v", err)
	}
}
