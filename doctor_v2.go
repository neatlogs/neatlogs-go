package neatlogs

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
)

const DoctorV2FormatVersion = "neatlogs.doctor/v2"

type DoctorStatus string

const (
	DoctorPass DoctorStatus = "pass"
	DoctorWarn DoctorStatus = "warn"
	DoctorFail DoctorStatus = "fail"
)

type DoctorV2Runtime struct {
	Language      string `json:"language"`
	SDKVersion    string `json:"sdk_version"`
	SchemaVersion string `json:"schema_version"`
	Transport     string `json:"transport"`
}
type DoctorV2Capture struct {
	TraceID        string  `json:"trace_id"`
	RootSpanID     *string `json:"root_span_id"`
	SpanCount      int     `json:"span_count"`
	SemanticDigest string  `json:"semantic_digest"`
}
type DoctorV2Sampling struct {
	EffectiveSampler string  `json:"effective_sampler"`
	RootSampleRate   float64 `json:"root_sample_rate"`
	Sampled          bool    `json:"sampled"`
}
type DoctorV2Ownership struct {
	Provider          string `json:"provider"`
	InstrumentorCount int    `json:"instrumentor_count"`
}
type DoctorV2Queue struct {
	Mode         string `json:"mode"`
	PendingSpans uint64 `json:"pending_spans"`
	DroppedSpans uint64 `json:"dropped_spans"`
	Capacity     *int   `json:"capacity"`
}
type DoctorV2Retry struct {
	Attempts  int   `json:"attempts"`
	WindowMS  int64 `json:"window_ms"`
	Exhausted bool  `json:"exhausted"`
}
type DoctorV2Flush struct {
	Outcome    string `json:"outcome"`
	TimeoutMS  int64  `json:"timeout_ms"`
	DurationMS *int64 `json:"duration_ms"`
}
type DoctorV2Check struct {
	Name            string         `json:"name"`
	Status          DoctorStatus   `json:"status"`
	ReasonCode      string         `json:"reason_code"`
	Message         string         `json:"message"`
	RemediationCode string         `json:"remediation_code"`
	Details         map[string]any `json:"details,omitempty"`
}
type DoctorV2Result struct {
	FormatVersion string             `json:"format_version"`
	Mode          string             `json:"mode"`
	Status        DoctorStatus       `json:"status"`
	FirstFailure  *string            `json:"first_failure"`
	Runtime       DoctorV2Runtime    `json:"runtime"`
	Capture       *DoctorV2Capture   `json:"capture,omitempty"`
	Sampling      *DoctorV2Sampling  `json:"sampling,omitempty"`
	Ownership     *DoctorV2Ownership `json:"ownership,omitempty"`
	Queue         *DoctorV2Queue     `json:"queue,omitempty"`
	Retry         *DoctorV2Retry     `json:"retry,omitempty"`
	Flush         *DoctorV2Flush     `json:"flush,omitempty"`
	Probe         *DoctorV2Probe     `json:"probe,omitempty"`
	Checks        []DoctorV2Check    `json:"checks"`
}

type DoctorLocalOptions struct {
	TraceID        string
	RootSampleRate float64
	FlushOutcome   string
	FlushTimeout   time.Duration
	FlushDuration  time.Duration
}

var doctorTraceID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var doctorSpanID = regexp.MustCompile(`^[0-9a-f]{16}$`)

