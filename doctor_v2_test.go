package neatlogs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDoctorV2CapturesFinalEnvelope(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(ctx, Config{Mask: func(_ context.Context, span SpanData) (*SpanData, error) {
		for index := range span.Attributes {
			if span.Attributes[index].Value.Type() == attribute.STRING && span.Attributes[index].Value.AsString() == "secret" {
				span.Attributes[index] = attribute.String(string(span.Attributes[index].Key), "[MASKED]")
			}
		}
		return &span, nil
	}}, WithExporter(tracetest.NewInMemoryExporter()))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	ctx = client.Context(ctx)
	rootCtx, root, endRoot := Trace(ctx, "doctor.workflow")
	_, _, endChild := StartSpan(rootCtx, "doctor.tool", "tool", attribute.String("test.secret", "secret"))
	endChild()
	endRoot()
	flushStart := time.Now()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	result := DoctorCapturedLocalV2(ctx, DoctorLocalOptions{TraceID: root.SpanContext().TraceID().String(), RootSampleRate: 1, FlushOutcome: "success", FlushTimeout: time.Second, FlushDuration: time.Since(flushStart)})
	if result.FormatVersion != DoctorV2FormatVersion || result.Status != DoctorPass || result.Capture == nil || result.Capture.SpanCount < 2 || result.FirstFailure != nil {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Ownership == nil || result.Ownership.Provider != "private" || result.Ownership.InstrumentorCount != 0 {
		t.Fatalf("manual local doctor must report private ownership with no integration instrumentor: %+v", result.Ownership)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("doctor leaked pre-mask content")
	}
}

func TestDoctorSemanticDigestMatchesCanonicalFixture(t *testing.T) {
	fixture := `{"trace_id":"11111111111111111111111111111111","root_span_id":"2222222222222222","spans":[{"span_id":"2222222222222222","parent_span_id":null,"name":"doctor.workflow","kind":"WORKFLOW","status":"OK","input":{"prompt":"generated diagnostic input"},"output":{"result":"generated diagnostic output"},"sampled":true,"ended":true},{"span_id":"3333333333333333","parent_span_id":"2222222222222222","name":"doctor.llm","kind":"LLM","status":"OK","input":{"messages":[{"role":"user","content":"generated diagnostic input"}]},"output":{"text":"generated diagnostic output"},"choices":[{"index":0,"message":{"role":"assistant","content":"choice zero","tool_calls":[{"id":"doctor_call_1","name":"diagnostic_tool","arguments":{"value":1}}]}},{"index":1,"message":{"role":"assistant","content":"choice one","tool_calls":[]}}],"stream_fragments":["generated ","diagnostic ","output"],"sampled":true,"ended":true},{"span_id":"4444444444444444","parent_span_id":"3333333333333333","name":"doctor.tool","kind":"TOOL","status":"OK","tool_call":{"id":"doctor_call_1","name":"diagnostic_tool","arguments":{"value":1},"result":{"value":2}},"sampled":true,"ended":true},{"span_id":"5555555555555555","parent_span_id":"2222222222222222","name":"doctor.payload","kind":"CHAIN","status":"OK","payload_references":[{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":1024,"mime_type":"application/json"}],"sampled":true,"ended":true}]}`
	var envelope DoctorEnvelope
	if err := json.Unmarshal([]byte(fixture), &envelope); err != nil {
		t.Fatal(err)
	}
	digest, err := DoctorSemanticDigestV2(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:76d8726734664dacaa4e6da4ffc547cc5b7c8edde4721a485b5875378c233381" {
		t.Fatalf("digest=%s", digest)
	}
}

func TestDoctorV2ControlledDefectMatrix(t *testing.T) {
	rootID := "2222222222222222"
	childID := "3333333333333333"
	base := func() DoctorEnvelope {
		return DoctorEnvelope{TraceID: "11111111111111111111111111111111", RootSpanID: &rootID, Spans: []DoctorSpan{
			{SpanID: rootID, Name: "doctor.workflow", Kind: "WORKFLOW", Status: "OK", Sampled: true, Ended: true},
			{SpanID: childID, ParentSpanID: &rootID, Name: "doctor.llm", Kind: "LLM", Status: "OK", Sampled: true, Ended: true},
		}}
	}
	missingParent := "9999999999999999"
	invalidParent := "bad"
	secondRoot := "4444444444444444"
	oneChoice := 2
	cases := []struct {
		name string
		code string
		edit func(*DoctorEnvelope)
	}{
		{"trace ID", "TRACE_ID_INVALID", func(value *DoctorEnvelope) { value.TraceID = "bad" }},
		{"span ID", "SPAN_ID_INVALID", func(value *DoctorEnvelope) { value.Spans[1].SpanID = "bad" }},
		{"duplicate span", "SPAN_ID_DUPLICATE", func(value *DoctorEnvelope) { value.Spans[1].SpanID = rootID }},
		{"sampling", "SAMPLING_INCONSISTENT", func(value *DoctorEnvelope) { value.Spans[1].Sampled = false }},
		{"parent ID", "PARENT_ID_INVALID", func(value *DoctorEnvelope) { value.Spans[1].ParentSpanID = &invalidParent }},
		{"missing parent", "PARENT_MISSING", func(value *DoctorEnvelope) { value.Spans[1].ParentSpanID = &missingParent }},
		{"input JSON", "INPUT_JSON_INVALID", func(value *DoctorEnvelope) { value.Spans[1].Input = func() {} }},
		{"output JSON", "OUTPUT_JSON_INVALID", func(value *DoctorEnvelope) { value.Spans[1].Output = make(chan int) }},
		{"choice loss", "CHOICE_LOSS", func(value *DoctorEnvelope) {
			value.Spans[1].ExpectedChoiceCount = &oneChoice
			value.Spans[1].Choices = []any{map[string]any{"index": 0}}
		}},
		{"stream fragments", "STREAM_FRAGMENT_MISSING", func(value *DoctorEnvelope) { value.Spans[1].Streaming = true }},
		{"payload", "PAYLOAD_ATTACHMENT_REQUIRED", func(value *DoctorEnvelope) { value.Spans[1].Oversized = true }},
		{"missing root", "ROOT_MISSING", func(value *DoctorEnvelope) { value.Spans[0].ParentSpanID = &missingParent }},
		{"multiple roots", "ROOT_MULTIPLE", func(value *DoctorEnvelope) {
			value.Spans = append(value.Spans, DoctorSpan{SpanID: secondRoot, Name: "second", Sampled: true, Ended: true})
		}},
		{"not ended", "ROOT_NOT_ENDED", func(value *DoctorEnvelope) { value.Spans[0].Ended = false }},
		{"tool execution", "TOOL_EXECUTION_MISSING", func(value *DoctorEnvelope) {
			value.Spans[1].Choices = []any{map[string]any{"tool_calls": []any{map[string]any{"id": "call_1"}}}}
		}},
		{"tool request", "TOOL_CALL_MISSING", func(value *DoctorEnvelope) { value.Spans[1].ToolCall = map[string]any{"id": "call_1"} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			value := base()
			test.edit(&value)
			checks := validateDoctorEnvelope(value)
			if len(checks) == 0 || checks[0].ReasonCode != test.code {
				t.Fatalf("checks = %#v, want first reason %s", checks, test.code)
			}
		})
	}
}

func TestDoctorCaptureStoreIsBoundedAndConcurrent(t *testing.T) {
	store := newDoctorCaptureStore(4)
	var wait sync.WaitGroup
	for index := 0; index < 100; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			store.mu.Lock()
			if len(store.order) > store.capacity {
				t.Error("capacity exceeded")
			}
			store.mu.Unlock()
		}()
	}
	wait.Wait()
}

