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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
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
	}}, WithExporter(tracetest.NewInMemoryExporter()), WithDoctorProbe())
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

func TestOrdinaryRuntimeAllocatesNoDoctorRetention(t *testing.T) {
	client, err := NewClient(context.Background(), Config{}, WithExporter(tracetest.NewInMemoryExporter()))
	if err != nil {
		t.Fatal(err)
	}
	if client.runtime.captures != nil {
		t.Fatal("ordinary SDK runtime allocated Doctor capture storage")
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorRuntimeClearsCaptureOnShutdown(t *testing.T) {
	client, err := NewClient(context.Background(), Config{}, WithExporter(tracetest.NewInMemoryExporter()), WithDoctorProbe())
	if err != nil {
		t.Fatal(err)
	}
	store := client.runtime.captures
	ctx := client.Context(context.Background())
	_, _, end := Trace(ctx, "doctor.cleanup")
	end()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.byTrace) == 0 {
		t.Fatal("Doctor capture did not receive the exported span")
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.byTrace) != 0 || store.totalBytes != 0 {
		t.Fatal("Doctor capture survived runtime shutdown")
	}
}

func TestEmittedSpanCaptureDerivesChoicesStreamToolAndPayloadFields(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(ctx, Config{}, WithExporter(tracetest.NewInMemoryExporter()), WithDoctorProbe())
	if err != nil {
		t.Fatal(err)
	}
	ctx = client.Context(ctx)
	rootCtx, llm, endLLM := StartSpan(ctx, "doctor.llm", "llm",
		attribute.String("neatlogs.llm.input", `{"prompt":"hello"}`),
		attribute.String("neatlogs.llm.output", `{"text":"done"}`),
		attribute.String("neatlogs.llm.output_messages.0.content", "first"),
		attribute.String("neatlogs.llm.output_messages.1.content", "second"),
		attribute.String("neatlogs.llm.choices.0.finish_reason", "tool_calls"),
		attribute.String("neatlogs.llm.choices.1.finish_reason", "stop"),
		attribute.String("neatlogs.llm.tool_calls.0.id", "doctor_call_1"),
		attribute.String("neatlogs.llm.tool_calls.0.name", "diagnostic_tool"),
		attribute.String("neatlogs.llm.tool_calls.0.arguments", `{"value":1}`),
		attribute.Int("neatlogs.llm.tool_calls.0.choice_index", 0),
		attribute.Bool("neatlogs.llm.is_streaming", true),
		attribute.Bool("neatlogs.capture.truncated", true),
		attribute.String("neatlogs.llm.output.media.0.sha256", strings.Repeat("a", 64)),
		attribute.Int("neatlogs.llm.output.media.0.byte_length", 1024),
		attribute.String("neatlogs.llm.output.media.0.mime_type", "application/json"),
	)
	traceID := llm.SpanContext().TraceID().String()
	llm.AddEvent("neatlogs.stream.chunk", trace.WithAttributes(
		attribute.String("neatlogs.stream.chunk.summary", `{"text":"done"}`),
	))
	_, _, endTool := StartSpan(rootCtx, "doctor.tool", "tool",
		attribute.String("neatlogs.tool_call.id", "doctor_call_1"),
		attribute.String("neatlogs.tool.name", "diagnostic_tool"),
		attribute.String("neatlogs.tool.input", `{"value":1}`),
		attribute.String("neatlogs.tool.output", `{"value":2}`),
	)
	endTool()
	endLLM()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	envelope, ok := client.runtime.captures.envelope(traceID)
	if !ok {
		t.Fatal("Doctor capture missing")
	}
	byName := make(map[string]DoctorSpan)
	for _, span := range envelope.Spans {
		byName[span.Name] = span
	}
	projected := byName["doctor.llm"]
	if projected.ExpectedChoiceCount == nil || *projected.ExpectedChoiceCount != 2 || doctorCollectionLength(projected.Choices) != 2 {
		t.Fatalf("choice projection = %#v", projected)
	}
	if !projected.Streaming || doctorCollectionLength(projected.StreamFragments) != 1 || !projected.Oversized || !doctorHasValidPayloadReference(projected.PayloadReferences) {
		t.Fatalf("stream/payload projection = %#v", projected)
	}
	tool, ok := byName["doctor.tool"].ToolCall.(map[string]any)
	if !ok || tool["id"] != "doctor_call_1" || !doctorValuesEqual(tool["arguments"], map[string]any{"value": float64(1)}) || !doctorValuesEqual(tool["result"], map[string]any{"value": float64(2)}) {
		t.Fatalf("tool projection = %#v", byName["doctor.tool"].ToolCall)
	}
	result := DoctorCapturedLocalV2(ctx, DoctorLocalOptions{TraceID: traceID, RootSampleRate: 1, FlushOutcome: "success", FlushTimeout: time.Second})
	if result.Status != DoctorPass {
		t.Fatalf("actual emitted projection failed Doctor validation: %#v", result)
	}
	if err := client.Shutdown(ctx); err != nil {
		t.Fatal(err)
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
	if digest != "sha256:824650f5fbc6d9f8d92381356411609263417219eaf7fdafbd2ba94795b6c4f7" {
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

func TestDoctorCaptureStoreEnforcesSpanAndByteBoundsAndClears(t *testing.T) {
	store := newDoctorCaptureStore(4)
	spans := make([]sdktrace.ReadOnlySpan, 0, 80)
	for index := 0; index < 80; index++ {
		spans = append(spans, tracetest.SpanStub{
			Name: "doctor.span",
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{1}, SpanID: trace.SpanID{byte(index + 1)}, TraceFlags: trace.FlagsSampled,
			}),
			Attributes: []attribute.KeyValue{attribute.String("neatlogs.span.kind", "tool")},
			EndTime:    time.Now(),
		}.Snapshot())
	}
	store.capture(spans)
	envelope, ok := store.envelope(trace.TraceID{1}.String())
	if !ok || len(envelope.Spans) != store.maxSpansPerTrace {
		t.Fatalf("captured spans = %d, want %d", len(envelope.Spans), store.maxSpansPerTrace)
	}
	large := tracetest.SpanStub{
		Name: "doctor.large",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: trace.TraceID{2}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
		}),
		Attributes: []attribute.KeyValue{attribute.String("neatlogs.input.value", strings.Repeat("x", 300*1024))},
		EndTime:    time.Now(),
	}.Snapshot()
	store.capture([]sdktrace.ReadOnlySpan{large})
	if _, ok := store.envelope(trace.TraceID{2}.String()); ok {
		t.Fatal("oversized Doctor span was retained")
	}
	store.clear()
	if store.totalBytes != 0 || len(store.byTrace) != 0 || len(store.order) != 0 {
		t.Fatal("Doctor capture was not cleared")
	}
}

