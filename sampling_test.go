package neatlogs

import (
	"context"
	"math"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSampleRateValidationIsStrict(t *testing.T) {
	tests := []struct {
		name string
		rate float64
	}{
		{name: "negative", rate: -0.0001},
		{name: "above one", rate: 1.0001},
		{name: "nan", rate: math.NaN()},
		{name: "positive infinity", rate: math.Inf(1)},
		{name: "negative infinity", rate: math.Inf(-1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewClient(context.Background(), Config{
				DisableExport: true,
				SampleRate:    &test.rate,
			})
			if err == nil || !strings.Contains(err.Error(), "sample rate must be finite and between 0 and 1") {
				t.Fatalf("NewClient error = %v", err)
			}
		})
	}
}

func TestInvalidGlobalSampleRateDoesNotClaimLifecycle(t *testing.T) {
	ctx := context.Background()
	invalid := 2.0
	if _, err := Init(ctx, Config{DisableExport: true, SampleRate: &invalid}); err == nil {
		t.Fatal("Init accepted invalid sample rate")
	}
	valid := 1.0
	shutdown, err := Init(ctx, Config{DisableExport: true, SampleRate: &valid})
	if err != nil {
		t.Fatalf("valid Init after rejected config: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSampleRateBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name      string
		rate      float64
		wantSpans int
	}{
		{name: "zero drops root", rate: 0, wantSpans: 0},
		{name: "one records root", rate: 1, wantSpans: 2}, // root + completion marker
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := tracetest.NewInMemoryExporter()
			client, err := NewClient(ctx, Config{WorkflowName: "sampling", SampleRate: &test.rate}, WithExporter(sink))
			if err != nil {
				t.Fatal(err)
			}
			_, span, end := Trace(WithClient(ctx, client), "sampling.root")
			if span.IsRecording() != (test.wantSpans > 0) {
				t.Fatalf("span IsRecording = %v", span.IsRecording())
			}
			end()
			defer client.Shutdown(ctx)
			if err := client.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			if got := len(sink.GetSpans()); got != test.wantSpans {
				t.Fatalf("exported spans = %d, want %d", got, test.wantSpans)
			}
		})
	}
}

func TestParentBasedSamplerHonorsRemoteParentDecision(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name       string
		traceFlags string
		rootRate   float64
		wantRecord bool
	}{
		{name: "sampled remote overrides zero root rate", traceFlags: "01", rootRate: 0, wantRecord: true},
		{name: "unsampled remote overrides one root rate", traceFlags: "00", rootRate: 1, wantRecord: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := tracetest.NewInMemoryExporter()
			client, err := NewClient(ctx, Config{WorkflowName: "remote-parent", SampleRate: &test.rootRate}, WithExporter(sink))
			if err != nil {
				t.Fatal(err)
			}
			headers := http.Header{}
			headers.Set("traceparent", "00-11111111111111111111111111111111-2222222222222222-"+test.traceFlags)
			remoteCtx := ExtractTraceContext(ctx, propagation.HeaderCarrier(headers))
			_, span, end := Trace(WithClient(remoteCtx, client), "remote.child")
			if span.IsRecording() != test.wantRecord {
				t.Fatalf("span IsRecording = %v, want %v", span.IsRecording(), test.wantRecord)
			}
			end()
			defer client.Shutdown(ctx)
			if err := client.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			if got := len(sink.GetSpans()); (got > 0) != test.wantRecord {
				t.Fatalf("exported spans = %d, wantRecord %v", got, test.wantRecord)
			}
			if test.wantRecord {
				child := byName(sink, "remote.child")
				if child.Parent.SpanID().String() != "2222222222222222" {
					t.Fatalf("remote parent span ID = %s", child.Parent.SpanID())
				}
			}
		})
	}
}

func TestClientsOwnIndependentParentBasedSamplers(t *testing.T) {
	ctx := context.Background()
	dropRate, recordRate := 0.0, 1.0
	dropSink := tracetest.NewInMemoryExporter()
	recordSink := tracetest.NewInMemoryExporter()
	dropClient, err := NewClient(ctx, Config{WorkflowName: "drop", SampleRate: &dropRate}, WithExporter(dropSink))
	if err != nil {
		t.Fatal(err)
	}
	recordClient, err := NewClient(ctx, Config{WorkflowName: "record", SampleRate: &recordRate}, WithExporter(recordSink))
	if err != nil {
		t.Fatal(err)
	}

	dropCtx, dropRoot, dropEnd := Trace(WithClient(ctx, dropClient), "drop.root")
	_, dropChild, dropChildEnd := StartSpan(dropCtx, "drop.child", "TOOL")
	if dropRoot.IsRecording() || dropChild.IsRecording() {
		t.Fatal("drop client did not preserve unsampled parent decision")
	}
	dropChildEnd()
	dropEnd()
	recordCtx, recordRoot, recordEnd := Trace(WithClient(ctx, recordClient), "record.root")
	_, recordChild, recordChildEnd := StartSpan(recordCtx, "record.child", "TOOL")
	if !recordRoot.IsRecording() || !recordChild.IsRecording() {
		t.Fatal("recording client did not preserve sampled parent decision")
	}
	recordChildEnd()
	recordEnd()
	defer dropClient.Shutdown(ctx)
	defer recordClient.Shutdown(ctx)
	if err := dropClient.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := recordClient.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(dropSink.GetSpans()); got != 0 {
		t.Fatalf("drop client exported %d spans", got)
	}
	if byName(recordSink, "record.root").Name == "" || byName(recordSink, "record.child").Name == "" {
		t.Fatal("record client did not export its root and child")
	}
}
