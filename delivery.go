package neatlogs

import (
	"context"
	"errors"
	"sync/atomic"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// DeliveryDiagnosticsSnapshot reports bounded-queue, privacy, and final-export
// loss for one private Neatlogs pipeline.
type DeliveryDiagnosticsSnapshot struct {
	SpanQueueDrops           uint64                   `json:"span_queue_drops"`
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
		SpanQueueDrops: d.spanQueueDrops.Load(), SpanExportFailures: d.spanExportFailures.Load(),
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
