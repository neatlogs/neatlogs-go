package neatlogs

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var (
	ErrMaskingFailed = errors.New("neatlogs: export masking failed")
	ErrExportFailed  = errors.New("neatlogs: span export failed")
)

// MaskableSpan is a detached export snapshot created after canonical
// normalization. It is never shared with the application's live span.
type MaskableSpan struct {
	Name       string
	Attributes []attribute.KeyValue
	Events     []sdktrace.Event
	Links      []sdktrace.Link
	Status     sdktrace.Status
	Resource   *resource.Resource
	Scope      instrumentation.Scope
}

// MaskFunc redacts a detached canonical export snapshot. Returning an error,
// panicking, or exceeding the configured timeout drops that span fail-closed.
type MaskFunc func(ctx context.Context, span *MaskableSpan) error

type maskContextKey struct{}

// WithMask applies mask to Neatlogs spans started with ctx. It takes precedence
// over Config.Mask for those spans and remains private to the selected runtime.
func WithMask(ctx context.Context, mask MaskFunc) context.Context {
	if mask == nil {
		return ctx
	}
	return context.WithValue(ctx, maskContextKey{}, mask)
}

func maskFromContext(ctx context.Context) MaskFunc {
	mask, _ := ctx.Value(maskContextKey{}).(MaskFunc)
	return mask
}

// ExportHealth is a cumulative snapshot for one private Neatlogs runtime.
type ExportHealth struct {
	DroppedSpans   uint64
	MaskFailures   uint64
	ExportFailures uint64
}

// Err reports unhealthy counters using stable sentinels without including
// callback/exporter error text that may itself contain secrets.
func (h ExportHealth) Err() error {
	var errs []error
	if h.MaskFailures > 0 || h.DroppedSpans > 0 {
		errs = append(errs, fmt.Errorf("%w: dropped_spans=%d mask_failures=%d", ErrMaskingFailed, h.DroppedSpans, h.MaskFailures))
	}
	if h.ExportFailures > 0 {
		errs = append(errs, fmt.Errorf("%w: failures=%d", ErrExportFailed, h.ExportFailures))
	}
	return errors.Join(errs...)
}

type exportHealthState struct {
	mu     sync.Mutex
	health ExportHealth
}

func (s *exportHealthState) snapshot() ExportHealth {
	if s == nil {
		return ExportHealth{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

func (s *exportHealthState) recordMaskFailure() {
	s.mu.Lock()
	s.health.DroppedSpans++
	s.health.MaskFailures++
	s.mu.Unlock()
}

func (s *exportHealthState) recordExportFailure() {
	s.mu.Lock()
	s.health.ExportFailures++
	s.mu.Unlock()
}

type spanMaskRegistry struct {
	mu       sync.Mutex
	bySpanID map[trace.SpanID]MaskFunc
}

func newSpanMaskRegistry() *spanMaskRegistry {
	return &spanMaskRegistry{bySpanID: make(map[trace.SpanID]MaskFunc)}
}

func (r *spanMaskRegistry) register(span trace.Span, mask MaskFunc) {
	if r == nil || mask == nil || !span.IsRecording() || !span.SpanContext().SpanID().IsValid() {
		return
	}
	r.mu.Lock()
	r.bySpanID[span.SpanContext().SpanID()] = mask
	r.mu.Unlock()
}

func (r *spanMaskRegistry) take(id trace.SpanID) MaskFunc {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	mask := r.bySpanID[id]
	delete(r.bySpanID, id)
	return mask
}

func (r *spanMaskRegistry) clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	clear(r.bySpanID)
	r.mu.Unlock()
}

// ExportHealthFor returns health for the Client bound to ctx, or the global
// Init runtime when no Client is bound.
func ExportHealthFor(ctx context.Context) ExportHealth {
	if client, ok := ClientFromContext(ctx); ok {
		return client.ExportHealth()
	}
	global.mu.Lock()
	runtime := global.runtime
	global.mu.Unlock()
	if runtime == nil {
		return ExportHealth{}
	}
	return runtime.exportHealth()
}
