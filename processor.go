package neatlogs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// identityProcessor stamps session + end-user identity onto ROOT spans at start
// time, reading it from the span's start context (where neatlogs.Identify put
// it). As a SpanProcessor it sees every span created on the private Neatlogs
// provider, including spans created by frameworks that accept an injected
// tracer/provider.
//
// Identity is root-only; child spans are skipped (the backend rolls the root's
// value up to the trace and its session).
type identityProcessor struct{}

var _ sdktrace.SpanProcessor = (*identityProcessor)(nil)

func (p *identityProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	if s.Parent().IsValid() {
		return // not a root span
	}
	sessionID, parentSessionID, featureName, entryPoint := sessionIdentityFromContext(parent)
	if sessionID != "" {
		s.SetAttributes(attribute.String(attrs.SessionID, sessionID))
	}
	if parentSessionID != "" {
		s.SetAttributes(attribute.String(attrs.SessionParentID, parentSessionID))
	}
	if featureName != "" {
		s.SetAttributes(attribute.String(attrs.SessionFeatureName, featureName))
	}
	if entryPoint != "" {
		s.SetAttributes(attribute.String(attrs.SessionEntryPoint, entryPoint))
	}
	if id, meta := endUserFromContext(parent); id != "" || meta != nil {
		if id != "" {
			s.SetAttributes(attribute.String(attrs.EndUserID, id))
		}
		if encoded := encodeEndUserMetadata(meta); encoded != "" {
			s.SetAttributes(attribute.String(attrs.EndUserMetadata, encoded))
		}
	}
}

func (p *identityProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (p *identityProcessor) Shutdown(context.Context) error   { return nil }
func (p *identityProcessor) ForceFlush(context.Context) error { return nil }

// completionMarkerName is the internal span the backend uses to finalize a
// trace. The Python and TypeScript SDKs emit the same marker; without it the
// backend receives spans but never surfaces the completed trace.
const completionMarkerName = "neatlogs.trace.complete"

// completionProcessor emits a neatlogs.trace.complete marker span whenever a
// root span (one with no parent) ends. The marker shares the root's trace and
// is parented to it, signalling the backend that the trace is complete.
//
// This runs as a SpanProcessor rather than in the exporter because it must
// create a new span on the same provider; the marker then flows through the
// normalizing exporter like any other span.
type completionProcessor struct {
	tracer trace.Tracer
}

var _ sdktrace.SpanProcessor = (*completionProcessor)(nil)

func (p *completionProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (p *completionProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	// Only root spans complete a trace; skip the marker itself to avoid
	// recursing (the marker is parented to the root, so it is not itself root,
	// but guard by name regardless).
	if s.Parent().HasSpanID() || s.Name() == completionMarkerName || isHTTPReadOnlySpan(s) {
		return
	}

	// Re-parent a new span onto the ending root's context so the marker shares
	// its trace ID and points at the root as parent.
	rootCtx := trace.ContextWithSpanContext(context.Background(), s.SpanContext())
	markerAttrs := []attribute.KeyValue{
		attribute.Bool(completionMarkerName, true),
		attribute.Bool("neatlogs.internal", true),
		attribute.String("neatlogs.span.kind", "Neatlogs.INTERNAL"),
	}
	// The marker may be exported separately from the root. Carry root-owned
	// identity and session metadata so ingestion can still finalize it under
	// the correct session without depending on batch order.
	identityKeys := map[attribute.Key]struct{}{
		attribute.Key(attrs.SessionID):          {},
		attribute.Key(attrs.SessionParentID):    {},
		attribute.Key(attrs.SessionFeatureName): {},
		attribute.Key(attrs.SessionEntryPoint):  {},
		attribute.Key(attrs.EndUserID):          {},
		attribute.Key(attrs.EndUserMetadata):    {},
	}
	for _, kv := range s.Attributes() {
		if _, ok := identityKeys[kv.Key]; ok {
			markerAttrs = append(markerAttrs, kv)
		}
	}
	// Carry forward trace-level tags from the resource, mirroring the TS SDK.
	if res := s.Resource(); res != nil {
		if v, ok := res.Set().Value(attrs.Tags); ok {
			markerAttrs = append(markerAttrs, attribute.String(attrs.Tags, v.AsString()))
		}
	}

	_, marker := p.tracer.Start(rootCtx, completionMarkerName, trace.WithAttributes(markerAttrs...))
	marker.End()
}

func (p *completionProcessor) Shutdown(context.Context) error   { return nil }
func (p *completionProcessor) ForceFlush(context.Context) error { return nil }