func TestDoctorProbeReadsExactTraceWithoutDiagnosticSession(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("x-api-key") != "local-key" {
			t.Error("missing auth")
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/traces/v3/11111111111111111111111111111111" {
			t.Fatalf("unexpected Doctor request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"_id":"11111111111111111111111111111111","spanCount":4,"promptTokens":11,"completionTokens":7,"totalTokensUsed":18,"spans":[{"span_id":"2222222222222222","parent_span_id":null,"node_name":"doctor.probe.root","node_type":"workflow","data":{"input_value":{},"output_value":{}},"span_metadata":{"neatlogs.doctor":true,"neatlogs.doctor.version":"v1","telemetry.sdk.language":"go"}},{"span_id":"3333333333333333","parent_span_id":"2222222222222222","node_name":"doctor.probe.agent","node_type":"agent_action","data":{"input_value":{},"output_value":{}},"span_metadata":{"neatlogs.doctor":true,"neatlogs.doctor.version":"v1","telemetry.sdk.language":"go"}},{"span_id":"4444444444444444","parent_span_id":"3333333333333333","node_name":"doctor.probe.llm","node_type":"llm","data":{"input_value":{},"output_value":{}},"span_metadata":{"neatlogs.doctor":true,"neatlogs.doctor.version":"v1","telemetry.sdk.language":"go"}},{"span_id":"5555555555555555","parent_span_id":"2222222222222222","node_name":"doctor.probe.tool","node_type":"tool_call","data":{"input_value":{},"output_value":{}},"span_metadata":{"neatlogs.doctor":true,"neatlogs.doctor.version":"v1","telemetry.sdk.language":"go"}}]}`))
	}))
	defer server.Close()
	root := "2222222222222222"
	local := newDoctorV2Result("local")
	local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 4, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", Message: "valid", RemediationCode: "NONE"}}
	result := DoctorProbeV2(context.Background(), local, DoctorProbeOptions{Endpoint: server.URL, APIKey: "local-key", Timeout: time.Second, PollInterval: time.Millisecond})
	if result.Status != DoctorPass || result.FirstFailure != nil || result.Probe == nil || !result.Probe.Visible || !result.Probe.HierarchyValid || !result.Probe.AttributesValid || !result.Probe.InputOutputValid || !result.Probe.MetadataValid || !result.Probe.TypedTokensValid {
		t.Fatalf("probe did not pass exact trace validation: %#v", result)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one exact trace GET", requests)
	}
}

