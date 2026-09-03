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

// DoctorProbeV2 reads the exact controlled trace back through the existing
// authenticated product route. The caller must first export local.Capture via
// the normal /v1/traces OTLP pipeline using WithDoctorProbe.
func DoctorProbeV2(ctx context.Context, local DoctorV2Result, options DoctorProbeOptions) DoctorV2Result {
	result := local
	result.Mode = "probe"
	if local.Capture == nil || local.Status == DoctorFail {
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
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}

	readURL := *base
	readURL.Path = "/api/traces/v3/" + url.PathEscape(local.Capture.TraceID)
	readURL.RawQuery = ""
	readURL.Fragment = ""

	for {
		request, requestErr := http.NewRequestWithContext(probeCtx, http.MethodGet, readURL.String(), nil)
		if requestErr != nil {
			return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "The exact trace read request could not be created", "CHECK_TRACE_ENDPOINT")
		}
		request.Header.Set("x-api-key", options.APIKey)
		response, requestErr := client.Do(request)
		if requestErr != nil {
			if probeCtx.Err() != nil {
				return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "Timed out waiting for the exact Doctor trace", "WAIT_FOR_TRACE")
			}
			return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "The existing trace read path is unavailable", "CHECK_TRACE_ENDPOINT")
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
			return persistedDoctorProbeResult(result, traceData)
		}
		status := response.StatusCode
		response.Body.Close()
		if status != http.StatusAccepted && status != http.StatusNotFound {
			return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "The existing trace read path returned an unexpected status", "CHECK_TRACE_ENDPOINT")
		}
		select {
		case <-probeCtx.Done():
			return probeReadFailure(result, "BACKEND_PROBE_UNAVAILABLE", "Timed out waiting for the exact Doctor trace", "WAIT_FOR_TRACE")
		case <-time.After(interval):
		}
	}
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
	spans := doctorObjectSlice(traceData["spans"])
	idSet := make(map[string]bool, len(spans))
	byName := make(map[string]map[string]any, len(spans))
	hierarchyValid := len(spans) == 4
	for _, span := range spans {
		id, _ := span["span_id"].(string)
		if !persistedDoctorSpanID.MatchString(id) || idSet[id] {
			hierarchyValid = false
		}
		idSet[id] = true
		byName[doctorString(span["node_name"], span["span_name"])] = span
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
	expectedIO := map[string][2]any{
		"doctor.probe.root":  {map[string]any{"prompt": "generated diagnostic input"}, map[string]any{"result": map[string]any{"value": float64(2)}}},
		"doctor.probe.agent": {map[string]any{"prompt": "generated diagnostic input"}, map[string]any{"text": "generated diagnostic output"}},
		"doctor.probe.llm":   {map[string]any{"messages": []any{map[string]any{"role": "user", "content": "generated diagnostic input"}}}, map[string]any{"text": "generated diagnostic output"}},
		"doctor.probe.tool":  {map[string]any{"value": float64(1)}, map[string]any{"value": float64(2)}},
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
		if !doctorValuesEqual(doctorJSONValue(data["input_value"]), io[0]) ||
			!doctorValuesEqual(doctorJSONValue(data["output_value"]), io[1]) {
			inputOutputValid = false
		}
		metadata := doctorObject(match["span_metadata"])
		spanType := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(kind, "agent_action", "agent"), "tool_call", "tool"))
		if metadata["neatlogs.doctor"] != true || metadata["neatlogs.doctor.version"] != "v1" ||
			metadata["service.name"] != "neatlogs.doctor.v2" || metadata["telemetry.sdk.language"] != "go" ||
			metadata["telemetry.sdk.version"] != Version || metadata["neatlogs.span.type"] != spanType {
			metadataValid = false
		}
	}
	if len(byName) != 4 {
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

	result.Probe = &DoctorV2Probe{
		IngestRoute: "/v1/traces", MarkerHeader: "x-neatlogs-doctor", MarkerVersion: "v1",
		Visible: visible, ReadbackSpanCount: readbackSpanCount, HierarchyValid: hierarchyValid,
		AttributesValid: attributesValid, InputOutputValid: inputOutputValid,
		MetadataValid: metadataValid, TypedTokensValid: typedTokensValid,
	}
	validations := []struct {
		name, passCode, remediation, message string
		passed                               bool
	}{
		{"probe_visibility", "TRACE_VISIBLE", "WAIT_FOR_TRACE", "The exact Doctor trace is visible through the authenticated trace API", visible && result.Capture != nil && readbackSpanCount == 4 && len(spans) == 4},
		{"probe_hierarchy", "HIERARCHY_VALID", "CHECK_TRACE_FINALIZER", "The persisted Doctor hierarchy has one root and valid parents", hierarchyValid},
		{"probe_attributes", "ATTRIBUTES_VALID", "CHECK_ATTRIBUTE_MAPPING", "The persisted Doctor span names and types are complete", attributesValid},
		{"probe_input_output", "INPUT_OUTPUT_VALID", "CHECK_PAYLOAD_MAPPING", "The persisted Doctor spans retain input and output", inputOutputValid},
		{"probe_metadata", "METADATA_VALID", "CHECK_METADATA_FINALIZATION", "The versioned Doctor SDK metadata survived finalization", metadataValid},
		{"probe_typed_tokens", "TYPED_TOKENS_VALID", "CHECK_TOKEN_MAPPING", "Persisted token totals remain numeric", typedTokensValid},
	}
	for _, validation := range validations {
		if validation.passed {
			result.Checks = append(result.Checks, DoctorV2Check{Name: validation.name, Status: DoctorPass, ReasonCode: validation.passCode, Message: validation.message, RemediationCode: "NONE"})
		} else {
			result.Checks = append(result.Checks, failV2(validation.name, validation.passCode+"_FAILED", validation.message, validation.remediation))
		}
	}
	return finishDoctorV2(result)
}

func doctorValuesEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
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
	result.Checks = append(result.Checks, failV2("probe_transport", code, message, remediation))
	return finishDoctorV2(result)
}

func decodeLimited(response *http.Response, destination any) error {
	if response == nil || response.Body == nil {
		return errors.New("empty response")
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(destination)
}
