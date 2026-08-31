package neatlogs

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SpanData is a mutable, transport-safe clone passed to Config.Mask after
// canonical neatlogs.* normalization and before batching/OTLP serialization.
// Trace/span identity and timing are intentionally immutable.
type SpanData struct {
	Name       string
	Attributes []attribute.KeyValue
	Events     []sdktrace.Event
	Links      []sdktrace.Link
	Resource   []attribute.KeyValue
	Status     sdktrace.Status
}

// MaskFunc transforms a cloned span at the final Neatlogs exporter boundary.
// Return nil to intentionally drop the span. Returning an error also drops the
// span (fail closed); the unmasked original is never exported. The callback
// runs on the OTel batch-export worker and should honor ctx cancellation.
type MaskFunc func(ctx context.Context, span SpanData) (*SpanData, error)

func spanDataFrom(stub *spanStub) SpanData {
	return SpanData{
		Name:       stub.Name,
		Attributes: cloneAttributes(stub.Attributes),
		Events:     cloneEvents(stub.Events),
		Links:      cloneLinks(stub.Links),
		Resource:   cloneAttributes(stub.Resource.Attributes()),
		Status:     stub.Status,
	}
}

func applySpanData(stub *spanStub, data *SpanData) {
	stub.Name = data.Name
	stub.Attributes = cloneAttributes(data.Attributes)
	stub.Events = cloneEvents(data.Events)
	stub.Links = cloneLinks(data.Links)
	stub.Resource = resource.NewWithAttributes(stub.Resource.SchemaURL(), data.Resource...)
	stub.Status = data.Status
}

func cloneAttributes(values []attribute.KeyValue) []attribute.KeyValue {
	return append([]attribute.KeyValue(nil), values...)
}

func cloneEvents(values []sdktrace.Event) []sdktrace.Event {
	cloned := append([]sdktrace.Event(nil), values...)
	for i := range cloned {
		cloned[i].Attributes = cloneAttributes(cloned[i].Attributes)
	}
	return cloned
}

func cloneLinks(values []sdktrace.Link) []sdktrace.Link {
	cloned := append([]sdktrace.Link(nil), values...)
	for i := range cloned {
		cloned[i].Attributes = cloneAttributes(cloned[i].Attributes)
	}
	return cloned
}
