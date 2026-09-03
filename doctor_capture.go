package neatlogs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// DoctorEnvelope is the language-neutral, privacy-safe semantic projection.
// It is produced only from normalized snapshots that have passed masking.
type DoctorEnvelope struct {
	TraceID    string       `json:"trace_id"`
	RootSpanID *string      `json:"root_span_id"`
	Spans      []DoctorSpan `json:"spans"`
}

type DoctorSpan struct {
	SpanID              string         `json:"span_id"`
	ParentSpanID        *string        `json:"parent_span_id"`
	Name                string         `json:"name"`
	Kind                string         `json:"kind,omitempty"`
	Status              string         `json:"status"`
	Input               any            `json:"input,omitempty"`
	Output              any            `json:"output,omitempty"`
	Choices             any            `json:"choices,omitempty"`
	ExpectedChoiceCount *int           `json:"expected_choice_count,omitempty"`
	ToolCall            any            `json:"tool_call,omitempty"`
	StreamFragments     any            `json:"stream_fragments,omitempty"`
	Streaming           bool           `json:"streaming,omitempty"`
	Oversized           bool           `json:"oversized,omitempty"`
	PayloadReferences   any            `json:"payload_references,omitempty"`
	StartTimeNS         *int64         `json:"start_time_ns,omitempty"`
	DurationNS          *int64         `json:"duration_ns,omitempty"`
	Sampled             bool           `json:"sampled"`
	Ended               bool           `json:"ended"`
	Attributes          map[string]any `json:"attributes,omitempty"`
}

type doctorCaptureStore struct {
	mu               sync.RWMutex
	capacity         int
	maxSpansPerTrace int
	maxBytesPerTrace int
	maxBytesTotal    int
	totalBytes       int
	order            []string
	byTrace          map[string]map[string]DoctorSpan
	traceBytes       map[string]int
}

func newDoctorCaptureStore(capacity int) *doctorCaptureStore {
	return &doctorCaptureStore{
		capacity: capacity, maxSpansPerTrace: 64, maxBytesPerTrace: 256 * 1024,
		maxBytesTotal: 1024 * 1024, byTrace: make(map[string]map[string]DoctorSpan),
		traceBytes: make(map[string]int),
	}
}