func doctorPersistedSpan(id, parent, name, kind string, input, output any) map[string]any {
	spanType := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(kind, "agent_action", "agent"), "tool_call", "tool"))
	span := map[string]any{
		"span_id": id, "node_name": name, "node_type": kind,
		"data": map[string]any{"input_value": input, "output_value": output},
		"span_metadata": map[string]any{
			"neatlogs.doctor": true, "neatlogs.doctor.version": "v1",
			"service.name": "neatlogs.doctor.v2", "telemetry.sdk.language": "go",
			"telemetry.sdk.version": Version, "neatlogs.span.type": spanType,
		},
	}
	if parent != "" {
		span["parent_span_id"] = parent
	}
	return span
}

func doctorPersistedTraceFixture() map[string]any {
	return map[string]any{
		"_id": "11111111111111111111111111111111", "status": "success", "spanCount": 4,
		"promptTokens": 11, "completionTokens": 7, "totalTokensUsed": 18,
		"spans": []any{
			doctorPersistedSpan("2222222222222222", "", "doctor.probe.root", "workflow", map[string]any{"prompt": "generated diagnostic input"}, map[string]any{"result": map[string]any{"value": 2}}),
			doctorPersistedSpan("3333333333333333", "2222222222222222", "doctor.probe.agent", "agent_action", map[string]any{"prompt": "generated diagnostic input"}, map[string]any{"text": "generated diagnostic output"}),
			doctorPersistedSpan("4444444444444444", "3333333333333333", "doctor.probe.llm", "llm", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "generated diagnostic input"}}}, map[string]any{"text": "generated diagnostic output"}),
			doctorPersistedSpan("5555555555555555", "2222222222222222", "doctor.probe.tool", "tool_call", map[string]any{"value": 1}, map[string]any{"value": 2}),
		},
	}
}

func doctorV3MaterializedTraceFixture() map[string]any {
	fixture := doctorPersistedTraceFixture()
	spans := fixture["spans"].([]any)
	spans[0].(map[string]any)["data"] = map[string]any{
		"input_value": "generated diagnostic input", "output_value": "Value: 2",
	}
	spans[2].(map[string]any)["data"] = map[string]any{
		"input_value":  map[string]any{"prompt": "generated diagnostic input"},
		"output_value": "Text: generated diagnostic output",
	}
	spans[1].(map[string]any)["data"] = map[string]any{
		"input_value": "Prompt: generated diagnostic input", "output_value": "Text: generated diagnostic output",
	}
	spans[3].(map[string]any)["data"] = map[string]any{
		"input_value": "Value: 1", "output_value": "Value: 2",
	}
	return fixture
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
		_ = json.NewEncoder(w).Encode(doctorV3MaterializedTraceFixture())
	}))
	defer server.Close()
	root := "2222222222222222"
	local := newDoctorV2Result("local")
	local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 4, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", Message: "valid", RemediationCode: "NONE"}}
	result := DoctorProbeV2(context.Background(), local, DoctorProbeOptions{Endpoint: server.URL, APIKey: "local-key", Timeout: time.Second, PollInterval: time.Millisecond})
	if result.Status != DoctorPass || result.FirstFailure != nil || result.Probe == nil || !result.Probe.Visible || !result.Probe.Finalized || result.Probe.ReadbackTraceID != result.Capture.TraceID || result.Probe.MeaningfulRoots != 1 || result.Probe.DuplicateSpans != 0 || !result.Probe.HierarchyValid || !result.Probe.AttributesValid || !result.Probe.InputOutputValid || !result.Probe.MetadataValid || !result.Probe.TypedTokensValid {
		t.Fatalf("probe did not pass exact trace validation: %#v", result)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want one exact trace GET", requests)
	}
}

