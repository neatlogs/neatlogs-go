package neatlogs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type DoctorV2Probe struct {
	IngestRoute       string `json:"ingest_route"`
	MarkerHeader      string `json:"marker_header"`
	MarkerVersion     string `json:"marker_version"`
	Visible           bool   `json:"visible"`
	ReadbackTraceID   string `json:"readback_trace_id"`
	Finalized         bool   `json:"finalized"`
	MeaningfulRoots   int    `json:"meaningful_root_count"`
	DuplicateSpans    int    `json:"duplicate_span_count"`
	ReadbackSpanCount int    `json:"readback_span_count"`
	HierarchyValid    bool   `json:"hierarchy_valid"`
	AttributesValid   bool   `json:"attributes_valid"`
	InputOutputValid  bool   `json:"input_output_valid"`
	MetadataValid     bool   `json:"metadata_valid"`
	TypedTokensValid  bool   `json:"typed_tokens_valid"`
}

type DoctorProbeOptions struct {
	Endpoint     string
	APIKey       string
	Timeout      time.Duration
	PollInterval time.Duration
	HTTPClient   *http.Client
}

var persistedDoctorSpanID = regexp.MustCompile(`^[0-9a-f]{16}$`)
var safeDoctorFailureCode = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

const maxDoctorReadbackBytes = 1 << 20

var doctorIngestionStages = map[string]bool{
	"kafka_published": true, "pii_dispatch": true, "pii_redaction": true,
	"storage_consumer": true, "raw_durable": true, "root_resolution": true,
	"simplification": true, "finalized": true,
}

var doctorIngestionStates = map[string]bool{
	"processing": true, "failed": true, "succeeded": true,
}

func doctorIngestionDiagnosticDetails(value map[string]any) map[string]any {
	diagnostics := doctorObject(value["ingestionDiagnostics"])
	protocolVersion, _ := diagnostics["protocolVersion"].(string)
	currentStage, currentOK := diagnostics["currentStage"].(string)
	state, stateOK := diagnostics["state"].(string)
	retryable, retryableOK := diagnostics["retryable"].(bool)
	if protocolVersion != "v1" || !currentOK || !doctorIngestionStages[currentStage] ||
		!stateOK || !doctorIngestionStates[state] || !retryableOK {
		return nil
	}
	details := map[string]any{
		"ingestion_state": state,
		"current_stage":   currentStage,
		"retryable":       retryable,
	}
	optionalStages := []struct{ source, target string }{
		{"lastSuccessfulStage", "last_successful_stage"},
		{"failedStage", "failed_stage"},
	}
	for _, field := range optionalStages {
		if raw, exists := diagnostics[field.source]; exists {
			stage, ok := raw.(string)
			if !ok || !doctorIngestionStages[stage] {
				return nil
			}
			details[field.target] = stage
		}
	}
	if raw, exists := diagnostics["failureCode"]; exists {
		failureCode, ok := raw.(string)
		if !ok || !safeDoctorFailureCode.MatchString(failureCode) {
			return nil
		}
		details["failure_code"] = failureCode
	}
	return details
}

