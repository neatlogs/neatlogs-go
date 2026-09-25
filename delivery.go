package neatlogs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

// DeliveryDiagnosticsSnapshot reports bounded-queue, privacy, and final-export
// loss for one private Neatlogs pipeline.
type DeliveryDiagnosticsSnapshot struct {
	SpanQueueDrops           uint64                   `json:"span_queue_drops"`
	SpanQueueByteDrops       uint64                   `json:"span_queue_byte_drops"`
	SpanExportFailures       uint64                   `json:"span_export_failures"`
	MaskedSpanDrops          uint64                   `json:"masked_span_drops"`
	UploadAuthorityAvailable bool                     `json:"upload_authority_available"`
	TypedMediaUploads        uint64                   `json:"typed_media_uploads"`
	TypedMediaUploadFailures uint64                   `json:"typed_media_upload_failures"`
	OTLPOverflowUploads      uint64                   `json:"otlp_overflow_uploads"`
	OTLPOverflowFailures     uint64                   `json:"otlp_overflow_failures"`
	OTLPOverflowUnavailable  uint64                   `json:"otlp_overflow_unavailable"`
	LastUploadFailure        *UploadFailureDiagnostic `json:"last_upload_failure,omitempty"`
}

// UploadFailureDiagnostic is a secret-free report of the most recent upload
// failure. It deliberately cannot contain a signed URL, upload headers, API
// key, response body, or payload content.
type UploadFailureDiagnostic struct {
	Stage      string `json:"stage"`
	ReasonCode string `json:"reason_code"`
	Retryable  bool   `json:"retryable"`
}

type deliveryDiagnostics struct {
	spanQueueDrops           atomic.Uint64
	spanQueueByteDrops       atomic.Uint64
	spanExportFailures       atomic.Uint64
	maskedSpanDrops          atomic.Uint64
	uploadAuthorityAvailable atomic.Bool
	typedMediaUploads        atomic.Uint64
	typedMediaUploadFailures atomic.Uint64
	otlpOverflowUploads      atomic.Uint64
	otlpOverflowFailures     atomic.Uint64
	otlpOverflowUnavailable  atomic.Uint64
	lastUploadFailure        atomic.Pointer[UploadFailureDiagnostic]
}

func (d *deliveryDiagnostics) snapshot() DeliveryDiagnosticsSnapshot {
	if d == nil {
		return DeliveryDiagnosticsSnapshot{}
	}
	return DeliveryDiagnosticsSnapshot{
		SpanQueueDrops: d.spanQueueDrops.Load(), SpanQueueByteDrops: d.spanQueueByteDrops.Load(), SpanExportFailures: d.spanExportFailures.Load(),
		MaskedSpanDrops: d.maskedSpanDrops.Load(), UploadAuthorityAvailable: d.uploadAuthorityAvailable.Load(),
		TypedMediaUploads: d.typedMediaUploads.Load(), TypedMediaUploadFailures: d.typedMediaUploadFailures.Load(),
		OTLPOverflowUploads: d.otlpOverflowUploads.Load(), OTLPOverflowFailures: d.otlpOverflowFailures.Load(),
		OTLPOverflowUnavailable: d.otlpOverflowUnavailable.Load(), LastUploadFailure: cloneUploadFailure(d.lastUploadFailure.Load()),
	}
}

func (d *deliveryDiagnostics) recordUploadFailure(err error) {
	if d == nil {
		return
	}
	diagnostic := &UploadFailureDiagnostic{Stage: "upload", ReasonCode: "unknown", Retryable: false}
	var failure *uploadFailure
	if errors.As(err, &failure) {
		diagnostic.Stage = failure.stage
		diagnostic.ReasonCode = failure.reasonCode
		diagnostic.Retryable = failure.retryable
	}
	d.lastUploadFailure.Store(diagnostic)
}

