package neatlogs

import (
	"context"
	"math"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func float64Pointer(value float64) *float64 { return &value }

func TestInitRejectsInvalidSampleRate(t *testing.T) {
	for _, rate := range []float64{-0.01, 1.01, math.NaN(), math.Inf(1)} {
		_, err := Init(
			context.Background(),
			Config{WorkflowName: "wf", SampleRate: float64Pointer(rate)},
			WithExporter(tracetest.NewInMemoryExporter()),
		)
		if err == nil {
			t.Fatalf("sample rate %v: expected validation error", rate)
		}
	}
}

func TestSampleRateZeroDropsTheWholeTrace(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := Init(
		ctx,
		Config{WorkflowName: "wf", SampleRate: float64Pointer(0)},
		WithExporter(sink),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	childCtx, _, endRoot := Trace(ctx, "root")
	_, _, endChild := StartSpan(childCtx, "child", "tool")
	endChild()
	endRoot()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if got := len(sink.GetSpans()); got != 0 {
		t.Fatalf("sample rate 0 exported %d spans, want 0", got)
	}
}

func TestSampleRateOneKeepsTheWholeTrace(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := Init(
		ctx,
		Config{WorkflowName: "wf", SampleRate: float64Pointer(1)},
		WithExporter(sink),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	childCtx, _, endRoot := Trace(ctx, "root")
	_, _, endChild := StartSpan(childCtx, "child", "tool")
	endChild()
	endRoot()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}

	spans := sink.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("sample rate 1 exported %d spans, want root, child, and completion marker", len(spans))
	}
}
