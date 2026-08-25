package neatlogs

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/neatlogs/neatlogs-go/internal/attributes"
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
	next        trace.SpanExporter
	mapper      *attributes.Mapper
	globalMask  MaskFunc
	maskTimeout time.Duration
	masks       *spanMaskRegistry
	health      *exportHealthState
}

var _ trace.SpanExporter = (*normalizingExporter)(nil)

func (e *normalizingExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	rewritten := make([]trace.ReadOnlySpan, 0, len(spans))
	maskFailed := false
	for _, s := range spans {
		stub := cloneSpanStub(s)
		stub.Attributes = e.mapper.Normalize(stub.Attributes)
		mask := e.masks.take(stub.SpanContext.SpanID())
		if mask == nil {
			mask = e.globalMask
		}
		if mask != nil {
			snapshot := maskableFromStub(stub)
			if !runMask(ctx, e.maskTimeout, mask, snapshot) {
				e.health.recordMaskFailure()
				maskFailed = true
				continue
			}
			applyMaskable(&stub, snapshot)
		}
		rewritten = append(rewritten, stub.Snapshot())
	}

	var result error
	if len(rewritten) > 0 {
		if err := e.next.ExportSpans(ctx, rewritten); err != nil {
			e.health.recordExportFailure()
			result = ErrExportFailed
		}
	}
	if maskFailed {
		result = errors.Join(result, ErrMaskingFailed)
	}
	return result
}

func (e *normalizingExporter) Shutdown(ctx context.Context) error {
	if err := e.next.Shutdown(ctx); err != nil {
		e.health.recordExportFailure()
		return ErrExportFailed
	}
	return nil
}

func runMask(parent context.Context, timeout time.Duration, mask MaskFunc, snapshot *MaskableSpan) bool {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- ErrMaskingFailed
			}
		}()
		done <- mask(ctx, snapshot)
	}()
	select {
	case err := <-done:
		return err == nil && validMaskable(snapshot)
	case <-ctx.Done():
		return false
	}
}

func validMaskable(snapshot *MaskableSpan) bool {
	if snapshot == nil || snapshot.Name == "" {
		return false
	}
	if !validAttributes(snapshot.Attributes) {
		return false
	}
	if !validAttributes(snapshot.Scope.Attributes.ToSlice()) {
		return false
	}
	for _, event := range snapshot.Events {
		if !validAttributes(event.Attributes) {
			return false
		}
	}
	for _, link := range snapshot.Links {
		if !validAttributes(link.Attributes) {
			return false
		}
	}
	return snapshot.Resource == nil || validAttributes(snapshot.Resource.Attributes())
}

func validAttributes(values []attribute.KeyValue) bool {
	for _, value := range values {
		if value.Key == "" || value.Value.Type() == attribute.INVALID {
			return false
		}
	}
	return true
}

func cloneSpanStub(span trace.ReadOnlySpan) tracetest.SpanStub {
	stub := tracetest.SpanStubFromReadOnlySpan(span)
	stub.Attributes = cloneAttributes(stub.Attributes)
	stub.Events = cloneEvents(stub.Events)
	stub.Links = cloneLinks(stub.Links)
	stub.Resource = cloneResource(stub.Resource)
	stub.InstrumentationScope = cloneScope(stub.InstrumentationScope)
	stub.InstrumentationLibrary = stub.InstrumentationScope
	return stub
}

func maskableFromStub(stub tracetest.SpanStub) *MaskableSpan {
	return &MaskableSpan{
		Name:       stub.Name,
		Attributes: cloneAttributes(stub.Attributes),
		Events:     cloneEvents(stub.Events),
		Links:      cloneLinks(stub.Links),
		Status:     stub.Status,
		Resource:   cloneResource(stub.Resource),
		Scope:      cloneScope(stub.InstrumentationScope),
	}
}

func applyMaskable(stub *tracetest.SpanStub, snapshot *MaskableSpan) {
	stub.Name = snapshot.Name
	stub.Attributes = cloneAttributes(snapshot.Attributes)
	stub.Events = cloneEvents(snapshot.Events)
	stub.Links = cloneLinks(snapshot.Links)
	stub.Status = snapshot.Status
	stub.Resource = cloneResource(snapshot.Resource)
	stub.InstrumentationScope = cloneScope(snapshot.Scope)
	stub.InstrumentationLibrary = stub.InstrumentationScope
}

func cloneEvents(events []trace.Event) []trace.Event {
	cloned := make([]trace.Event, len(events))
	copy(cloned, events)
	for i := range cloned {
		cloned[i].Attributes = cloneAttributes(events[i].Attributes)
	}
	return cloned
}

func cloneLinks(links []trace.Link) []trace.Link {
	cloned := make([]trace.Link, len(links))
	copy(cloned, links)
	for i := range cloned {
		cloned[i].Attributes = cloneAttributes(links[i].Attributes)
	}
	return cloned
}

func cloneResource(input *resource.Resource) *resource.Resource {
	if input == nil {
		return nil
	}
	return resource.NewWithAttributes(input.SchemaURL(), cloneAttributes(input.Attributes())...)
}

func cloneScope(input instrumentation.Scope) instrumentation.Scope {
	return instrumentation.Scope{
		Name: input.Name, Version: input.Version, SchemaURL: input.SchemaURL,
		Attributes: attribute.NewSet(cloneAttributes(input.Attributes.ToSlice())...),
	}
}

func cloneAttributes(input []attribute.KeyValue) []attribute.KeyValue {
	cloned := make([]attribute.KeyValue, 0, len(input))
	for _, kv := range input {
		cloned = append(cloned, cloneAttribute(kv))
	}
	return cloned
}

func cloneAttribute(kv attribute.KeyValue) attribute.KeyValue {
	key := string(kv.Key)
	switch kv.Value.Type() {
	case attribute.BOOL:
		return attribute.Bool(key, kv.Value.AsBool())
	case attribute.INT64:
		return attribute.Int64(key, kv.Value.AsInt64())
	case attribute.FLOAT64:
		return attribute.Float64(key, kv.Value.AsFloat64())
	case attribute.STRING:
		return attribute.String(key, kv.Value.AsString())
	case attribute.BOOLSLICE:
		return attribute.BoolSlice(key, append([]bool(nil), kv.Value.AsBoolSlice()...))
	case attribute.INT64SLICE:
		return attribute.Int64Slice(key, append([]int64(nil), kv.Value.AsInt64Slice()...))
	case attribute.FLOAT64SLICE:
		return attribute.Float64Slice(key, append([]float64(nil), kv.Value.AsFloat64Slice()...))
	case attribute.STRINGSLICE:
		return attribute.StringSlice(key, append([]string(nil), kv.Value.AsStringSlice()...))
	default:
		return kv
	}
}
