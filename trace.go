package neatlogs

import (
	"context"
	"encoding/json"
	"fmt"

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

// SetTraceOutput records the application-declared final result on the root span
// returned by Trace. Structured values are JSON encoded so the backend receives
// the same typed root-output representation regardless of Go value type.
//
// Call this before the root's end function:
//
//	_, root, end := neatlogs.Trace(ctx, "chat_turn")
//	defer end()
//	if err := neatlogs.SetTraceOutput(root, result); err != nil { ... }
func SetTraceOutput(span trace.Span, value any) error {
	if span == nil {
		return fmt.Errorf("neatlogs: trace span is nil")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("neatlogs: encode trace output: %w", err)
	}
	span.SetAttributes(attribute.String(attrs.TraceOutput, string(encoded)))
	return nil
}

// SetTraceInput records the application-declared input on a WORKFLOW root.
// It uses the canonical neatlogs.input.value key consumed by every ingest path.
// Call it before ending the span returned by Trace.
func SetTraceInput(span trace.Span, value any) error {
	if span == nil {
		return fmt.Errorf("neatlogs: trace span is nil")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("neatlogs: encode trace input: %w", err)
	}
	span.SetAttributes(attribute.String(attrs.Input, string(encoded)))
	return nil
}

// StartSpan starts an explicitly typed Neatlogs span on the Client bound to ctx,
// or on the process-wide Init provider when no Client is bound.
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
	return startSpanForContext(ctx, name, trace.WithAttributes(attributes...))
}