func TestDoctorProbeRejectsWrongMaterializedInputOutput(t *testing.T) {
	root := "2222222222222222"
	local := newDoctorV2Result("local")
	local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 4, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", RemediationCode: "NONE"}}
	fixture := doctorV3MaterializedTraceFixture()
	fixture["spans"].([]any)[2].(map[string]any)["data"].(map[string]any)["output_value"] = "Text: wrong output"
	result := persistedDoctorProbeResult(local, fixture)
	if result.Status != DoctorFail || result.Probe == nil || result.Probe.InputOutputValid || result.FirstFailure == nil || *result.FirstFailure != "INPUT_OUTPUT_VALID_FAILED" {
		t.Fatalf("wrong materialized output passed: %#v", result)
	}
}

func TestDoctorProbeReportsTerminalCorrelationRootsAndDuplicates(t *testing.T) {
	root := "2222222222222222"
	local := newDoctorV2Result("local")
	local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 4, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", RemediationCode: "NONE"}}
	fixture := doctorV3MaterializedTraceFixture()
	spans := fixture["spans"].([]any)
	spans[3].(map[string]any)["span_id"] = spans[1].(map[string]any)["span_id"]
	result := persistedDoctorProbeResult(local, fixture)
	if result.Status != DoctorFail || result.Probe == nil || result.Probe.ReadbackTraceID != result.Capture.TraceID || !result.Probe.Finalized || result.Probe.MeaningfulRoots != 1 || result.Probe.DuplicateSpans != 1 || result.Probe.HierarchyValid {
		t.Fatalf("correlation fields did not expose duplicate: %#v", result)
	}
}

func TestDoctorProbeRejectsFailedOrErrorTerminalReadback(t *testing.T) {
	root := "2222222222222222"
	for _, terminalStatus := range []string{"failed", "error", "completed"} {
		t.Run(terminalStatus, func(t *testing.T) {
			local := newDoctorV2Result("local")
			local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 4, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
			local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", RemediationCode: "NONE"}}
			fixture := doctorV3MaterializedTraceFixture()
			fixture["status"] = terminalStatus
			result := persistedDoctorProbeResult(local, fixture)
			if result.Status != DoctorFail || result.Probe == nil || result.Probe.Finalized || result.FirstFailure == nil || *result.FirstFailure != "TRACE_FINALIZED_FAILED" {
				t.Fatalf("failed terminal status passed: %#v", result)
			}
		})
	}
}

func TestDoctorProbeRejectsWrongEdgesExtrasAndIncompleteMetadata(t *testing.T) {
	root := "2222222222222222"
	base := func() DoctorV2Result {
		result := newDoctorV2Result("local")
		result.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 4, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
		result.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", RemediationCode: "NONE"}}
		return result
	}

	wrongEdge := doctorPersistedTraceFixture()
	wrongSpans := wrongEdge["spans"].([]any)
	wrongSpans[2].(map[string]any)["parent_span_id"] = root
	if result := persistedDoctorProbeResult(base(), wrongEdge); result.Probe == nil || result.Probe.HierarchyValid || result.Status != DoctorFail {
		t.Fatalf("wrong parent edge passed: %#v", result)
	}

	extra := doctorPersistedTraceFixture()
	extra["spanCount"] = 5
	extra["spans"] = append(extra["spans"].([]any), doctorPersistedSpan("6666666666666666", root, "doctor.probe.extra", "tool_call", map[string]any{}, map[string]any{}))
	if result := persistedDoctorProbeResult(base(), extra); result.Probe == nil || result.Probe.ReadbackSpanCount != 5 || result.Status != DoctorFail {
		t.Fatalf("unexpected span passed: %#v", result)
	}

	incomplete := doctorPersistedTraceFixture()
	delete(incomplete["spans"].([]any)[0].(map[string]any)["span_metadata"].(map[string]any), "telemetry.sdk.version")
	if result := persistedDoctorProbeResult(base(), incomplete); result.Probe == nil || result.Probe.MetadataValid || result.Status != DoctorFail {
		t.Fatalf("incomplete metadata passed: %#v", result)
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
