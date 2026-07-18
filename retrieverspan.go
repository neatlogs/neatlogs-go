package neatlogs

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// RetrieverSpan is a handle to a retrieval / semantic-memory span opened by
// StartRetrieverSpan. Record results with SetDocuments/SetDocumentCount (or
// SetError), then call End exactly once (via defer).
//
// Retrieval-shaped operations — vector search, RAG document lookup, and agent
// memory recall — all use span kind "retriever"; there is no separate "memory"
// kind in the canonical mapping.
type RetrieverSpan struct {
	span trace.Span
	end  func()
}

// StartRetrieverSpan opens a RETRIEVER span for a retrieval / memory operation
// named name, recording the query and (optional) top-k. It auto-roots when the
// context has no active parent and nests under an existing parent otherwise
// (e.g. a TOOL span continued from an upstream service). Uses the private
// Neatlogs provider only; when Neatlogs is not initialized the span is a no-op.
//
// Call End on the returned RetrieverSpan exactly once, usually via defer.
func StartRetrieverSpan(ctx context.Context, name, query string, topK int) (context.Context, *RetrieverSpan) {
	ctx, span, end := StartProviderSpan(ctx, name, attrs.KindRetriever)
	span.SetAttributes(attribute.String(attrs.SpanKind, attrs.KindRetriever))
	if query != "" {
		span.SetAttributes(attribute.String(attrs.RetrieverQuery, query))
	}
	if topK != 0 {
		span.SetAttributes(attribute.Int(attrs.RetrieverTopK, topK))
	}
	return ctx, &RetrieverSpan{span: span, end: end}
}

// SetDocuments records the retrieved documents (JSON-encoded) and their count.
// It also writes the same JSON to the generic output field so every completed
// retrieval, including an empty result set, has an explicit I/O output.
func (r *RetrieverSpan) SetDocuments(documents any, count int) {
	if r == nil {
		return
	}

	encodedDocuments := "[]"
	if documents != nil {
		if encoded := jsonString(documents); encoded != "" && encoded != "null" {
			encodedDocuments = encoded
		}
	}
	r.span.SetAttributes(
		attribute.String(attrs.RetrieverDocuments, encodedDocuments),
		attribute.String(attrs.Output, encodedDocuments),
	)
	r.SetDocumentCount(count)
}

// SetDocumentCount records how many documents the retrieval returned.
func (r *RetrieverSpan) SetDocumentCount(count int) {
	if r == nil {
		return
	}
	r.span.SetAttributes(attribute.Int(attrs.DocumentsCount, count))
}

// SetError marks the span failed and records err.
func (r *RetrieverSpan) SetError(err error) {
	if r == nil || err == nil {
		return
	}
	r.span.RecordError(err)
	r.span.SetStatus(codes.Error, err.Error())
}

// End closes the span (and its auto-root, if one was opened). Call exactly once.
func (r *RetrieverSpan) End() {
	if r == nil {
		return
	}
	r.end()
}

// jsonString marshals v to a JSON string, returning "" on failure. Shared by
// the span helpers for encoding structured attribute values.
func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
