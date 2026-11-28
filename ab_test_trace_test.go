package neatlogs

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestABAssignmentTagsPrivateRootAndQueuesExplicitClosure(t *testing.T) {
	experiments := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { abRespond(w, abFixture()) })
	if err := experiments.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink := tracetest.NewInMemoryExporter()
	telemetry, err := NewClient(context.Background(), Config{WorkflowName: "ab-test"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	ctx := WithABTestClient(telemetry.Context(context.Background()), experiments)
	ctx = Identify(ctx, IdentifyOptions{SessionID: "session:42"})
	ctx, root, endRoot := Trace(ctx, "reply")
	ctx, _, endChild := StartSpan(ctx, "answer", "LLM")
	result := ABTest(ctx, abTestKey, ABAssignmentOptions{Unit: "session:42"})
	if result.Arm != "b" {
		t.Fatalf("assignment %+v", result)
	}
	endChild()
	endRoot()
	if err := telemetry.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if experiments.Diagnostics().Pending != 1 {
		t.Fatal("assignment did not queue one exposure")
	}
	if !CompleteABTestUnit(ctx, abTestKey, ABExposureOptions{UnitID: "session:42", Arm: "b", TraceID: root.SpanContext().TraceID().String()}, "sdk") {
		t.Fatal("closure was not queued")
	}
	if experiments.Diagnostics().Pending != 2 {
		t.Fatal("completion must be explicit and separate")
	}
	found := false
	for _, span := range sink.GetSpans() {
		if span.SpanContext.SpanID() != root.SpanContext().SpanID() {
			continue
		}
		for _, attr := range span.Attributes {
			if string(attr.Key) == "neatlogs.ab."+abTestKey && attr.Value.AsString() == "b" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("private trace root was not tagged")
	}
}

func TestABChooseRecordsOnlyStartedRuns(t *testing.T) {
	experiments := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { abRespond(w, abFixture()) })
	if err := experiments.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink := tracetest.NewInMemoryExporter()
	telemetry, err := NewClient(context.Background(), Config{WorkflowName: "ab-test"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	ctx := WithABTestClient(telemetry.Context(context.Background()), experiments)
	abandoned := ChooseABTest(ctx, abTestKey, ABAssignmentOptions{Unit: "abandoned"})
	if abandoned.Reason != "assigned" || experiments.Diagnostics().Pending != 0 {
		t.Fatal("choosing must not record traffic")
	}
	if RecordABTestRun(ctx, abandoned) {
		t.Fatal("a run without a trace cannot be recorded")
	}
	ctx = Identify(ctx, IdentifyOptions{SessionID: "session:42"})
	ctx, root, end := Trace(ctx, "failed-agent")
	decision := ChooseABTest(ctx, abTestKey, ABAssignmentOptions{Unit: "session:42"})
	if !RecordABTestRun(ctx, decision) {
		t.Fatal("started run was not recorded")
	}
	end() // An agent error still leaves an exported trace and exposure.
	if err := telemetry.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !CompleteABTestDecision(ctx, decision, root.SpanContext().TraceID().String()) {
		t.Fatal("completion was not queued")
	}
	if experiments.Diagnostics().Pending != 2 {
		t.Fatal("expected one run and one completion")
	}
}