func TestDoctorProbePendingTraceNeverPasses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	root := "2222222222222222"
	local := newDoctorV2Result("local")
	local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 4, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", Message: "valid", RemediationCode: "NONE"}}
	result := DoctorProbeV2(context.Background(), local, DoctorProbeOptions{Endpoint: server.URL, APIKey: "local-key", Timeout: 10 * time.Millisecond, PollInterval: time.Millisecond})
	if result.Status != DoctorFail || result.FirstFailure == nil || *result.FirstFailure != "BACKEND_PROBE_UNAVAILABLE" {
		t.Fatalf("pending trace falsely passed: %#v", result)
	}
}

func TestDoctorProbeRejectsInvalidEndpointBeforeNetwork(t *testing.T) {
	root := "2222222222222222"
	local := newDoctorV2Result("local")
	local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 1, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", Message: "valid", RemediationCode: "NONE"}}
	result := DoctorProbeV2(context.Background(), local, DoctorProbeOptions{Endpoint: "not-a-url", APIKey: "test-key"})
	if result.FirstFailure == nil || *result.FirstFailure != "ENDPOINT_INVALID" {
		t.Fatalf("result = %#v, want ENDPOINT_INVALID", result)
	}
}

func TestDoctorProbeMarkerUsesNormalOTLPRoute(t *testing.T) {
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/traces" {
			t.Errorf("unexpected export request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "project-key" {
			t.Error("normal project-key authentication header is missing")
		}
		if r.Header.Get("x-neatlogs-doctor") != "v1" {
			t.Error("versioned Doctor marker header is missing")
		}
		requests <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := context.Background()
	client, err := NewClient(ctx, Config{APIKey: "project-key", Endpoint: server.URL, WorkflowName: "neatlogs.doctor.v2"}, WithDoctorProbe())
	if err != nil {
		t.Fatal(err)
	}
	clientCtx := client.Context(ctx)
	_, _, end := Trace(clientCtx, "doctor.probe.root")
	end()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("Doctor probe did not export through /v1/traces")
	}
}

func TestDoctorProbeResourceMetadataIsVersioned(t *testing.T) {
	resource := buildResource(context.Background(), Config{WorkflowName: "neatlogs.doctor.v2"}, true)
	values := map[string]any{}
	for _, item := range resource.Attributes() {
		values[string(item.Key)] = doctorAttributeValue(item.Value)
	}
	if values["neatlogs.doctor"] != true || values["neatlogs.doctor.version"] != "v1" || values["telemetry.sdk.language"] != "go" || values["telemetry.sdk.version"] != Version {
		t.Fatalf("Doctor resource metadata = %#v", values)
	}
}

func TestDoctorProbeRejectsRedactedTokenCounts(t *testing.T) {
	if doctorExactNumber("[REDACTED]", 11) {
		t.Fatal("Doctor accepted a redacted token count as numeric")
	}
	if !doctorExactNumber(float64(11), 11) {
		t.Fatal("Doctor rejected the expected numeric token count")
	}
}
