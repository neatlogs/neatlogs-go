package neatlogs

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// ToolSpan is a handle to a TOOL span opened by StartToolSpanFromHeaders. Record
// the result with SetOutput or SetError, then call End exactly once (via defer).
type ToolSpan struct {
	span trace.Span
	end  func()
}

// StartToolSpanFromHeaders continues the Neatlogs trace carried on an incoming
// request's headers (injected by an upstream Neatlogs SDK) and opens a TOOL
// child span named toolName, binding the given identity and recording input.
//
// It exists so a service boundary can be instrumented with Neatlogs alone:
// header extraction, the private W3C propagator, span-kind and tool.* attribute
// keys, and error status are all handled here, so the caller imports only this
// package — no OpenTelemetry types. It uses the private Neatlogs propagator and
// provider only; the process-global OTel / Datadog context is never read or
// mutated. When Neatlogs is not initialized the span is a no-op and this just
// returns a wrapped context.
//
// Call End on the returned ToolSpan exactly once, usually via defer.
func StartToolSpanFromHeaders(
	ctx context.Context,
	headers http.Header,
	toolName string,
	input string,
	opts IdentifyOptions,
) (context.Context, *ToolSpan) {
	ctx = ExtractTraceContext(ctx, propagation.HeaderCarrier(headers))
	ctx = Identify(ctx, opts)
	ctx, span, end := StartProviderSpan(ctx, toolName, attrs.KindTool)
	span.SetAttributes(
		attribute.String(attrs.SpanKind, attrs.KindTool),
		attribute.String(attrs.ToolName, toolName),
		attribute.String(attrs.ToolInput, input),
	)
	return ctx, &ToolSpan{span: span, end: end}
}

// SetOutput records the tool's output payload on the span.
func (t *ToolSpan) SetOutput(output string) {
	if t == nil {
		return
	}
	t.span.SetAttributes(attribute.String(attrs.ToolOutput, output))
}

// SetError marks the span failed and records err as the tool output.
func (t *ToolSpan) SetError(err error) {
	if t == nil || err == nil {
		return
	}
	t.span.SetStatus(codes.Error, err.Error())
	t.span.SetAttributes(attribute.String(attrs.ToolOutput, err.Error()))
}

// End closes the span. Call exactly once, usually via defer.
func (t *ToolSpan) End() {
	if t == nil {
		return
	}
	t.end()
}
