package neatlogs

import (
	"context"
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// DeliveryDiagnosticsSnapshot reports bounded-queue, privacy, and final-export
// loss for one private Neatlogs pipeline.
type DeliveryDiagnosticsSnapshot struct {
	SpanQueueDrops     uint64 `json:"span_queue_drops"`
	SpanExportFailures uint64 `json:"span_export_failures"`
	MaskedSpanDrops    uint64 `json:"masked_span_drops"`
}

type deliveryDiagnostics struct {
	spanQueueDrops     atomic.Uint64
	spanExportFailures atomic.Uint64
	maskedSpanDrops    atomic.Uint64
}

func (d *deliveryDiagnostics) snapshot() DeliveryDiagnosticsSnapshot {
	if d == nil {
		return DeliveryDiagnosticsSnapshot{}
	}
	return DeliveryDiagnosticsSnapshot{
		SpanQueueDrops: d.spanQueueDrops.Load(), SpanExportFailures: d.spanExportFailures.Load(),
		MaskedSpanDrops: d.maskedSpanDrops.Load(),
	}
}

type deliveryQueue struct {
	slots chan struct{}
	diag  *deliveryDiagnostics
}

func newDeliveryQueue(capacity int, diag *deliveryDiagnostics) *deliveryQueue {
	return &deliveryQueue{slots: make(chan struct{}, capacity), diag: diag}
}

func (q *deliveryQueue) acquire() bool {
	select {
	case q.slots <- struct{}{}:
		return true
	default:
		q.diag.spanQueueDrops.Add(1)
		return false
	}
}

func (q *deliveryQueue) release(count int) {
	for range count {
		select {
		case <-q.slots:
		default:
			return
		}
	}
}

// boundedSpanProcessor counts and drops before OTel's private queue can
// overflow. Slots cover all ended-but-not-yet-attempted spans, including the
// processor's in-progress batch.
type boundedSpanProcessor struct {
	next    sdktrace.SpanProcessor
	queue   *deliveryQueue
	stopped atomic.Bool
}

func (p *boundedSpanProcessor) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	p.next.OnStart(ctx, span)
}

func (p *boundedSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if p.stopped.Load() || span == nil || span.SpanContext().TraceFlags()&trace.FlagsSampled == 0 {
		return
	}
	if p.queue.acquire() {
		p.next.OnEnd(span)
	}
}

func (p *boundedSpanProcessor) ForceFlush(ctx context.Context) error {
	return p.next.ForceFlush(ctx)
}

func (p *boundedSpanProcessor) Shutdown(ctx context.Context) error {
	p.stopped.Store(true)
	return p.next.Shutdown(ctx)
}

// GetDeliveryDiagnostics returns counters for the Client bound to ctx, or the
// process-wide Neatlogs pipeline when no Client is bound.
func GetDeliveryDiagnostics(ctx context.Context) DeliveryDiagnosticsSnapshot {
	if client, ok := ClientFromContext(ctx); ok {
		return client.DeliveryDiagnostics()
	}
	global.mu.Lock()
	runtime := global.runtime
	last := global.lastDelivery
	global.mu.Unlock()
	if runtime == nil {
		return last
	}
	return runtime.delivery.snapshot()
}