func runtimeForContext(ctx context.Context) *sdkRuntime {
	if client, ok := ClientFromContext(ctx); ok {
		return client.runtime
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.state != stateRunning {
		return nil
	}
	return global.runtime
}

// DoctorCapturedLocalV2 validates the latest final masked export projection for
// TraceID. It is read-only and performs no network access or source mutation.
func DoctorCapturedLocalV2(ctx context.Context, options DoctorLocalOptions) DoctorV2Result {
	result := newDoctorV2Result("local")
	runtime := runtimeForContext(ctx)
	if runtime == nil || runtime.captures == nil {
		result.Checks = append(result.Checks, failV2("local_envelope", "INSTRUMENTOR_INACTIVE", "No running Neatlogs runtime is selected", "ENABLE_INSTRUMENTOR"))
		return finishDoctorV2(result)
	}
	envelope, found := runtime.captures.envelope(options.TraceID)
	if !found {
		result.Checks = append(result.Checks, failV2("local_envelope", "TRACE_ID_INVALID", "No captured final envelope matches the trace ID", "RECREATE_TRACE"))
		return finishDoctorV2(result)
	}
	digest, err := semanticDigest(envelope)
	if err != nil {
		result.Checks = append(result.Checks, failV2("local_envelope", "OUTPUT_JSON_INVALID", "The normalized envelope cannot be canonicalized", "SERIALIZE_OUTPUT_JSON"))
		return finishDoctorV2(result)
	}
	result.Capture = &DoctorV2Capture{TraceID: envelope.TraceID, RootSpanID: envelope.RootSpanID, SpanCount: len(envelope.Spans), SemanticDigest: digest}
	rate := options.RootSampleRate
	sampled := false
	if len(envelope.Spans) > 0 {
		sampled = envelope.Spans[0].Sampled
	}
	result.Sampling = &DoctorV2Sampling{EffectiveSampler: "parentbased_traceidratio", RootSampleRate: rate, Sampled: sampled}
	// Manual Doctor spans do not activate an integration instrumentor. Provider
	// ownership and instrumentor count are separate cross-SDK signals.
	result.Ownership = &DoctorV2Ownership{Provider: "private", InstrumentorCount: 0}
	health := runtime.delivery.snapshot()
	dropped := health.SpanQueueDrops + health.MaskedSpanDrops
	capacity := 2048
	result.Queue = &DoctorV2Queue{Mode: "diagnostic_capture", DroppedSpans: dropped, Capacity: &capacity}
	result.Retry = &DoctorV2Retry{Exhausted: health.SpanExportFailures > 0}
	var duration *int64
	if options.FlushDuration > 0 {
		value := options.FlushDuration.Milliseconds()
		duration = &value
	}
	outcome := options.FlushOutcome
	if outcome == "" {
		outcome = "not_attempted"
	}
	result.Flush = &DoctorV2Flush{Outcome: outcome, TimeoutMS: options.FlushTimeout.Milliseconds(), DurationMS: duration}
	result.Checks = append(result.Checks, validateDoctorEnvelope(envelope)...)
	if controlledDoctorEnvelope(envelope) {
		if check := validateControlledDoctorEnvelope(envelope); check != nil {
			result.Checks = append(result.Checks, *check)
		}
	}
	if health.MaskedSpanDrops > 0 {
		result.Checks = append(result.Checks, failV2("masking", "MASKING_FAILED_CLOSED", "Masking failed closed and affected spans were dropped", "FIX_MASK_CALLBACK"))
	}
	if dropped > 0 {
		result.Checks = append(result.Checks, failV2("queue", "QUEUE_SATURATED", "The exporter dropped telemetry", "INCREASE_OR_DRAIN_QUEUE"))
	}
	if health.SpanExportFailures > 0 {
		result.Checks = append(result.Checks, failV2("retry", "EXPORT_RETRY_EXHAUSTED", "The exporter observed a transport failure", "CHECK_TRANSPORT"))
	}
	if outcome == "timeout" {
		result.Checks = append(result.Checks, failV2("flush", "FLUSH_TIMEOUT", "The flush deadline expired", "INCREASE_FLUSH_BUDGET"))
	}
	return finishDoctorV2(result)
}

func controlledDoctorEnvelope(envelope DoctorEnvelope) bool {
	for _, span := range envelope.Spans {
		if strings.HasPrefix(span.Name, "doctor.probe.") {
			return true
		}
	}
	return false
}

func validateControlledDoctorEnvelope(envelope DoctorEnvelope) *DoctorV2Check {
	expected := map[string]struct {
		kind, parent string
	}{
		"doctor.probe.root":  {"WORKFLOW", ""},
		"doctor.probe.agent": {"AGENT", "doctor.probe.root"},
		"doctor.probe.llm":   {"LLM", "doctor.probe.agent"},
		"doctor.probe.tool":  {"TOOL", "doctor.probe.root"},
	}
	if len(envelope.Spans) != 4 {
		check := failV2("probe_fixture", "PROBE_FIXTURE_INVALID", "Doctor must capture exactly four semantic fixture spans", "FIX_DOCTOR_INSTRUMENTATION")
		return &check
	}
	byName := make(map[string]DoctorSpan, 4)
	namesByID := make(map[string]string, 4)
	for _, span := range envelope.Spans {
		byName[span.Name] = span
		namesByID[span.SpanID] = span.Name
	}
	if len(byName) != 4 {
		check := failV2("probe_fixture", "PROBE_FIXTURE_INVALID", "Doctor fixture span names are not unique", "FIX_DOCTOR_INSTRUMENTATION")
		return &check
	}
	for name, want := range expected {
		span, ok := byName[name]
		parent := ""
		if span.ParentSpanID != nil {
			parent = namesByID[*span.ParentSpanID]
		}
		if !ok || span.Kind != want.kind || parent != want.parent || span.Input == nil || span.Output == nil ||
			span.Attributes["neatlogs.doctor"] != true || span.Attributes["neatlogs.doctor.version"] != "v1" ||
			span.Attributes["service.name"] != "neatlogs.doctor.v2" || span.Attributes["telemetry.sdk.language"] != "go" ||
			span.Attributes["telemetry.sdk.version"] != Version || span.Attributes["neatlogs.span.type"] != want.kind {
			check := failV2("probe_fixture", "PROBE_FIXTURE_INVALID", "Doctor fixture hierarchy, metadata, or input/output is incomplete", "FIX_DOCTOR_INSTRUMENTATION")
			return &check
		}
	}
	return nil
}

func validateDoctorEnvelope(envelope DoctorEnvelope) []DoctorV2Check {
	if !doctorTraceID.MatchString(envelope.TraceID) {
		return []DoctorV2Check{failV2("identity", "TRACE_ID_INVALID", "Trace ID is invalid", "RECREATE_TRACE")}
	}
	seen := map[string]bool{}
	toolRequests := map[string]bool{}
	toolExecutions := map[string]bool{}
	rootCount := 0
	var sampled *bool
	for _, span := range envelope.Spans {
		if !doctorSpanID.MatchString(span.SpanID) {
			return []DoctorV2Check{failV2("identity", "SPAN_ID_INVALID", "A span ID is invalid", "RECREATE_SPAN")}
		}
		if seen[span.SpanID] {
			return []DoctorV2Check{failV2("identity", "SPAN_ID_DUPLICATE", "A span ID is duplicated", "RECREATE_SPAN")}
		}
		seen[span.SpanID] = true
		if sampled == nil {
			value := span.Sampled
			sampled = &value
		} else if *sampled != span.Sampled {
			return []DoctorV2Check{failV2("sampling", "SAMPLING_INCONSISTENT", "Sampling decisions differ within one trace", "FIX_PARENT_BASED_SAMPLING")}
		}
		if span.ParentSpanID == nil && span.Name != completionMarkerName {
			rootCount++
		}
		if span.Input != nil {
			if _, err := json.Marshal(span.Input); err != nil {
				return []DoctorV2Check{failV2("input", "INPUT_JSON_INVALID", "Span input is not canonical JSON", "SERIALIZE_INPUT_JSON")}
			}
		}
		if span.Output != nil {
			if _, err := json.Marshal(span.Output); err != nil {
				return []DoctorV2Check{failV2("output", "OUTPUT_JSON_INVALID", "Span output is not canonical JSON", "SERIALIZE_OUTPUT_JSON")}
			}
		}
		if span.ParentSpanID != nil && !doctorSpanID.MatchString(*span.ParentSpanID) {
			return []DoctorV2Check{failV2("hierarchy", "PARENT_ID_INVALID", "A parent span ID is invalid", "FIX_PARENT_CONTEXT")}
		}
		if span.ExpectedChoiceCount != nil && doctorCollectionLength(span.Choices) < *span.ExpectedChoiceCount {
			return []DoctorV2Check{failV2("choices", "CHOICE_LOSS", "The normalized response lost model choices", "PRESERVE_ALL_CHOICES")}
		}
		if span.Streaming && doctorCollectionLength(span.StreamFragments) == 0 {
			return []DoctorV2Check{failV2("streaming", "STREAM_FRAGMENT_MISSING", "A streaming span has no captured canonical chunk events", "PRESERVE_STREAM_FRAGMENTS")}
		}
		if span.Oversized && !doctorHasValidPayloadReference(span.PayloadReferences) {
			return []DoctorV2Check{failV2("payload", "PAYLOAD_ATTACHMENT_REQUIRED", "Oversized content requires a valid payload reference", "UPLOAD_PAYLOAD_ATTACHMENT")}
		}
		collectDoctorToolIDs(span.Choices, toolRequests)
		collectDoctorToolIDs(span.ToolCall, toolExecutions)
	}
	if rootCount == 0 {
		return []DoctorV2Check{failV2("lifecycle", "ROOT_MISSING", "The captured envelope has no root span", "CREATE_ROOT_SPAN")}
	}
	if rootCount > 1 {
		return []DoctorV2Check{failV2("lifecycle", "ROOT_MULTIPLE", "The captured envelope has multiple roots", "USE_SINGLE_ROOT")}
	}
	for _, span := range envelope.Spans {
		if span.ParentSpanID != nil && !seen[*span.ParentSpanID] {
			return []DoctorV2Check{failV2("hierarchy", "PARENT_MISSING", "A parent span is missing from the captured envelope", "FIX_PARENT_CONTEXT")}
		}
		if !span.Ended {
			return []DoctorV2Check{failV2("lifecycle", "ROOT_NOT_ENDED", "A captured span has not ended", "END_ROOT_SPAN")}
		}
	}
	for id := range toolRequests {
		if !toolExecutions[id] {
			return []DoctorV2Check{failV2("tools", "TOOL_EXECUTION_MISSING", "A requested tool call has no execution span", "CAPTURE_TOOL_EXECUTION")}
		}
	}
	for id := range toolExecutions {
		if !toolRequests[id] {
			return []DoctorV2Check{failV2("tools", "TOOL_CALL_MISSING", "A tool execution has no assistant tool request", "CAPTURE_TOOL_REQUEST")}
		}
	}
	return []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", Message: "The final normalized local envelope is valid", RemediationCode: "NONE"}}
}

func doctorCollectionLength(value any) int {
	switch typed := value.(type) {
	case []any:
		return len(typed)
	case []map[string]any:
		return len(typed)
	default:
		return 0
	}
}

func doctorHasValidPayloadReference(value any) bool {
	items := make([]any, 0)
	switch typed := value.(type) {
	case []any:
		items = typed
	case []map[string]any:
		for _, item := range typed {
			items = append(items, item)
		}
	default:
		return false
	}
	digest := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	for _, item := range items {
		reference, ok := item.(map[string]any)
		if !ok {
			continue
		}
		digestValue, digestOK := reference["digest"].(string)
		mimeType, mimeOK := reference["mime_type"].(string)
		size := doctorPositiveNumber(reference["size"])
		if digestOK && mimeOK && mimeType != "" && size && digest.MatchString(digestValue) {
			return true
		}
	}
	return false
}

func doctorPositiveNumber(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed > 0
	case int64:
		return typed > 0
	case float64:
		return typed > 0
	default:
		return false
	}
}

