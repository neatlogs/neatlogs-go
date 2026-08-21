package neatlogs

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func TestClientsRouteHelpersToIndependentProjects(t *testing.T) {
	ctx := context.Background()
	sinkA := tracetest.NewInMemoryExporter()
	sinkB := tracetest.NewInMemoryExporter()
	clientA, err := NewClient(ctx, Config{WorkflowName: "project-a"}, WithExporter(sinkA))
	if err != nil {
		t.Fatal(err)
	}
	defer clientA.Shutdown(ctx)
	clientB, err := NewClient(ctx, Config{WorkflowName: "project-b"}, WithExporter(sinkB))
	if err != nil {
		t.Fatal(err)
	}
	defer clientB.Shutdown(ctx)

	ctxA := clientA.Context(ctx)
	ctxB := WithClient(ctx, clientB)
	_, llmA := StartLLMSpan(ctxA, LLMCallOptions{Provider: "a", Model: "model-a"})
	llmA.SetOutputMessage("assistant", "from a")
	llmA.End()
	_, llmB := StartLLMSpan(ctxB, LLMCallOptions{Provider: "b", Model: "model-b"})
	llmB.SetOutputMessage("assistant", "from b")
	llmB.End()
	if err := clientA.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := Flush(ctxB); err != nil {
		t.Fatal(err)
	}

	assertOnlyProjectSpans(t, sinkA.GetSpans(), "a.chat", "project-a")
	assertOnlyProjectSpans(t, sinkB.GetSpans(), "b.chat", "project-b")
}

func assertOnlyProjectSpans(t *testing.T, spans tracetest.SpanStubs, operation, workflow string) {
	t.Helper()
	found := false
	for _, span := range spans {
		if span.Name == operation {
			found = true
			if got := resourceString(span, attrs.WorkflowName); got != workflow {
				t.Errorf("%s workflow resource = %q, want %q", operation, got, workflow)
			}
		}
		if span.Name != operation && span.Name != workflow && span.Name != completionMarkerName {
			t.Errorf("unexpected cross-project span %q in %s", span.Name, workflow)
		}
	}
	if !found {
		t.Errorf("missing operation %q", operation)
	}
}

func resourceString(span tracetest.SpanStub, key string) string {
	if span.Resource == nil {
		return ""
	}
	value, ok := span.Resource.Set().Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return value.AsString()
}

func TestClientRebindingDoesNotCrossParentProjects(t *testing.T) {
	ctx := context.Background()
	sinkA := tracetest.NewInMemoryExporter()
	sinkB := tracetest.NewInMemoryExporter()
	clientA, err := NewClient(ctx, Config{WorkflowName: "project-a"}, WithExporter(sinkA))
	if err != nil {
		t.Fatal(err)
	}
	defer clientA.Shutdown(ctx)
	clientB, err := NewClient(ctx, Config{WorkflowName: "project-b"}, WithExporter(sinkB))
	if err != nil {
		t.Fatal(err)
	}
	defer clientB.Shutdown(ctx)

	ctxA, spanA, endA := Trace(WithClient(ctx, clientA), "a.root")
	rebound := WithClient(ctxA, clientB)
	_, spanB, endB := Trace(rebound, "b.root")
	endB()
	endA()
	if err := clientA.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := clientB.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	b := byName(sinkB, "b.root")
	if b.Parent.IsValid() {
		t.Fatalf("client B root inherited client A parent %s", b.Parent.SpanID())
	}
	if spanA.SpanContext().TraceID() == spanB.SpanContext().TraceID() {
		t.Fatal("cross-project spans share a trace ID")
	}
}

func TestConcurrentClientsRemainIsolated(t *testing.T) {
	ctx := context.Background()
	sinkA := tracetest.NewInMemoryExporter()
	sinkB := tracetest.NewInMemoryExporter()
	clientA, err := NewClient(ctx, Config{WorkflowName: "concurrent-a"}, WithExporter(sinkA))
	if err != nil {
		t.Fatal(err)
	}
	defer clientA.Shutdown(ctx)
	clientB, err := NewClient(ctx, Config{WorkflowName: "concurrent-b"}, WithExporter(sinkB))
	if err != nil {
		t.Fatal(err)
	}
	defer clientB.Shutdown(ctx)

	const perClient = 64
	var wg sync.WaitGroup
	for _, item := range []struct {
		client *Client
		prefix string
	}{{clientA, "a"}, {clientB, "b"}} {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			bound := WithClient(ctx, item.client)
			for i := 0; i < perClient; i++ {
				_, _, end := Trace(bound, fmt.Sprintf("%s.%d", item.prefix, i))
				end()
			}
		}()
	}
	wg.Wait()
	if err := clientA.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := clientB.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	assertPrefixCount(t, sinkA.GetSpans(), "a.", perClient)
	assertPrefixCount(t, sinkB.GetSpans(), "b.", perClient)
}

func assertPrefixCount(t *testing.T, spans tracetest.SpanStubs, prefix string, want int) {
	t.Helper()
	got := 0
	for _, span := range spans {
		if len(span.Name) >= len(prefix) && span.Name[:len(prefix)] == prefix {
			got++
		}
	}
	if got != want {
		t.Fatalf("%s spans = %d, want %d", prefix, got, want)
	}
}

