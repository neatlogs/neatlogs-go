package neatlogs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
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
	ToolCalls           any            `json:"tool_calls,omitempty"`
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
	spanBytes        map[string]map[string]int
}

func newDoctorCaptureStore(capacity int) *doctorCaptureStore {
	return &doctorCaptureStore{
		capacity: capacity, maxSpansPerTrace: 64, maxBytesPerTrace: 256 * 1024,
		maxBytesTotal: 1024 * 1024, byTrace: make(map[string]map[string]DoctorSpan),
		traceBytes: make(map[string]int), spanBytes: make(map[string]map[string]int),
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
			s.spanBytes[traceID] = make(map[string]int)
		}
		_, exists := s.byTrace[traceID][spanID]
		if !exists && len(s.byTrace[traceID]) >= s.maxSpansPerTrace {
			continue
		}
		previousSize := s.spanBytes[traceID][spanID]
		nextTraceBytes := s.traceBytes[traceID] - previousSize + item.size
		if nextTraceBytes > s.maxBytesPerTrace {
			continue
		}
		s.byTrace[traceID][spanID] = item.span
		s.spanBytes[traceID][spanID] = item.size
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
	delete(s.spanBytes, traceID)
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
	s.spanBytes = make(map[string]map[string]int)
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
	choices, expectedChoiceCount := doctorChoices(values)
	toolCalls := doctorToolCalls(values)
	streamFragments := doctorStreamFragments(span)
	toolCall := doctorToolCall(values, strings.EqualFold(strings.TrimPrefix(kind, "Neatlogs."), "tool"))
	payloadReferences := doctorPayloadReferences(values)
	streaming, _ := values["neatlogs.llm.is_streaming"].(bool)
	if doctorCollectionLength(streamFragments) > 0 {
		streaming = true
	}
	oversized, _ := values["neatlogs.capture.truncated"].(bool)
	delete(values, "neatlogs.input.value")
	delete(values, "neatlogs.output.value")
	delete(values, "neatlogs.span.kind")
	status := strings.ToUpper(span.Status().Code.String())
	if status == "UNSET" {
		status = "OK"
	}
	return DoctorSpan{SpanID: span.SpanContext().SpanID().String(), ParentSpanID: parent,
		Name: span.Name(), Kind: strings.ToUpper(strings.TrimPrefix(kind, "Neatlogs.")), Status: status,
		Input: input, Output: output, Choices: choices, ExpectedChoiceCount: expectedChoiceCount,
		ToolCalls: toolCalls,
		ToolCall:  toolCall, StreamFragments: streamFragments, Streaming: streaming,
		Oversized: oversized, PayloadReferences: payloadReferences,
		Sampled: span.SpanContext().IsSampled(), Ended: !span.EndTime().IsZero(), Attributes: values}
}

func doctorChoices(values map[string]any) (any, *int) {
	expected, hasExpected := doctorInteger(values["neatlogs.llm.generation_choices"])
	indexes := make(map[int]bool)
	messages := make(map[int]map[string]any)
	finishes := make(map[int]any)
	for key, value := range values {
		if index, field, ok := doctorIndexedField(key, "neatlogs.llm.output_messages."); ok {
			indexes[index] = true
			if messages[index] == nil {
				messages[index] = make(map[string]any)
			}
			messages[index][field] = doctorJSONValue(value)
			continue
		}
		if index, field, ok := doctorIndexedField(key, "neatlogs.llm.choices."); ok && field == "finish_reason" {
			indexes[index] = true
			finishes[index] = doctorJSONValue(value)
		}
	}
	ordered := make([]int, 0, len(indexes))
	for index := range indexes {
		ordered = append(ordered, index)
	}
	sort.Ints(ordered)
	choices := make([]map[string]any, 0, len(ordered))
	for _, index := range ordered {
		message := messages[index]
		if message == nil {
			message = make(map[string]any)
		}
		choice := map[string]any{"index": index, "message": message}
		if finish, ok := finishes[index]; ok {
			choice["finish_reason"] = finish
		}
		choices = append(choices, choice)
	}
	if !hasExpected {
		if len(choices) == 0 {
			return nil, nil
		}
		expected = len(choices)
	}
	return choicesOrNil(choices), &expected
}