func collectDoctorToolIDs(value any, output map[string]bool) {
	switch item := value.(type) {
	case map[string]any:
		if id, ok := item["id"].(string); ok && id != "" {
			output[id] = true
		}
		for _, child := range item {
			collectDoctorToolIDs(child, output)
		}
	case []any:
		for _, child := range item {
			collectDoctorToolIDs(child, output)
		}
	case []map[string]any:
		for _, child := range item {
			collectDoctorToolIDs(child, output)
		}
	}
}

func newDoctorV2Result(mode string) DoctorV2Result {
	return DoctorV2Result{FormatVersion: DoctorV2FormatVersion, Mode: mode, Status: DoctorPass, Runtime: DoctorV2Runtime{Language: "go", SDKVersion: Version, SchemaVersion: "2", Transport: "otlp_http_protobuf"}, Checks: []DoctorV2Check{}}
}
func failV2(name, code, message, remediation string) DoctorV2Check {
	return DoctorV2Check{Name: name, Status: DoctorFail, ReasonCode: code, Message: message, RemediationCode: remediation}
}
func finishDoctorV2(result DoctorV2Result) DoctorV2Result {
	result.Status = DoctorPass
	for _, check := range result.Checks {
		if check.Status == DoctorFail {
			result.Status = DoctorFail
			code := check.ReasonCode
			result.FirstFailure = &code
			break
		}
		if check.Status == DoctorWarn {
			result.Status = DoctorWarn
		}
	}
	return result
}

// CapturedTraceIDs returns a sorted, bounded snapshot useful for in-process
// diagnostics. It never exposes span content.
func CapturedTraceIDs(ctx context.Context) []string {
	runtime := runtimeForContext(ctx)
	if runtime == nil || runtime.captures == nil {
		return nil
	}
	runtime.captures.mu.RLock()
	ids := append([]string(nil), runtime.captures.order...)
	runtime.captures.mu.RUnlock()
	sort.Strings(ids)
	return ids
}