func TestClientShutdownIsIndependentAndClosedContextDoesNotFallback(t *testing.T) {
	ctx := context.Background()
	globalSink := tracetest.NewInMemoryExporter()
	globalShutdown, err := Init(ctx, Config{WorkflowName: "global"}, WithExporter(globalSink))
	if err != nil {
		t.Fatal(err)
	}
	defer globalShutdown(ctx)

	clientSink := &retainingExporter{InMemoryExporter: tracetest.NewInMemoryExporter()}
	client, err := NewClient(ctx, Config{WorkflowName: "client"}, WithExporter(clientSink))
	if err != nil {
		t.Fatal(err)
	}
	clientCtx := WithClient(ctx, client)
	_, _, activeEnd := Trace(clientCtx, "client.active")
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	activeEnd()

	_, closedSpan, closedEnd := Trace(clientCtx, "must-not-leak")
	closedEnd()
	if closedSpan.SpanContext().IsValid() {
		t.Fatal("closed client context fell back to global Init")
	}
	_, _, globalEnd := Trace(ctx, "global.still-running")
	globalEnd()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if byName(globalSink, "global.still-running").Name == "" {
		t.Fatal("client shutdown stopped global Init")
	}
	if byName(globalSink, "must-not-leak").Name != "" || byName(clientSink.InMemoryExporter, "must-not-leak").Name != "" {
		t.Fatal("closed client span was exported")
	}
	active := byName(clientSink.InMemoryExporter, "client.active")
	if active.Status.Code != codes.Error {
		t.Fatalf("client active span status = %v, want interrupted Error", active.Status.Code)
	}
}

func TestClientShutdownClosesActiveSpansChildFirst(t *testing.T) {
	ctx := context.Background()
	sink := &retainingExporter{InMemoryExporter: tracetest.NewInMemoryExporter()}
	client, err := NewClient(ctx, Config{WorkflowName: "client-child-first"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	rootCtx, _, rootEnd := Trace(WithClient(ctx, client), "client.root")
	_, _, childEnd := StartSpan(rootCtx, "client.child", "agent")
	const shutdownCallers = 8
	start := make(chan struct{})
	errs := make(chan error, shutdownCallers)
	var shutdownWG sync.WaitGroup
	for i := 0; i < shutdownCallers; i++ {
		shutdownWG.Add(1)
		go func() {
			defer shutdownWG.Done()
			<-start
			errs <- client.Shutdown(ctx)
		}()
	}
	close(start)
	shutdownWG.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	childEnd()
	rootEnd()

	rootIndex, childIndex := -1, -1
	for i, span := range sink.GetSpans() {
		switch span.Name {
		case "client.root":
			rootIndex = i
		case "client.child":
			childIndex = i
		}
	}
	if childIndex < 0 || rootIndex < 0 || childIndex >= rootIndex {
		t.Fatalf("client shutdown order child=%d root=%d", childIndex, rootIndex)
	}
	for _, name := range []string{"client.child", "client.root"} {
		span := byName(sink.InMemoryExporter, name)
		if span.Status.Code != codes.Error {
			t.Errorf("%s status = %v, want interrupted Error", name, span.Status.Code)
		}
		if len(span.Events) != 1 || span.Events[0].Name != interruptionEventName {
			t.Errorf("%s interruption events = %#v", name, span.Events)
		}
	}
}

func TestClientClosingGateDoesNotBlockOtherClientOrRecreation(t *testing.T) {
	ctx := context.Background()
	blocking := newBlockingShutdownExporter()
	closingClient, err := NewClient(ctx, Config{WorkflowName: "closing"}, WithExporter(blocking))
	if err != nil {
		t.Fatal(err)
	}
	closingClient.runtime.mu.Lock()
	if closingClient.runtime.state != stateRunning {
		t.Fatalf("new Client state = %v, want running", closingClient.runtime.state)
	}
	closingClient.runtime.mu.Unlock()
	otherSink := tracetest.NewInMemoryExporter()
	otherClient, err := NewClient(ctx, Config{WorkflowName: "other"}, WithExporter(otherSink))
	if err != nil {
		t.Fatal(err)
	}
	defer otherClient.Shutdown(ctx)

	closeDone := make(chan error, 1)
	go func() { closeDone <- closingClient.Shutdown(ctx) }()
	<-blocking.entered
	closingClient.runtime.mu.Lock()
	if closingClient.runtime.state != stateClosing {
		t.Fatalf("Client state during shutdown = %v, want closing", closingClient.runtime.state)
	}
	closingClient.runtime.mu.Unlock()

	_, late, lateEnd := Trace(WithClient(ctx, closingClient), "closing.late")
	lateEnd()
	if late.SpanContext().IsValid() {
		t.Fatal("closing Client accepted a new span")
	}
	_, _, otherEnd := Trace(WithClient(ctx, otherClient), "other.live")
	otherEnd()
	if err := otherClient.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if byName(otherSink, "other.live").Name == "" {
		t.Fatal("one Client's close blocked another Client")
	}

	close(blocking.release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	closingClient.runtime.mu.Lock()
	if closingClient.runtime.state != stateClosed {
		t.Fatalf("Client state after shutdown = %v, want closed", closingClient.runtime.state)
	}
	closingClient.runtime.mu.Unlock()
	recreated, err := NewClient(ctx, Config{DisableExport: true, WorkflowName: "recreated"})
	if err != nil {
		t.Fatalf("new Client after another closed: %v", err)
	}
	if err := recreated.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
