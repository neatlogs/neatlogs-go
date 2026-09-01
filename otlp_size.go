package neatlogs

// This file mirrors the wire conversion used by the OpenTelemetry Go OTLP
// trace exporter for one span. The upstream transformer is an internal package,
// so it cannot be imported by SDK consumers. Keeping the small one-span
// conversion here lets batching use actual protobuf sizes instead of character
// counts or guessed JSON expansion.

import (
	"math"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func encodedSpanUpperBound(span sdktrace.ReadOnlySpan) int {
	if span == nil {
		return 0
	}
	return proto.Size(otlpRequestForSpan(span))
}

func encodeSpanEnvelope(span sdktrace.ReadOnlySpan) ([]byte, error) {
	if span == nil {
		return nil, nil
	}
	return proto.Marshal(otlpRequestForSpan(span))
}

func otlpRequestForSpan(span sdktrace.ReadOnlySpan) *collectortracepb.ExportTraceServiceRequest {
	scope := span.InstrumentationScope()
	return &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource:  &resourcepb.Resource{Attributes: otlpKeyValues(span.Resource().Attributes())},
			SchemaUrl: span.Resource().SchemaURL(),
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{
					Name: scope.Name, Version: scope.Version,
					Attributes: otlpKeyValues(scope.Attributes.ToSlice()),
				},
				SchemaUrl: scope.SchemaURL,
				Spans:     []*tracepb.Span{otlpSpan(span)},
			}},
		}},
	}
}

func otlpSpan(span sdktrace.ReadOnlySpan) *tracepb.Span {
	spanContext := span.SpanContext()
	traceID, spanID := spanContext.TraceID(), spanContext.SpanID()
	result := &tracepb.Span{
		TraceId: traceID[:], SpanId: spanID[:], TraceState: spanContext.TraceState().String(),
		Name: span.Name(), Kind: otlpSpanKind(span.SpanKind()),
		StartTimeUnixNano:      nonNegativeUnixNano(span.StartTime().UnixNano()),
		EndTimeUnixNano:        nonNegativeUnixNano(span.EndTime().UnixNano()),
		Attributes:             otlpKeyValues(span.Attributes()),
		DroppedAttributesCount: clampUint32(span.DroppedAttributes()),
		Events:                 otlpEvents(span.Events()), DroppedEventsCount: clampUint32(span.DroppedEvents()),
		Links: otlpLinks(span.Links()), DroppedLinksCount: clampUint32(span.DroppedLinks()),
		Status: otlpStatus(span.Status()),
		Flags:  otlpFlags(spanContext.TraceFlags(), span.Parent()),
	}
	if parentID := span.Parent().SpanID(); parentID.IsValid() {
		result.ParentSpanId = parentID[:]
	}
	return result
}

func otlpKeyValues(values []attribute.KeyValue) []*commonpb.KeyValue {
	result := make([]*commonpb.KeyValue, 0, len(values))
	for _, value := range values {
		result = append(result, &commonpb.KeyValue{Key: string(value.Key), Value: otlpValue(value.Value)})
	}
	return result
}

func otlpValue(value attribute.Value) *commonpb.AnyValue {
	result := &commonpb.AnyValue{}
	switch value.Type() {
	case attribute.BOOL:
		result.Value = &commonpb.AnyValue_BoolValue{BoolValue: value.AsBool()}
	case attribute.INT64:
		result.Value = &commonpb.AnyValue_IntValue{IntValue: value.AsInt64()}
	case attribute.FLOAT64:
		result.Value = &commonpb.AnyValue_DoubleValue{DoubleValue: value.AsFloat64()}
	case attribute.STRING:
		result.Value = &commonpb.AnyValue_StringValue{StringValue: value.AsString()}
	case attribute.BYTESLICE:
		result.Value = &commonpb.AnyValue_BytesValue{BytesValue: value.AsByteSlice()}
	case attribute.BOOLSLICE:
		result.Value = otlpArray(boolValues(value.AsBoolSlice()))
	case attribute.INT64SLICE:
		result.Value = otlpArray(intValues(value.AsInt64Slice()))
	case attribute.FLOAT64SLICE:
		result.Value = otlpArray(floatValues(value.AsFloat64Slice()))
	case attribute.STRINGSLICE:
		result.Value = otlpArray(stringValues(value.AsStringSlice()))
	case attribute.SLICE:
		items := value.AsSlice()
		converted := make([]*commonpb.AnyValue, len(items))
		for index := range items {
			converted[index] = otlpValue(items[index])
		}
		result.Value = otlpArray(converted)
	default:
		result.Value = &commonpb.AnyValue_StringValue{StringValue: "INVALID"}
	}
	return result
}

