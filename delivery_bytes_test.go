package neatlogs

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func stubSpan(id byte, payload int) sdktrace.ReadOnlySpan {
	return tracetest.SpanStub{
		Name: "llm",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{1}, SpanID: trace.SpanID{id}, TraceFlags: trace.FlagsSampled,
		}),
		Attributes: []attribute.KeyValue{attribute.String("neatlogs.llm.input_messages.0.content", strings.Repeat("x", payload))},
	}.Snapshot()
}

func TestDeliveryQueueDropsWholeSpanOverByteBudget(t *testing.T) {
	diagnostics := &deliveryDiagnostics{}
	queue := newByteBoundedDeliveryQueue(10, 3000, diagnostics)
	next := &retainingSpanProcessor{}
	processor := &boundedSpanProcessor{next: next, queue: queue}

	processor.OnEnd(stubSpan(1, 1000))
	processor.OnEnd(stubSpan(2, 1000))
	processor.OnEnd(stubSpan(3, 1000))

	if next.ended != 2 {
		t.Fatalf("forwarded spans = %d, want 2", next.ended)
	}
	got := diagnostics.snapshot()
	if got.SpanQueueDrops != 1 || got.SpanQueueByteDrops != 1 {
		t.Fatalf("drops = %d/%d, want 1/1", got.SpanQueueDrops, got.SpanQueueByteDrops)
	}
}

func TestDeliveryQueueReleaseFreesBytesAndSlots(t *testing.T) {
	diagnostics := &deliveryDiagnostics{}
	queue := newByteBoundedDeliveryQueue(1, 3000, diagnostics)
	first, second := stubSpan(1, 2000), stubSpan(2, 2000)

	if !queue.acquire(first) {
		t.Fatal("first span rejected")
	}
	if queue.acquire(second) {
		t.Fatal("second span admitted over budget")
	}
	queue.release([]sdktrace.ReadOnlySpan{first})
	if used := queue.usedBytes(); used != 0 {
		t.Fatalf("used bytes after release = %d, want 0", used)
	}
	if !queue.acquire(second) {
		t.Fatal("second span rejected after release")
	}
}

func TestDeliveryQueueAdmitsOversizedSpanWhenEmpty(t *testing.T) {
	queue := newByteBoundedDeliveryQueue(4, 100, &deliveryDiagnostics{})
	if !queue.acquire(stubSpan(1, 5000)) {
		t.Fatal("oversized span rejected by empty queue")
	}
	if queue.acquire(stubSpan(2, 10)) {
		t.Fatal("span admitted while oversized span holds the budget")
	}
}

type stalledExporter struct{ release chan struct{} }

func (e *stalledExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return nil
}
func (e *stalledExporter) Shutdown(context.Context) error { close(e.release); return nil }

// TestQueueByteBudgetBoundsHeap measures retained heap with long histories and
// a stalled exporter. Opt-in: NEATLOGS_MEMBENCH=1 go test -run QueueByteBudget -v
func TestQueueByteBudgetBoundsHeap(t *testing.T) {
	if os.Getenv("NEATLOGS_MEMBENCH") == "" {
		t.Skip("set NEATLOGS_MEMBENCH=1")
	}
	for _, budget := range []int64{0, defaultMaxQueueBytes} {
		exp := &stalledExporter{release: make(chan struct{})}
		queue := newByteBoundedDeliveryQueue(defaultMaxQueueSize, budget, &deliveryDiagnostics{})
		bsp := sdktrace.NewBatchSpanProcessor(&releasingExporter{next: exp, release: queue.release},
			sdktrace.WithMaxQueueSize(defaultMaxQueueSize), sdktrace.WithMaxExportBatchSize(defaultMaxExportBatchSize),
			sdktrace.WithBatchTimeout(time.Hour))
		limits := sdktrace.NewSpanLimits()
		limits.AttributeCountLimit = 10000
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanLimits(limits),
			sdktrace.WithSpanProcessor(&boundedSpanProcessor{next: bsp, queue: queue}))
		tr := tp.Tracer("membench")
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		content := strings.Repeat("x", 1000)
		for n := 0; n < defaultMaxQueueSize+defaultMaxExportBatchSize; n++ {
			_, sp := tr.Start(context.Background(), "llm")
			attrs := make([]attribute.KeyValue, 0, 1000)
			for i := 0; i < 200; i++ {
				attrs = append(attrs, attribute.String("neatlogs.llm.input_messages."+itoa(i)+".content", content[:992]+itoa8(n)))
			}
			sp.SetAttributes(attrs...)
			sp.End()
		}
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		t.Logf("budget=%d MiB retained_heap=%.0f MB queue_bytes=%.0f MB", budget>>20,
			float64(after.HeapInuse-before.HeapInuse)/1e6, float64(queue.usedBytes())/1e6)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = tp.Shutdown(ctx)
		cancel()
	}
}

type releasingExporter struct {
	next    sdktrace.SpanExporter
	release func([]sdktrace.ReadOnlySpan)
}

func (e *releasingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	defer e.release(spans)
	return e.next.ExportSpans(ctx, spans)
}
func (e *releasingExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }

func itoa(i int) string { return strconv.Itoa(i) }
func itoa8(i int) string {
	s := strconv.Itoa(i)
	return strings.Repeat("0", 8-len(s)) + s
}
