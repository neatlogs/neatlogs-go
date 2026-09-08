package neatlogs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

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
