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
	client, err := NewClient(ctx, Config{Mask: func(_ context.Context, span *MaskableSpan) error {
		for index := range span.Attributes {
			if span.Attributes[index].Value.Type() == attribute.STRING && span.Attributes[index].Value.AsString() == "secret" {
				span.Attributes[index] = attribute.String(string(span.Attributes[index].Key), "[MASKED]")
			}
		}
		return nil
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

func TestDoctorProbeRequiresEveryStage(t *testing.T) {
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "local-key" {
			t.Error("missing auth")
		}
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"format_version":"neatlogs.diagnostic-session/v2","diagnostic_id":"diag_0123456789abcdef","probe_token":"dpt_0123456789abcdefghijklmnop","created_at":"2030-01-01T00:00:00Z","expires_at":"2030-01-01T00:05:00Z","fixture_version":"doctor-v2"}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"format_version":"neatlogs.diagnostic-receipt/v2","diagnostic_id":"diag_0123456789abcdef","status":"pending","first_failure":null,"stages":[{"stage":"auth","status":"accepted","reason_code":"AUTH_ACCEPTED","at":"2030-01-01T00:00:01Z"}],"created_at":"2030-01-01T00:00:00Z","expires_at":"2030-01-01T00:05:00Z"}`))
		case http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	root := "2222222222222222"
	local := newDoctorV2Result("local")
	local.Capture = &DoctorV2Capture{TraceID: "11111111111111111111111111111111", RootSpanID: &root, SpanCount: 1, SemanticDigest: "sha256:" + strings.Repeat("a", 64)}
	local.Checks = []DoctorV2Check{{Name: "local_envelope", Status: DoctorPass, ReasonCode: "LOCAL_ENVELOPE_VALID", Message: "valid", RemediationCode: "NONE"}}
	result := DoctorProbeV2(context.Background(), local, DoctorProbeOptions{Endpoint: server.URL, APIKey: "local-key", Timeout: 20 * time.Millisecond, PollInterval: time.Millisecond})
	if result.Status != DoctorFail || result.FirstFailure == nil || *result.FirstFailure != "STAGE_PENDING" || !deleted {
		t.Fatalf("probe falsely passed: %#v deleted=%v", result, deleted)
	}
}
