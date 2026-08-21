package neatlogs

import (
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestShutdownEndsActiveSpansChildFirstAndAppliesInterruptionStatus(t *testing.T) {
	ctx := context.Background()
	sink := &retainingExporter{InMemoryExporter: tracetest.NewInMemoryExporter()}
	shutdown, err := Init(ctx, Config{
		WorkflowName: "graceful-shutdown",
	}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}

	childCtx, root, _ := Trace(ctx, "workflow")
	root.SetStatus(codes.Ok, "successful so far")
	_, _, _ = StartSpan(childCtx, "agent.unset", "agent")
	_, failed, _ := StartSpan(childCtx, "agent.failed", "agent")
	failed.SetStatus(codes.Error, "existing failure")

	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	spans := sink.GetSpans()
	rootIndex, unsetIndex, failedIndex := -1, -1, -1
	var rootSpan, unsetSpan, failedSpan tracetest.SpanStub
	for i, span := range spans {
		switch span.Name {
		case "workflow":
			rootIndex, rootSpan = i, span
		case "agent.unset":
			unsetIndex, unsetSpan = i, span
		case "agent.failed":
			failedIndex, failedSpan = i, span
		}
	}
	if unsetIndex < 0 || failedIndex < 0 || rootIndex < 0 || unsetIndex >= rootIndex || failedIndex >= rootIndex {
		t.Fatalf("span order = unset %d, failed %d, root %d; want children before root", unsetIndex, failedIndex, rootIndex)
	}
	if rootSpan.Status.Code != codes.Ok {
		t.Fatalf("root status = %v, want OK", rootSpan.Status.Code)
	}
	if unsetSpan.Status.Code != codes.Error || unsetSpan.Status.Description != "shutdown" {
		t.Fatalf("unset child status = (%v, %q), want (Error, shutdown)", unsetSpan.Status.Code, unsetSpan.Status.Description)
	}
	if failedSpan.Status.Code != codes.Error || failedSpan.Status.Description != "existing failure" {
		t.Fatalf("failed child status = (%v, %q), want existing Error", failedSpan.Status.Code, failedSpan.Status.Description)
	}
	for _, span := range []tracetest.SpanStub{unsetSpan, failedSpan, rootSpan} {
		if interrupted, ok := attrBool(span.Attributes, interruptedAttribute); !ok || !interrupted {
			t.Errorf("%s interrupted = %v (present %v), want true", span.Name, interrupted, ok)
		}
		if reason, ok := attrString(span.Attributes, terminationReasonAttribute); !ok || reason != "shutdown" {
			t.Errorf("%s reason = %q (present %v), want shutdown", span.Name, reason, ok)
		}
		if len(span.Events) != 1 || span.Events[0].Name != interruptionEventName {
			t.Errorf("%s interruption events = %#v, want one %q", span.Name, span.Events, interruptionEventName)
		} else if reason, ok := attrString(span.Events[0].Attributes, terminationReasonAttribute); !ok || reason != "shutdown" {
			t.Errorf("%s event reason = %q (present %v), want shutdown", span.Name, reason, ok)
		}
	}
	if byName(sink.InMemoryExporter, completionMarkerName).Name != completionMarkerName {
		t.Fatal("missing completion marker")
	}
}

// tracetest.InMemoryExporter clears its buffer from Shutdown; retain it so this
// test can inspect what the SDK flushed as part of its shutdown call.
type retainingExporter struct {
	*tracetest.InMemoryExporter
}

func (e *retainingExporter) Shutdown(context.Context) error { return nil }

func TestShutdownSignalControllerFlushesBeforeTermination(t *testing.T) {
	controller := newShutdownSignalController()
	shutdownCalled := make(chan os.Signal, 1)
	terminated := make(chan os.Signal, 1)
	go controller.run(
		func(sig os.Signal) { shutdownCalled <- sig },
		func(sig os.Signal) { terminated <- sig },
	)

	controller.signals <- syscall.SIGTERM

	select {
	case sig := <-shutdownCalled:
		if reason := signalTerminationReason(sig); reason != "SIGTERM" {
			t.Fatalf("reason = %q, want SIGTERM", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown was not called")
	}
	select {
	case sig := <-terminated:
		if sig != syscall.SIGTERM {
			t.Fatalf("terminated with %v, want SIGTERM", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("termination was not called")
	}
}

func TestInterruptedReasonIsRuneBounded(t *testing.T) {
	ctx := context.Background()
	sink := &retainingExporter{InMemoryExporter: tracetest.NewInMemoryExporter()}
	lifecycle := newActiveSpanRegistry()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(sink))
	provider.RegisterSpanProcessor(lifecycle)
	_, _ = provider.Tracer("test").Start(ctx, "active")
	reason := strings.Repeat("界", maxTerminationReasonRunes+20)
	if ended := lifecycle.endActiveSpans(reason); ended != 1 {
		t.Fatalf("ended spans = %d, want 1", ended)
	}
	if err := provider.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	span := byName(sink.InMemoryExporter, "active")
	if got := []rune(span.Status.Description); len(got) != maxTerminationReasonRunes {
		t.Fatalf("status reason runes = %d, want %d", len(got), maxTerminationReasonRunes)
	}
	if reason, _ := attrString(span.Attributes, terminationReasonAttribute); len([]rune(reason)) != maxTerminationReasonRunes {
		t.Fatalf("attribute reason runes = %d, want %d", len([]rune(reason)), maxTerminationReasonRunes)
	}
}

type blockingShutdownExporter struct {
	next    *retainingExporter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingShutdownExporter() *blockingShutdownExporter {
	return &blockingShutdownExporter{
		next:    &retainingExporter{InMemoryExporter: tracetest.NewInMemoryExporter()},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (e *blockingShutdownExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return e.next.ExportSpans(ctx, spans)
}

func (e *blockingShutdownExporter) Shutdown(context.Context) error {
	e.once.Do(func() { close(e.entered) })
	<-e.release
	return nil
}

func TestGlobalClosingGateRejectsLateSpansAndOverlappingInit(t *testing.T) {
	ctx := context.Background()
	exporter := newBlockingShutdownExporter()
	shutdown, err := Init(ctx, Config{WorkflowName: "first"}, WithExporter(exporter))
	if err != nil {
		t.Fatal(err)
	}
	global.mu.Lock()
	if global.state != stateRunning {
		t.Fatalf("global state after Init = %v, want running", global.state)
	}
	global.mu.Unlock()
	_, _, _ = Trace(ctx, "active")

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdown(ctx) }()
	<-exporter.entered
	global.mu.Lock()
	if global.state != stateClosing {
		t.Fatalf("global state during shutdown = %v, want closing", global.state)
	}
	global.mu.Unlock()

	_, late, lateEnd := Trace(ctx, "late")
	lateEnd()
	if late.SpanContext().IsValid() || late.IsRecording() {
		t.Fatal("span started after global state entered closing")
	}
	if _, err := Init(ctx, Config{DisableExport: true}); err == nil {
		t.Fatal("Init succeeded while the prior provider was closing")
	}

	close(exporter.release)
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	global.mu.Lock()
	if global.state != stateUninitialized {
		t.Fatalf("global state after shutdown = %v, want uninitialized", global.state)
	}
	global.mu.Unlock()

	reinitialized, err := Init(ctx, Config{DisableExport: true, WorkflowName: "second"})
	if err != nil {
		t.Fatalf("Init after completed close: %v", err)
	}
	if err := reinitialized(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentInitHasSingleRunningOwner(t *testing.T) {
	ctx := context.Background()
	const callers = 16
	start := make(chan struct{})
	results := make(chan struct {
		shutdown ShutdownFunc
		err      error
	}, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			shutdown, err := Init(ctx, Config{DisableExport: true})
			results <- struct {
				shutdown ShutdownFunc
				err      error
			}{shutdown: shutdown, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	var shutdown ShutdownFunc
	for result := range results {
		if result.err == nil {
			successes++
			shutdown = result.shutdown
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent Init calls = %d, want 1", successes)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

type blockingStartProcessor struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingStartProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
}

func (*blockingStartProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (*blockingStartProcessor) Shutdown(context.Context) error   { return nil }
func (*blockingStartProcessor) ForceFlush(context.Context) error { return nil }

func TestSpanStartAndShutdownShareGate(t *testing.T) {
	ctx := context.Background()
	sink := &retainingExporter{InMemoryExporter: tracetest.NewInMemoryExporter()}
	shutdown, err := Init(ctx, Config{WorkflowName: "race"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	processor := &blockingStartProcessor{entered: make(chan struct{}), release: make(chan struct{})}
	global.mu.Lock()
	global.runtime.provider.RegisterSpanProcessor(processor)
	global.mu.Unlock()

	startDone := make(chan struct{})
	go func() {
		_, _, _ = Trace(ctx, "started-before-close")
		close(startDone)
	}()
	<-processor.entered

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdown(ctx) }()
	close(processor.release)
	<-startDone
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}

	span := byName(sink.InMemoryExporter, "started-before-close")
	if span.Name == "" {
		t.Fatal("start that held the gate was lost during shutdown")
	}
	if span.Status.Code != codes.Error {
		t.Fatalf("interrupted status = %v, want Error", span.Status.Code)
	}
}

func TestSanitizeTerminationReason(t *testing.T) {
	got := sanitizeTerminationReason("SIGTERM\nforged=value\x00\tmore")
	if got != "SIGTERM forged=value more" {
		t.Fatalf("sanitizeTerminationReason() = %q", got)
	}

	got = sanitizeTerminationReason(strings.Repeat("x", maxTerminationReasonRunes+10))
	if len([]rune(got)) != maxTerminationReasonRunes {
		t.Fatalf("sanitized rune length = %d, want %d", len([]rune(got)), maxTerminationReasonRunes)
	}
}

var _ sdktrace.SpanExporter = (*blockingShutdownExporter)(nil)

func attrBool(kvs []attribute.KeyValue, key string) (bool, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value.AsBool(), true
		}
	}
	return false, false
}
