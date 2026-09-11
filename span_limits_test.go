package neatlogs

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The default raised budget: a span carrying more than OTel's 128-attribute
// default must keep every attribute, like the Python and TypeScript SDKs.
func TestSpanLimitsKeepSemanticAttributesBeyondOTelDefault(t *testing.T) {
	sink := tracetest.NewInMemoryExporter()
	client, err := NewClient(
		context.Background(),
		Config{WorkflowName: "span-limits"},
		WithExporter(sink),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(context.Background())

	_, span := client.runtime.provider.Tracer("test").Start(context.Background(), "chat")
	const total = 200
	for i := 0; i < total; i++ {
		span.SetAttributes(attribute.String(fmt.Sprintf("neatlogs.llm.input_messages.%d.content", i), "message"))
	}
	span.SetAttributes(attribute.Int("neatlogs.llm.token_count.total", 507))
	span.End()
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	var chat *tracetest.SpanStub
	for i, s := range sink.GetSpans() {
		if s.Name == "chat" {
			chat = &sink.GetSpans()[i]
		}
	}
	if chat == nil {
		t.Fatal("chat span not exported")
	}
	if chat.DroppedAttributes != 0 {
		t.Fatalf("DroppedAttributes = %d, want 0", chat.DroppedAttributes)
	}
	got := len(chat.Attributes)
	if got != total+2 { // + token_count.total + neatlogs.span.kind
		t.Fatalf("exported %d attributes, want %d (OTel default cap of 128 must not apply)", got, total+2)
	}
}

// Explicit OTel limits via env vars win over the raised default, same as the
// Python SDK.
func TestSpanLimitsRespectExplicitOTelEnv(t *testing.T) {
	t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "200")
	if got := spanLimitsForCaptureEverything().AttributeCountLimit; got != 200 {
		t.Fatalf("AttributeCountLimit = %d, want 200 from OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", got)
	}
	t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "")
	t.Setenv("OTEL_ATTRIBUTE_COUNT_LIMIT", "150")
	if got := spanLimitsForCaptureEverything().AttributeCountLimit; got != 150 {
		t.Fatalf("AttributeCountLimit = %d, want 150 from OTEL_ATTRIBUTE_COUNT_LIMIT", got)
	}
}

// With no explicit env override the raised default applies.
func TestSpanLimitsDefaultRaised(t *testing.T) {
	if got := spanLimitsForCaptureEverything().AttributeCountLimit; got != defaultMaxSpanAttributes {
		t.Fatalf("AttributeCountLimit = %d, want %d", got, defaultMaxSpanAttributes)
	}
}