// DoctorProbeV2 reads the exact controlled trace back through the existing
// authenticated product route. The caller must first export local.Capture via
// the normal /v1/traces OTLP pipeline using WithDoctorProbe.
func DoctorProbeV2(ctx context.Context, local DoctorV2Result, options DoctorProbeOptions) DoctorV2Result {
	result := local
	result.Mode = "probe"
	if local.Capture == nil || !doctorReadbackEligible(local) {
		return result
	}
	if strings.TrimSpace(options.APIKey) == "" {
		result.Checks = append(result.Checks, failV2("configuration", "CREDENTIAL_MISSING", "A project ingestion credential is required", "SET_CREDENTIAL"))
		return finishDoctorV2(result)
	}
	base, err := safeDoctorEndpoint(options.Endpoint)
	if err != nil {
		result.Checks = append(result.Checks, failV2("configuration", "ENDPOINT_INVALID", "The trace endpoint is invalid", "SET_ENDPOINT"))
		return finishDoctorV2(result)
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	interval := options.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	baseClient := options.HTTPClient
	if baseClient == nil {
		baseClient = &http.Client{}
	}
	client := *baseClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	readURL := *base
	readURL.Path = "/api/traces/v3/" + url.PathEscape(local.Capture.TraceID)
	readURL.RawQuery = ""
	readURL.Fragment = ""
	var lastDiagnostics map[string]any

	for {
		request, requestErr := http.NewRequestWithContext(probeCtx, http.MethodGet, readURL.String(), nil)
		if requestErr != nil {
			return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "The exact trace read request could not be created", "CHECK_TRACE_ENDPOINT")
		}
		request.Header.Set("x-api-key", options.APIKey)
		request.Header.Set("x-neatlogs-doctor", "v1")
		response, requestErr := client.Do(request)
		if requestErr != nil {
			if probeCtx.Err() != nil {
				return probeReadFailureWithDetails(result, "BACKEND_PROBE_UNAVAILABLE", "Timed out waiting for the exact Doctor trace", "WAIT_FOR_TRACE", lastDiagnostics)
			}
			return probeReadFailureWithDetails(result, "BACKEND_PROBE_UNAVAILABLE", "The existing trace read path is unavailable", "CHECK_TRACE_ENDPOINT", lastDiagnostics)
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			response.Body.Close()
			return probeReadFailure(result, "AUTH_FAILED", "The project key was rejected by the existing trace API", "CHECK_INGEST_CREDENTIAL")
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 && response.StatusCode != http.StatusAccepted {
			var traceData map[string]any
			decodeErr := decodeLimited(response, &traceData)
			response.Body.Close()
			if decodeErr != nil {
				return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "Trace read-back returned an invalid response", "CHECK_TRACE_ENDPOINT")
			}
			lastDiagnostics = doctorIngestionDiagnosticDetails(traceData)
			return persistedDoctorProbeResultWithDiagnostics(result, traceData, lastDiagnostics)
		}
		status := response.StatusCode
		var currentDiagnostics map[string]any
		if status == http.StatusAccepted || status == http.StatusNotFound || status == http.StatusConflict {
			var value map[string]any
			if decodeLimited(response, &value) == nil {
				if diagnostics := doctorIngestionDiagnosticDetails(value); diagnostics != nil {
					currentDiagnostics = diagnostics
					lastDiagnostics = diagnostics
				}
			}
		}
		response.Body.Close()
		if status == http.StatusConflict {
			return probeReadFailureWithDetails(result, "BACKEND_PROBE_UNAVAILABLE", "Trace ingestion reported a terminal failure", "CHECK_TRACE_ENDPOINT", currentDiagnostics)
		}
		if status != http.StatusAccepted && status != http.StatusNotFound {
			if status >= http.StatusInternalServerError {
				return probeReadFailureWithDetails(result, "BACKEND_PROBE_UNAVAILABLE", "The existing trace read path returned an unexpected status", "CHECK_TRACE_ENDPOINT", lastDiagnostics)
			}
			return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "The existing trace read path returned an unexpected status", "CHECK_TRACE_ENDPOINT")
		}
		select {
		case <-probeCtx.Done():
			return probeReadFailureWithDetails(result, "BACKEND_PROBE_UNAVAILABLE", "Timed out waiting for the exact Doctor trace", "WAIT_FOR_TRACE", lastDiagnostics)
		case <-time.After(interval):
		}
	}
}

// A rejected OTLP export can still leave a complete post-mask capture. In that
// case the authenticated product read is the authoritative way to classify a
// bad project key. Structural/capture failures remain local and never trigger a
// misleading backend request.
func doctorReadbackEligible(local DoctorV2Result) bool {
	for _, check := range local.Checks {
		if check.Status != DoctorFail {
			continue
		}
		switch check.ReasonCode {
		case "EXPORT_RETRY_EXHAUSTED", "FLUSH_TIMEOUT":
			continue
		default:
			return false
		}
	}
	return true
}

func safeDoctorEndpoint(value string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(value))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
		return nil, errors.New("invalid endpoint")
	}
	base.Path = ""
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base, nil
}

func persistedDoctorProbeResult(result DoctorV2Result, traceData map[string]any) DoctorV2Result {
	return persistedDoctorProbeResultWithDiagnostics(result, traceData, nil)
}

