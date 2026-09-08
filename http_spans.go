package neatlogs

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

var httpScopePrefixes = []string{
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
	"go.opentelemetry.io/contrib/instrumentation/net/http/httptrace",
}

var httpAttributeKeys = map[string]struct{}{
	"http.method":         {},
	"http.request.method": {},
	"http.url":            {},
	"http.route":          {},
	"url.full":            {},
}

var semanticSpanKinds = map[string]struct{}{
	"WORKFLOW":     {},
	"AGENT":        {},
	"CHAIN":        {},
	"TOOL":         {},
	"RETRIEVER":    {},
	"EMBEDDING":    {},
	"GUARDRAIL":    {},
	"LLM":          {},
	"RERANKER":     {},
	"VECTOR_STORE": {},
	"TASK":         {},
	"EVALUATOR":    {},
	"LOG":          {},
	"MEMORY":       {},
	"MCP_TOOL":     {},
}

func isHTTPKind(kind string) bool {
	return strings.EqualFold(strings.TrimSpace(kind), "HTTP")
}

func isHTTPSpanAttributes(
	attributes []attribute.KeyValue,
	spanKind trace.SpanKind,
	scope instrumentation.Scope,
) bool {
	neatlogsKind := ""
	openInferenceKind := ""
	traceloopKind := ""
	hasHTTPAttribute := false
	for _, item := range attributes {
		key := string(item.Key)
		if key == "neatlogs.span.kind" {
			neatlogsKind = strings.ToUpper(strings.TrimSpace(item.Value.AsString()))
		}
		if key == "openinference.span.kind" {
			openInferenceKind = strings.ToUpper(strings.TrimSpace(item.Value.AsString()))
		}
		if key == "traceloop.span.kind" {
			traceloopKind = strings.ToUpper(strings.TrimSpace(item.Value.AsString()))
		}
		if _, ok := httpAttributeKeys[key]; ok {
			hasHTTPAttribute = true
		}
	}
	resolvedKind := strings.ToUpper(attrs.ResolveCanonicalSpanKind(
		neatlogsKind,
		openInferenceKind,
		traceloopKind,
	))
	if resolvedKind == "HTTP" {
		return true
	}
	if _, ok := semanticSpanKinds[resolvedKind]; ok {
		return false
	}
	for _, prefix := range httpScopePrefixes {
		if strings.HasPrefix(scope.Name, prefix) {
			return true
		}
	}
	return spanKind == trace.SpanKindClient && hasHTTPAttribute
}

func isHTTPReadOnlySpan(span sdktrace.ReadOnlySpan) bool {
	return span != nil && isHTTPSpanAttributes(
		span.Attributes(),
		span.SpanKind(),
		span.InstrumentationScope(),
	)
}
