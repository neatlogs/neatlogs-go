package neatlogs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// Trace opens a WORKFLOW root span named `name` and returns the child context,
// the span, and an end func the caller must invoke exactly once (usually via
// defer). It is the Go equivalent of Python's `with neatlogs.trace(...)` and
// TypeScript's `neatlogs.trace({...}, fn)`.
//
// Session and end-user identity are NOT passed as arguments here — they ride on
// the context. Set them once at the request/turn boundary with Identify, then
// pass that context in:
//
//	ctx = neatlogs.Identify(ctx, neatlogs.IdentifyOptions{
//	    SessionID: "chat_123", EndUserID: "user_456",
//	    EndUserMetadata: map[string]any{"plan": "pro"},
//	})
//	ctx, span, end := neatlogs.Trace(ctx, "chat_turn") // reads identity from ctx
//	defer end()
//
// Identity (session + end-user bound via Identify) is stamped on the root span
// by the identityProcessor, which reads it from the span's start context — so it
// applies to this WORKFLOW root, the WrapGenAI auto-root, and spans created by
// integrations using the private provider. Trace itself only opens the span.
func Trace(ctx context.Context, name string) (context.Context, trace.Span, func()) {
	return StartSpan(ctx, name, attrs.KindWorkflow)
}

// StartSpan starts an explicitly typed Neatlogs span on the private provider.
// Use it at framework and service boundaries where Trace's WORKFLOW kind is not
// appropriate (for example a TOOL child extracted from an incoming trace).
func StartSpan(
	ctx context.Context,
	name string,
	kind string,
	attributes ...attribute.KeyValue,
) (context.Context, trace.Span, func()) {
	attributes = append([]attribute.KeyValue{
		attribute.String(attrs.SpanKind, kind),
	}, attributes...)
	_, span := tracer().Start(
		privateStartContext(ctx),
		name,
		trace.WithAttributes(attributes...),
	)
	return withPrivateTraceContext(ctx, span.SpanContext()), span, func() { span.End() }
}