func persistedDoctorProbeResultWithDiagnostics(result DoctorV2Result, traceData map[string]any, diagnosticDetails map[string]any) DoctorV2Result {
	spans := doctorObjectSlice(traceData["spans"])
	idSet := make(map[string]bool, len(spans))
	byName := make(map[string]map[string]any, len(spans))
	duplicateSpanCount := 0
	meaningfulRootCount := 0
	hierarchyValid := len(spans) == 4
	for _, span := range spans {
		id, _ := span["span_id"].(string)
		if !persistedDoctorSpanID.MatchString(id) || idSet[id] {
			hierarchyValid = false
		}
		if idSet[id] {
			duplicateSpanCount++
		}
		idSet[id] = true
		name := doctorString(span["node_name"], span["span_name"])
		byName[name] = span
		if doctorString(span["parent_span_id"]) == "" && name != completionMarkerName {
			meaningfulRootCount++
		}
	}

	expected := map[string]string{
		"doctor.probe.root":  "workflow",
		"doctor.probe.agent": "agent_action",
		"doctor.probe.llm":   "llm",
		"doctor.probe.tool":  "tool_call",
	}
	attributesValid := len(byName) == len(expected)
	inputOutputValid := true
	metadataValid := true
	// The v3 read path intentionally returns the UI-facing simplified view.
	// It may preserve normalized JSON or render the same deterministic semantic
	// value for display. Keep this allowlist identical across SDKs.
	type materializedIO struct{ inputs, outputs []any }
	expectedIO := map[string]materializedIO{
		"doctor.probe.root": {
			inputs:  []any{map[string]any{"prompt": "generated diagnostic input"}, "generated diagnostic input"},
			outputs: []any{map[string]any{"result": map[string]any{"value": float64(2)}}, "Value: 2"},
		},
		"doctor.probe.agent": {
			inputs:  []any{map[string]any{"prompt": "generated diagnostic input"}, "Prompt: generated diagnostic input"},
			outputs: []any{map[string]any{"text": "generated diagnostic output"}, "Text: generated diagnostic output"},
		},
		"doctor.probe.llm": {
			inputs: []any{
				map[string]any{"messages": []any{map[string]any{"role": "user", "content": "generated diagnostic input"}}},
				map[string]any{"prompt": "generated diagnostic input"},
			},
			outputs: []any{map[string]any{"text": "generated diagnostic output"}, "Text: generated diagnostic output"},
		},
		"doctor.probe.tool": {
			inputs:  []any{map[string]any{"value": float64(1)}, "Value: 1"},
			outputs: []any{map[string]any{"value": float64(2)}, "Value: 2"},
		},
	}
	for name, kind := range expected {
		match := byName[name]
		if match == nil {
			attributesValid = false
			inputOutputValid = false
			metadataValid = false
			continue
		}
		if strings.ToLower(doctorString(match["node_type"], match["span_type"])) != kind {
			attributesValid = false
		}
		data := doctorObject(match["data"])
		io := expectedIO[name]
		if !doctorMatchesMaterializedValue(data["input_value"], io.inputs) ||
			!doctorMatchesMaterializedValue(data["output_value"], io.outputs) {
			inputOutputValid = false
		}
		metadata := doctorObject(match["span_metadata"])
		spanType := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(kind, "agent_action", "agent"), "tool_call", "tool"))
		if metadata["neatlogs.doctor"] != true || metadata["neatlogs.doctor.version"] != "v1" ||
			metadata["service.name"] != "neatlogs.doctor.v2" || metadata["telemetry.sdk.language"] != "go" ||
			metadata["telemetry.sdk.version"] != Version || !strings.EqualFold(doctorString(metadata["neatlogs.span.kind"]), spanType) {
			metadataValid = false
		}
	}
	if len(byName) != 4 {
		hierarchyValid = false
	}
	if duplicateSpanCount != 0 || meaningfulRootCount != 1 {
		hierarchyValid = false
	}
	if hierarchyValid {
		rootID, _ := byName["doctor.probe.root"]["span_id"].(string)
		agentID, _ := byName["doctor.probe.agent"]["span_id"].(string)
		rootParent, _ := byName["doctor.probe.root"]["parent_span_id"].(string)
		hierarchyValid = rootParent == "" &&
			doctorString(byName["doctor.probe.agent"]["parent_span_id"]) == rootID &&
			doctorString(byName["doctor.probe.llm"]["parent_span_id"]) == agentID &&
			doctorString(byName["doctor.probe.tool"]["parent_span_id"]) == rootID
	}

	typedTokensValid := doctorExactNumber(traceData["promptTokens"], 11) &&
		doctorExactNumber(traceData["completionTokens"], 7) &&
		doctorExactNumber(traceData["totalTokensUsed"], 18)
	readbackSpanCount := len(spans)
	if value, ok := doctorInt(traceData["spanCount"]); ok {
		readbackSpanCount = value
	}
	traceID, _ := traceData["_id"].(string)
	visible := result.Capture != nil && traceID == result.Capture.TraceID
	status := strings.ToLower(doctorString(traceData["status"]))
	// A terminal failure is materialized, but it is not a successful Doctor
	// probe. Only the product API's documented successful terminal value passes.
	finalized := status == "success"

	result.Probe = &DoctorV2Probe{
		IngestRoute: "/v1/traces", MarkerHeader: "x-neatlogs-doctor", MarkerVersion: "v1",
		Visible: visible, ReadbackTraceID: traceID, Finalized: finalized,
		MeaningfulRoots: meaningfulRootCount, DuplicateSpans: duplicateSpanCount,
		ReadbackSpanCount: readbackSpanCount, HierarchyValid: hierarchyValid,
		AttributesValid: attributesValid, InputOutputValid: inputOutputValid,
		MetadataValid: metadataValid, TypedTokensValid: typedTokensValid,
	}
	validations := []struct {
		name, passCode, remediation, message string
		passed                               bool
	}{
		{"probe_visibility", "TRACE_VISIBLE", "WAIT_FOR_TRACE", "The exact Doctor trace is visible through the authenticated trace API", visible && result.Capture != nil && readbackSpanCount == 4 && len(spans) == 4},
		{"probe_finalization", "TRACE_FINALIZED", "WAIT_FOR_TRACE", "The exact Doctor trace reached a terminal materialized state", finalized},
		{"probe_hierarchy", "HIERARCHY_VALID", "CHECK_TRACE_FINALIZER", "The persisted Doctor hierarchy has one root and valid parents", hierarchyValid},
		{"probe_attributes", "ATTRIBUTES_VALID", "CHECK_ATTRIBUTE_MAPPING", "The persisted Doctor span names and types are complete", attributesValid},
		{"probe_input_output", "INPUT_OUTPUT_VALID", "CHECK_PAYLOAD_MAPPING", "The persisted Doctor spans retain input and output", inputOutputValid},
		{"probe_metadata", "METADATA_VALID", "CHECK_METADATA_FINALIZATION", "The versioned Doctor SDK metadata survived finalization", metadataValid},
		{"probe_typed_tokens", "TYPED_TOKENS_VALID", "CHECK_TOKEN_MAPPING", "Persisted token totals remain numeric", typedTokensValid},
	}
	for _, validation := range validations {
		if validation.passed {
			check := DoctorV2Check{Name: validation.name, Status: DoctorPass, ReasonCode: validation.passCode, Message: validation.message, RemediationCode: "NONE"}
			if validation.name == "probe_finalization" && diagnosticDetails != nil {
				check.Details = diagnosticDetails
			}
			result.Checks = append(result.Checks, check)
		} else {
			check := failV2(validation.name, validation.passCode+"_FAILED", validation.message, validation.remediation)
			if validation.name == "probe_finalization" && diagnosticDetails != nil {
				check.Details = diagnosticDetails
			}
			result.Checks = append(result.Checks, check)
		}
	}
	return finishDoctorV2(result)
}

func doctorValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func doctorMatchesMaterializedValue(value any, candidates []any) bool {
	actual := doctorJSONValue(value)
	for _, candidate := range candidates {
		if doctorValuesEqual(actual, candidate) {
			return true
		}
	}
	return false
}

func doctorObject(value any) map[string]any {
	if item, ok := value.(map[string]any); ok {
		return item
	}
	return map[string]any{}
}

func doctorObjectSlice(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func doctorString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func doctorExactNumber(value any, expected float64) bool {
	number, ok := value.(float64)
	return ok && !math.IsNaN(number) && !math.IsInf(number, 0) && number == expected
}

func doctorInt(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || math.Trunc(number) != number || number < 0 || number > float64(^uint(0)>>1) {
		return 0, false
	}
	return int(number), true
}

func probeReadFailure(result DoctorV2Result, code, message, remediation string) DoctorV2Result {
	return probeReadFailureWithDetails(result, code, message, remediation, nil)
}

func probeReadFailureWithDetails(result DoctorV2Result, code, message, remediation string, details map[string]any) DoctorV2Result {
	check := failV2("probe_transport", code, message, remediation)
	if details != nil {
		check.Details = details
	}
	if code == "AUTH_FAILED" {
		// Authentication is the actionable root classification even when the
		// preceding OTLP flush recorded the same rejected credential as a
		// transport failure.
		result.Checks = append([]DoctorV2Check{check}, result.Checks...)
	} else {
		result.Checks = append(result.Checks, check)
	}
	return finishDoctorV2(result)
}

func decodeLimited(response *http.Response, destination any) error {
	if response == nil || response.Body == nil {
		return errors.New("empty response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDoctorReadbackBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxDoctorReadbackBytes {
		return errors.New("trace read-back exceeded the response limit")
	}
	return json.Unmarshal(body, destination)
}