func otlpArray(values []*commonpb.AnyValue) *commonpb.AnyValue_ArrayValue {
	return &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: values}}
}
func boolValues(values []bool) []*commonpb.AnyValue {
	result := make([]*commonpb.AnyValue, len(values))
	for i, value := range values {
		result[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}}
	}
	return result
}
func intValues(values []int64) []*commonpb.AnyValue {
	result := make([]*commonpb.AnyValue, len(values))
	for i, value := range values {
		result[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}
	}
	return result
}
func floatValues(values []float64) []*commonpb.AnyValue {
	result := make([]*commonpb.AnyValue, len(values))
	for i, value := range values {
		result[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value}}
	}
	return result
}
func stringValues(values []string) []*commonpb.AnyValue {
	result := make([]*commonpb.AnyValue, len(values))
	for i, value := range values {
		result[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
	}
	return result
}

func otlpEvents(events []sdktrace.Event) []*tracepb.Span_Event {
	result := make([]*tracepb.Span_Event, len(events))
	for index, event := range events {
		result[index] = &tracepb.Span_Event{Name: event.Name, TimeUnixNano: nonNegativeUnixNano(event.Time.UnixNano()), Attributes: otlpKeyValues(event.Attributes), DroppedAttributesCount: clampUint32(event.DroppedAttributeCount)}
	}
	return result
}

func otlpLinks(links []sdktrace.Link) []*tracepb.Span_Link {
	result := make([]*tracepb.Span_Link, len(links))
	for index, link := range links {
		traceID, spanID := link.SpanContext.TraceID(), link.SpanContext.SpanID()
		result[index] = &tracepb.Span_Link{TraceId: traceID[:], SpanId: spanID[:], Attributes: otlpKeyValues(link.Attributes), DroppedAttributesCount: clampUint32(link.DroppedAttributeCount), Flags: otlpFlags(link.SpanContext.TraceFlags(), link.SpanContext)}
	}
	return result
}

func otlpStatus(status sdktrace.Status) *tracepb.Status {
	code := tracepb.Status_STATUS_CODE_UNSET
	if status.Code == codes.Ok {
		code = tracepb.Status_STATUS_CODE_OK
	}
	if status.Code == codes.Error {
		code = tracepb.Status_STATUS_CODE_ERROR
	}
	return &tracepb.Status{Code: code, Message: status.Description}
}

func otlpSpanKind(kind trace.SpanKind) tracepb.Span_SpanKind {
	switch kind {
	case trace.SpanKindInternal:
		return tracepb.Span_SPAN_KIND_INTERNAL
	case trace.SpanKindClient:
		return tracepb.Span_SPAN_KIND_CLIENT
	case trace.SpanKindServer:
		return tracepb.Span_SPAN_KIND_SERVER
	case trace.SpanKindProducer:
		return tracepb.Span_SPAN_KIND_PRODUCER
	case trace.SpanKindConsumer:
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

func otlpFlags(flags trace.TraceFlags, parent trace.SpanContext) uint32 {
	result := uint32(flags) | uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK)
	if parent.IsRemote() {
		result |= uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK)
	}
	return result
}

func clampUint32(value int) uint32 {
	if value < 0 {
		return 0
	}
	if int64(value) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value)
}

func nonNegativeUnixNano(value int64) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value)
}
