package neatlogs

import (
	"context"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Private W3C propagator used only when the application explicitly asks
// Neatlogs to cross a process boundary. The process-global propagator is never
// read or replaced.
var privatePropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{},
	propagation.Baggage{},
)

type privateTraceContextKey struct{}

type privateTraceContext struct {
	spanContext trace.SpanContext
	owner       *sdkRuntime
}

// withPrivateTraceContext carries the Neatlogs parent outside the standard OTel
// active-span slot. Other OTel instrumentation therefore cannot see or parent
// from Neatlogs, while deadlines, cancellation, and application values remain
// on the original context.
func withPrivateTraceContext(ctx context.Context, spanContext trace.SpanContext, owner *sdkRuntime) context.Context {
	return context.WithValue(ctx, privateTraceContextKey{}, privateTraceContext{
		spanContext: spanContext,
		owner:       owner,
	})
}

func privateSpanContext(ctx context.Context) trace.SpanContext {
	binding, _ := ctx.Value(privateTraceContextKey{}).(privateTraceContext)
	return binding.spanContext
}

// privateSpanContextFor returns a parent only when it belongs to the selected
// runtime. Extracted remote parents have no local owner and may be continued by
// whichever global or context-scoped Client handles the request.
func privateSpanContextFor(ctx context.Context, owner *sdkRuntime) trace.SpanContext {
	binding, _ := ctx.Value(privateTraceContextKey{}).(privateTraceContext)
	if binding.owner != nil && binding.owner != owner {
		return trace.SpanContext{}
	}
	return binding.spanContext
}

// privateStartContext builds the temporary OTel context used only while
// starting a Neatlogs span. It clears a foreign active span, then installs the
// private Neatlogs parent (when one exists).
func privateStartContext(ctx context.Context, owner *sdkRuntime) context.Context {
	startCtx := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	if parent := privateSpanContextFor(ctx, owner); parent.IsValid() {
		startCtx = trace.ContextWithSpanContext(startCtx, parent)
	}
	return startCtx
}

// InjectTraceContext writes the Neatlogs span carried by ctx into carrier.
func InjectTraceContext(ctx context.Context, carrier propagation.TextMapCarrier) {
	if !privateSpanContext(ctx).IsValid() {
		return
	}
	privatePropagator.Inject(trace.ContextWithSpanContext(ctx, privateSpanContext(ctx)), carrier)
}

// ExtractTraceContext returns a context containing the remote Neatlogs parent
// found in carrier. It does not activate or alter process-global OTel context.
func ExtractTraceContext(
	ctx context.Context,
	carrier propagation.TextMapCarrier,
) context.Context {
	extracted := privatePropagator.Extract(
		trace.ContextWithSpanContext(ctx, trace.SpanContext{}),
		carrier,
	)
	if extractedBaggage := baggage.FromContext(extracted); extractedBaggage.Len() > 0 {
		ctx = baggage.ContextWithBaggage(ctx, extractedBaggage)
	}
	return withPrivateTraceContext(ctx, trace.SpanContextFromContext(extracted), nil)
}
