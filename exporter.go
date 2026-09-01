package neatlogs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/neatlogs/neatlogs-go/internal/attributes"
	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

// normalizingExporter wraps a SpanExporter and rewrites every span's attributes
// into the neatlogs.* namespace before delegating the actual export.
//
// Go's OTel SpanProcessor.OnEnd receives a read-only span, so attribute
// normalization cannot happen in a processor (unlike the JS/Python SDKs). We
// normalize at the exporter boundary instead, round-tripping each span through
// a tracetest.SpanStub whose attributes we can edit.
//
// This makes spans created through Neatlogs wrappers or an explicitly injected
// private tracer arrive keyed by the neatlogs.* contract.
type normalizingExporter struct {
	next      trace.SpanExporter
	mapper    *attributes.Mapper
	mask      MaskFunc
	delivery  *deliveryDiagnostics
	uploads   uploadAuthority
	release   func(int)
	maskOnce  sync.Once
	maskSlots chan struct{}
}

const (
	defaultMaskTimeout = 5 * time.Second
	// Mask callbacks historically ran serially on the export worker. Keep one
	// shared slot so existing stateful callbacks remain safe even when an
	// exporter is invoked concurrently.
	defaultMaskWorkers = 1
)

// Local alias keeps the mask conversion independent of tracetest naming in the
// public API while retaining the SDK's lossless read-only-span clone.
type spanStub = tracetest.SpanStub

var _ trace.SpanExporter = (*normalizingExporter)(nil)

func (e *normalizingExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	if e.release != nil {
		defer e.release(len(spans))
	}
	if err := ctx.Err(); err != nil {
		e.discardAndRecord(spans, nil)
		return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
	}
	stubs := make([]spanStub, len(spans))
	for index, s := range spans {
		if err := ctx.Err(); err != nil {
			e.discardAndRecord(spans, nil)
			return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
		}
		stub := tracetest.SpanStubFromReadOnlySpan(s)
		stub.Attributes = e.mapper.Normalize(stub.Attributes)
		stubs[index] = stub
		if err := ctx.Err(); err != nil {
			e.discardAndRecord(spans, nil)
			return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
		}
	}

	keep := make([]bool, len(stubs))
	for index := range keep {
		keep[index] = true
	}
	if err := ctx.Err(); err != nil {
		e.discardKeptAndRecord(stubs, keep)
		return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
	}
	if e.mask != nil {
		masked := e.maskBatch(ctx, stubs)
		if err := ctx.Err(); err != nil {
			e.discardKeptAndRecord(stubs, keep)
			return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
		}
		for index, result := range masked {
			if result == nil {
				keep[index] = false
				internalmedia.DiscardSpan(stubs[index].SpanContext)
				if e.delivery != nil {
					e.delivery.maskedSpanDrops.Add(1)
				}
				continue
			}
			applySpanData(&stubs[index], result)
		}
	}
	if err := ctx.Err(); err != nil {
		e.discardKeptAndRecord(stubs, keep)
		return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
	}

	// Raw typed media is retained out of band. The accepted masked clone carries
	// only opaque tokens, and this is the first point at which upload authority
	// may see the corresponding bytes.
	uploadCtx, cancelUploads := context.WithTimeout(ctx, defaultUploadTimeout)
	mediaFailureSpans := 0
	for index := range stubs {
		if keep[index] {
			if !uploadTypedMedia(uploadCtx, &stubs[index], e.uploads, e.delivery) {
				mediaFailureSpans++
			}
			if err := ctx.Err(); err != nil {
				cancelUploads()
				e.discardKeptAndRecord(stubs, keep)
				return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
			}
		}
	}
	cancelUploads()

	rewritten := make([]trace.ReadOnlySpan, 0, len(stubs))
	for index := range stubs {
		if keep[index] {
			if err := ctx.Err(); err != nil {
				e.recordFailure(countKept(keep))
				return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
			}
			rewritten = append(rewritten, stubs[index].Snapshot())
			if err := ctx.Err(); err != nil {
				e.recordFailure(countKept(keep))
				return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
			}
		}
	}
	if len(rewritten) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		e.recordFailure(len(rewritten))
		return newUploadFailure("prepare", contextReason(ctx), contextRetryable(ctx))
	}
	if err := e.next.ExportSpans(ctx, rewritten); err != nil {
		return err
	}
	if mediaFailureSpans > 0 {
		if e.delivery != nil {
			e.delivery.spanExportFailures.Add(uint64(mediaFailureSpans))
		}
		return newUploadFailure("typed_media", "one_or_more_uploads_failed", false)
	}
	return nil
}

func countKept(keep []bool) int {
	count := 0
	for _, kept := range keep {
		if kept {
			count++
		}
	}
	return count
}

func (e *normalizingExporter) discardAndRecord(spans []trace.ReadOnlySpan, stubs []spanStub) {
	for _, span := range spans {
		if span != nil {
			internalmedia.DiscardSpan(span.SpanContext())
		}
	}
	for index := range stubs {
		internalmedia.DiscardSpan(stubs[index].SpanContext)
	}
	e.recordFailure(len(spans) + len(stubs))
}

func (e *normalizingExporter) discardKeptAndRecord(stubs []spanStub, keep []bool) {
	count := 0
	for index := range stubs {
		if keep[index] {
			internalmedia.DiscardSpan(stubs[index].SpanContext)
			count++
		}
	}
	e.recordFailure(count)
}

func (e *normalizingExporter) recordFailure(count int) {
	if e.delivery != nil && count > 0 {
		e.delivery.spanExportFailures.Add(uint64(count))
	}
}

type maskBatchResult struct {
	index int
	data  *SpanData
	err   error
}

func (e *normalizingExporter) maskBatch(ctx context.Context, stubs []spanStub) []*SpanData {
	e.maskOnce.Do(func() { e.maskSlots = make(chan struct{}, defaultMaskWorkers) })
	maskCtx, cancel := context.WithTimeout(ctx, defaultMaskTimeout)
	defer cancel()
	results := make([]*SpanData, len(stubs))

	for index := range stubs {
		select {
		case e.maskSlots <- struct{}{}:
		case <-maskCtx.Done():
			return results
		}
		data := spanDataFrom(&stubs[index])
		completed := make(chan maskBatchResult, 1)
		go func() {
			defer func() { <-e.maskSlots }()
			masked, err := callMaskSafely(e.mask, maskCtx, data)
			completed <- maskBatchResult{index: index, data: masked, err: err}
		}()
		select {
		case result := <-completed:
			if result.err == nil {
				results[result.index] = result.data
			}
		case <-maskCtx.Done():
			return results
		}
	}
	return results
}

func callMaskSafely(mask MaskFunc, ctx context.Context, data SpanData) (masked *SpanData, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			masked = nil
			err = fmt.Errorf("mask panic: %v", recovered)
		}
	}()
	return mask(ctx, data)
}

func (e *normalizingExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}