func (s *doctorCaptureStore) capture(spans []sdktrace.ReadOnlySpan) {
	if s == nil {
		return
	}
	type preparedSpan struct {
		traceID, spanID string
		span            DoctorSpan
		size            int
	}
	prepared := make([]preparedSpan, 0, len(spans))
	for _, span := range spans {
		if span.Name() == completionMarkerName {
			continue
		}
		item := doctorSpanFrom(span)
		encoded, err := json.Marshal(item)
		if err != nil || len(encoded) > s.maxBytesPerTrace {
			continue
		}
		prepared = append(prepared, preparedSpan{
			traceID: span.SpanContext().TraceID().String(),
			spanID:  span.SpanContext().SpanID().String(),
			span:    item, size: len(encoded),
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range prepared {
		traceID, spanID := item.traceID, item.spanID
		if _, exists := s.byTrace[traceID]; !exists {
			if len(s.order) == s.capacity {
				s.evictLocked(s.order[0])
				s.order = s.order[1:]
			}
			s.order = append(s.order, traceID)
			s.byTrace[traceID] = make(map[string]DoctorSpan)
		}
		previous, exists := s.byTrace[traceID][spanID]
		if !exists && len(s.byTrace[traceID]) >= s.maxSpansPerTrace {
			continue
		}
		previousSize := 0
		if exists {
			encoded, _ := json.Marshal(previous)
			previousSize = len(encoded)
		}
		nextTraceBytes := s.traceBytes[traceID] - previousSize + item.size
		if nextTraceBytes > s.maxBytesPerTrace {
			continue
		}
		s.byTrace[traceID][spanID] = item.span
		s.traceBytes[traceID] = nextTraceBytes
		s.totalBytes += item.size - previousSize
		for s.totalBytes > s.maxBytesTotal && len(s.order) > 1 {
			evicted := s.order[0]
			s.order = s.order[1:]
			s.evictLocked(evicted)
		}
	}
}

func (s *doctorCaptureStore) evictLocked(traceID string) {
	s.totalBytes -= s.traceBytes[traceID]
	delete(s.traceBytes, traceID)
	delete(s.byTrace, traceID)
}

func (s *doctorCaptureStore) clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.order = nil
	s.byTrace = make(map[string]map[string]DoctorSpan)
	s.traceBytes = make(map[string]int)
	s.totalBytes = 0
	s.mu.Unlock()
}

func (s *doctorCaptureStore) envelope(traceID string) (DoctorEnvelope, bool) {
	if s == nil {
		return DoctorEnvelope{}, false
	}
	s.mu.RLock()
	spansByID, ok := s.byTrace[strings.ToLower(traceID)]
	if !ok {
		s.mu.RUnlock()
		return DoctorEnvelope{}, false
	}
	spans := make([]DoctorSpan, 0, len(spansByID))
	for _, span := range spansByID {
		spans = append(spans, span)
	}
	s.mu.RUnlock()
	sort.Slice(spans, func(i, j int) bool { return spans[i].SpanID < spans[j].SpanID })
	var root *string
	for i := range spans {
		if spans[i].ParentSpanID == nil && spans[i].Name != completionMarkerName {
			id := spans[i].SpanID
			root = &id
			break
		}
	}
	return DoctorEnvelope{TraceID: strings.ToLower(traceID), RootSpanID: root, Spans: spans}, true
}

func doctorSpanFrom(span sdktrace.ReadOnlySpan) DoctorSpan {
	var parent *string
	if span.Parent().SpanID().IsValid() {
		value := span.Parent().SpanID().String()
		parent = &value
	}
	values := make(map[string]any)
	for _, kv := range span.Attributes() {
		values[string(kv.Key)] = doctorAttributeValue(kv.Value)
	}
	kind, _ := values["neatlogs.span.kind"].(string)
	input := doctorJSONValue(values["neatlogs.input.value"])
	output := doctorJSONValue(values["neatlogs.output.value"])
	delete(values, "neatlogs.input.value")
	delete(values, "neatlogs.output.value")
	delete(values, "neatlogs.span.kind")
	status := strings.ToUpper(span.Status().Code.String())
	if status == "UNSET" {
		status = "OK"
	}
	return DoctorSpan{SpanID: span.SpanContext().SpanID().String(), ParentSpanID: parent,
		Name: span.Name(), Kind: strings.ToUpper(strings.TrimPrefix(kind, "Neatlogs.")), Status: status,
		Input: input, Output: output, Sampled: span.SpanContext().IsSampled(), Ended: !span.EndTime().IsZero(), Attributes: values}
}

func doctorJSONValue(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return text
}

func doctorAttributeValue(value attribute.Value) any {
	switch value.Type() {
	case attribute.BOOL:
		return value.AsBool()
	case attribute.INT64:
		return value.AsInt64()
	case attribute.FLOAT64:
		return value.AsFloat64()
	case attribute.STRING:
		return value.AsString()
	case attribute.BOOLSLICE:
		return value.AsBoolSlice()
	case attribute.INT64SLICE:
		return value.AsInt64Slice()
	case attribute.FLOAT64SLICE:
		return value.AsFloat64Slice()
	case attribute.STRINGSLICE:
		return value.AsStringSlice()
	default:
		return nil
	}
}

func semanticDigest(envelope DoctorEnvelope) (string, error) {
	namesByID := make(map[string]string, len(envelope.Spans))
	for _, span := range envelope.Spans {
		namesByID[span.SpanID] = span.Name
	}
	projection := make([]map[string]any, 0, len(envelope.Spans))
	for _, span := range envelope.Spans {
		if span.Name == completionMarkerName {
			continue
		}
		item := map[string]any{
			"name": span.Name, "kind": span.Kind, "status": span.Status,
			"sampled": span.Sampled, "ended": span.Ended,
		}
		if span.ParentSpanID == nil {
			item["parent"] = nil
		} else {
			item["parent"] = namesByID[*span.ParentSpanID]
		}
		optional := map[string]any{
			"input": span.Input, "output": span.Output, "choices": span.Choices,
			"tool_call":          span.ToolCall,
			"payload_references": span.PayloadReferences,
		}
		for key, value := range optional {
			if value != nil {
				item[key] = value
			}
		}
		if span.ExpectedChoiceCount != nil {
			item["expected_choice_count"] = *span.ExpectedChoiceCount
		}
		if span.Streaming {
			item["streaming"] = true
		}
		if span.Oversized {
			item["oversized"] = true
		}
		projection = append(projection, item)
	}
	sort.Slice(projection, func(i, j int) bool {
		left, right := projection[i], projection[j]
		return left["name"].(string) < right["name"].(string) ||
			(left["name"] == right["name"] && left["kind"].(string) < right["kind"].(string))
	})
	encoded, err := json.Marshal(map[string]any{"spans": projection})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// DoctorSemanticDigestV2 exposes the contract digest for golden fixtures and
// adapters. Callers must provide an already normalized and masked envelope.
func DoctorSemanticDigestV2(envelope DoctorEnvelope) (string, error) { return semanticDigest(envelope) }