func doctorToolCalls(values map[string]any) any {
	indexed := make(map[int]map[string]any)
	for key, value := range values {
		index, field, ok := doctorIndexedField(key, "neatlogs.llm.tool_calls.")
		if !ok {
			continue
		}
		if indexed[index] == nil {
			indexed[index] = make(map[string]any)
		}
		indexed[index][field] = doctorJSONValue(value)
	}
	indexes := make([]int, 0, len(indexed))
	for index := range indexed {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		call := indexed[index]
		if _, ok := call["id"].(string); ok {
			calls = append(calls, call)
		}
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

func choicesOrNil(choices []map[string]any) any {
	if len(choices) == 0 {
		return nil
	}
	return choices
}

func doctorIndexedField(key, prefix string) (int, string, bool) {
	if !strings.HasPrefix(key, prefix) {
		return 0, "", false
	}
	remainder := strings.TrimPrefix(key, prefix)
	parts := strings.SplitN(remainder, ".", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	index := 0
	for _, char := range parts[0] {
		if char < '0' || char > '9' {
			return 0, "", false
		}
		index = index*10 + int(char-'0')
	}
	return index, parts[1], parts[0] != ""
}

func doctorInteger(value any) (int, bool) {
	switch item := value.(type) {
	case int:
		return item, true
	case int64:
		return int(item), true
	case float64:
		return int(item), item == float64(int(item))
	default:
		return 0, false
	}
}

func doctorStreamFragments(span sdktrace.ReadOnlySpan) any {
	fragments := make([]any, 0)
	for _, event := range span.Events() {
		if event.Name != "neatlogs.stream.chunk" {
			continue
		}
		for _, item := range event.Attributes {
			if string(item.Key) == "neatlogs.stream.chunk.summary" {
				fragments = append(fragments, doctorJSONValue(doctorAttributeValue(item.Value)))
			}
		}
	}
	if len(fragments) == 0 {
		return nil
	}
	return fragments
}

func doctorToolCall(values map[string]any, isTool bool) any {
	if !isTool {
		return nil
	}
	id, _ := values["neatlogs.tool_call.id"].(string)
	if id == "" {
		return nil
	}
	call := map[string]any{"id": id}
	if name, ok := values["neatlogs.tool.name"].(string); ok {
		call["name"] = name
	}
	if input, ok := values["neatlogs.tool.input"]; ok {
		call["arguments"] = doctorJSONValue(input)
	}
	if output, ok := values["neatlogs.tool.output"]; ok {
		call["result"] = doctorJSONValue(output)
	}
	return call
}

func doctorPayloadReferences(values map[string]any) any {
	records := make(map[string]map[string]any)
	for key, value := range values {
		mediaAt := strings.Index(key, ".media.")
		if !strings.HasPrefix(key, "neatlogs.") || mediaAt < 0 {
			continue
		}
		prefix := key[:mediaAt+len(".media.")]
		index, field, ok := doctorIndexedField(key, prefix)
		if !ok || (field != "sha256" && field != "byte_length" && field != "mime_type") {
			continue
		}
		identity := prefix + strconv.Itoa(index)
		if records[identity] == nil {
			records[identity] = make(map[string]any)
		}
		records[identity][field] = doctorJSONValue(value)
	}
	identities := make([]string, 0, len(records))
	for identity := range records {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	references := make([]map[string]any, 0, len(records))
	for _, identity := range identities {
		record := records[identity]
		digest, ok := record["sha256"].(string)
		if !ok || len(digest) != 64 {
			continue
		}
		size, _ := doctorInteger(record["byte_length"])
		mimeType, _ := record["mime_type"].(string)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		references = append(references, map[string]any{"digest": "sha256:" + digest, "size": size, "mime_type": mimeType})
	}
	if len(references) == 0 {
		return nil
	}
	return references
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
			"tool_calls":         span.ToolCalls,
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
