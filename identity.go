package neatlogs

import (
	"context"
	"encoding/json"
)

// Request-scoped session & end-user identity.
//
// Session and end-user identity are PER-REQUEST, not process-global, so they are
// never set on Init. Bind them on the context with Identify, then pass that
// context into Trace and/or your wrapped calls — the trace root (including the
// auto-root opened by WrapGenAI / WrapModel) reads identity from the context:
//
//	ctx = neatlogs.Identify(ctx, neatlogs.IdentifyOptions{
//	    SessionID: "chat_123", EndUserID: "user_456",
//	    EndUserMetadata: map[string]any{"plan": "pro"},
//	})
//	ctx, span, end := neatlogs.Trace(ctx, "chat_turn") // reads identity from ctx
//	gc.GenerateContent(ctx, ...)                        // wrapper auto-root too
//
// This is the Go equivalent of Python's `with neatlogs.identify(...)` and
// TypeScript's `neatlogs.identify({...}, fn)`. It is backed by context.Context,
// so it propagates through the call tree (and across goroutines that carry the
// context), exactly like OpenTelemetry's own context.
//
// Identity is stamped on the ROOT span only; the backend rolls it up to the
// trace and its session. To change identity for a later turn, call Identify
// again with new values — it returns a fresh derived context.

type contextKey int

const (
	sessionIDKey contextKey = iota
	endUserIDKey
	endUserMetadataKey
)

// IdentifyOptions carries the request-scoped identity for Identify. All fields
// are optional; only set ones are bound onto the context.
type IdentifyOptions struct {
	SessionID       string
	EndUserID       string
	EndUserMetadata map[string]any
}

// Identify returns a copy of ctx carrying the given session + end-user identity.
// It is the single entry point for session/end-user on the Go SDK. Pass the
// returned context into Trace and/or your wrapped LLM calls; the (auto-)root
// span picks the identity up. Only set fields are bound, so calling Identify
// again overrides individual fields without clearing the others.
func Identify(ctx context.Context, opts IdentifyOptions) context.Context {
	if opts.SessionID != "" {
		ctx = context.WithValue(ctx, sessionIDKey, opts.SessionID)
	}
	if opts.EndUserID != "" {
		ctx = context.WithValue(ctx, endUserIDKey, opts.EndUserID)
	}
	if opts.EndUserMetadata != nil {
		ctx = context.WithValue(ctx, endUserMetadataKey, opts.EndUserMetadata)
	}
	return ctx
}

// sessionIDFromContext returns the session id bound to ctx, or "" if none.
func sessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey).(string); ok {
		return v
	}
	return ""
}

// endUserFromContext returns the end-user id and metadata bound to ctx. Either
// may be zero-valued when unset.
func endUserFromContext(ctx context.Context) (string, map[string]any) {
	id, _ := ctx.Value(endUserIDKey).(string)
	meta, _ := ctx.Value(endUserMetadataKey).(map[string]any)
	return id, meta
}

// encodeEndUserMetadata JSON-encodes end-user metadata to a string for the span
// attribute. Returns "" for nil/empty maps or on marshal failure.
func encodeEndUserMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(b)
}