func cloneUploadFailure(value *UploadFailureDiagnostic) *UploadFailureDiagnostic {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// deliveryQueue bounds ended-but-not-yet-exported spans by count and by an
// estimate of their retained attribute bytes. With the 10k attribute budget a
// long conversation can make each LLM span hundreds of KB, so the slot count
// alone does not bound memory when export stalls. Spans are dropped whole and
// counted; attributes are never truncated here.
type deliveryQueue struct {
	slots    chan struct{}
	diag     *deliveryDiagnostics
	maxBytes int64

	mu    sync.Mutex
	used  int64
	sizes map[spanKey][]int64
}

type spanKey struct {
	trace trace.TraceID
	span  trace.SpanID
}

func newDeliveryQueue(capacity int, diag *deliveryDiagnostics) *deliveryQueue {
	return newByteBoundedDeliveryQueue(capacity, defaultMaxQueueBytes, diag)
}

func newByteBoundedDeliveryQueue(capacity int, maxBytes int64, diag *deliveryDiagnostics) *deliveryQueue {
	return &deliveryQueue{
		slots: make(chan struct{}, capacity), diag: diag, maxBytes: maxBytes,
		sizes: make(map[spanKey][]int64),
	}
}

func keyOf(span sdktrace.ReadOnlySpan) spanKey {
	sc := span.SpanContext()
	return spanKey{trace: sc.TraceID(), span: sc.SpanID()}
}

func (q *deliveryQueue) acquire(span sdktrace.ReadOnlySpan) bool {
	size := estimateSpanBytes(span)
	q.mu.Lock()
	// A single span larger than the whole budget is still admitted into an
	// empty queue so it cannot be starved forever.
	if q.maxBytes > 0 && q.used > 0 && q.used+size > q.maxBytes {
		q.mu.Unlock()
		q.diag.spanQueueDrops.Add(1)
		q.diag.spanQueueByteDrops.Add(1)
		return false
	}
	select {
	case q.slots <- struct{}{}:
	default:
		q.mu.Unlock()
		q.diag.spanQueueDrops.Add(1)
		return false
	}
	q.used += size
	key := keyOf(span)
	q.sizes[key] = append(q.sizes[key], size)
	q.mu.Unlock()
	return true
}

func (q *deliveryQueue) release(spans []sdktrace.ReadOnlySpan) {
	q.mu.Lock()
	for _, span := range spans {
		if span == nil {
			continue
		}
		key := keyOf(span)
		if held := q.sizes[key]; len(held) > 0 {
			q.used -= held[0]
			if len(held) == 1 {
				delete(q.sizes, key)
			} else {
				q.sizes[key] = held[1:]
			}
		}
		select {
		case <-q.slots:
		default:
		}
	}
	if q.used < 0 {
		q.used = 0
	}
	q.mu.Unlock()
}

func (q *deliveryQueue) usedBytes() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used
}

// estimateSpanBytes is a cheap upper-ish estimate of what an ended span keeps
// alive: its name, attribute keys and values, and event/link attributes.
func estimateSpanBytes(span sdktrace.ReadOnlySpan) int64 {
	const overhead = 256
	total := int64(overhead + len(span.Name()))
	total += attributesBytes(span.Attributes())
	for _, event := range span.Events() {
		total += int64(64+len(event.Name)) + attributesBytes(event.Attributes)
	}
	for _, link := range span.Links() {
		total += 64 + attributesBytes(link.Attributes)
	}
	return total
}

func attributesBytes(kvs []attribute.KeyValue) int64 {
	var total int64
	for _, kv := range kvs {
		total += int64(32 + len(kv.Key))
		switch kv.Value.Type() {
		case attribute.STRING:
			total += int64(len(kv.Value.AsString()))
		case attribute.STRINGSLICE:
			for _, v := range kv.Value.AsStringSlice() {
				total += int64(16 + len(v))
			}
		case attribute.BOOLSLICE:
			total += int64(len(kv.Value.AsBoolSlice()))
		case attribute.INT64SLICE:
			total += int64(8 * len(kv.Value.AsInt64Slice()))
		case attribute.FLOAT64SLICE:
			total += int64(8 * len(kv.Value.AsFloat64Slice()))
		default:
			total += 8
		}
	}
	return total
}

// boundedSpanProcessor counts and drops before OTel's private queue can
// overflow. Slots cover all ended-but-not-yet-attempted spans, including the
// processor's in-progress batch.
type boundedSpanProcessor struct {
	next    sdktrace.SpanProcessor
	queue   *deliveryQueue
	media   *internalmedia.Store
	stopped atomic.Bool
}

func (p *boundedSpanProcessor) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	if !p.stopped.Load() && p.media != nil && span != nil && span.SpanContext().IsSampled() {
		internalmedia.RegisterSpan(span.SpanContext(), p.media)
	}
	p.next.OnStart(ctx, span)
}

func (p *boundedSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if p.stopped.Load() || span == nil || span.SpanContext().TraceFlags()&trace.FlagsSampled == 0 {
		if span != nil {
			internalmedia.DiscardSpan(span.SpanContext())
		}
		return
	}
	if p.queue.acquire(span) {
		p.next.OnEnd(span)
		return
	}
	internalmedia.DiscardSpan(span.SpanContext())
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
