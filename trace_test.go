package neatlogs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func attrString(kvs []attribute.KeyValue, key string) (string, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}

func byName(sink *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	for _, s := range sink.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	return tracetest.SpanStub{}
}

// Trace stamps session + end-user identity (bound on ctx via Identify) on the
// WORKFLOW root.
func TestTrace_StampsIdentityOnRoot(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	ctx = Identify(ctx, IdentifyOptions{
		SessionID:       "chat_123",
		EndUserID:       "user_456",
		EndUserMetadata: map[string]any{"plan": "pro"},
	})
	_, _, end := Trace(ctx, "chat_turn")
	end()
	Flush(ctx)

	root := byName(sink, "chat_turn")
	if root.Name != "chat_turn" {
		t.Fatal("expected a chat_turn workflow span")
	}
	if v, _ := attrString(root.Attributes, attrs.SessionID); v != "chat_123" {
		t.Errorf("session.id = %q, want chat_123", v)
	}
	if v, _ := attrString(root.Attributes, attrs.EndUserID); v != "user_456" {
		t.Errorf("end_user.id = %q, want user_456", v)
	}
	if v, _ := attrString(root.Attributes, attrs.EndUserMetadata); v != `{"plan":"pro"}` {
		t.Errorf("end_user.metadata = %q, want JSON", v)
	}
}

// Calling Identify again overrides individual fields without clearing the others.
func TestIdentify_OverridesPerField(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	ctx = Identify(ctx, IdentifyOptions{SessionID: "sess_a", EndUserID: "user_a"})
	ctx2 := Identify(ctx, IdentifyOptions{SessionID: "sess_b"}) // session only

	_, _, end := Trace(ctx2, "t")
	end()
	Flush(ctx)

	root := byName(sink, "t")
	if v, _ := attrString(root.Attributes, attrs.SessionID); v != "sess_b" {
		t.Errorf("session = %q, want sess_b (overridden)", v)
	}
	if v, _ := attrString(root.Attributes, attrs.EndUserID); v != "user_a" {
		t.Errorf("end_user = %q, want user_a (preserved)", v)
	}
}

// Identity is root-only: a Trace nested inside a recording span carries nothing.
func TestTrace_NestedChildNotStamped(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	ctx = Identify(ctx, IdentifyOptions{SessionID: "chat_1", EndUserID: "u_1"})
	rootCtx, _, endRoot := Trace(ctx, "root")
	_, _, endChild := Trace(rootCtx, "child")
	endChild()
	endRoot()
	Flush(ctx)

	child := byName(sink, "child")
	if _, ok := attrString(child.Attributes, attrs.SessionID); ok {
		t.Error("child span must not carry session.id")
	}
	if _, ok := attrString(child.Attributes, attrs.EndUserID); ok {
		t.Error("child span must not carry end_user.id")
	}
	root := byName(sink, "root")
	if v, _ := attrString(root.Attributes, attrs.SessionID); v != "chat_1" {
		t.Errorf("root session = %q, want chat_1", v)
	}
}

// No identity bound → root carries neither attribute.
func TestTrace_NoIdentitySet(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	_, _, end := Trace(ctx, "bare")
	end()
	Flush(ctx)

	root := byName(sink, "bare")
	if _, ok := attrString(root.Attributes, attrs.SessionID); ok {
		t.Error("expected no session.id when none set")
	}
	if _, ok := attrString(root.Attributes, attrs.EndUserID); ok {
		t.Error("expected no end_user.id when none set")
	}
}

// A bare provider span (auto-root) picks up identity bound on its context.
func TestAutoRoot_StampsContextIdentity(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	ctx = Identify(ctx, IdentifyOptions{
		SessionID:       "ctx_session",
		EndUserID:       "ctx_user",
		EndUserMetadata: map[string]any{"plan": "pro"},
	})
	_, span, end := startProviderSpan(ctx, "google_genai.models.generate_content", attrs.KindLLM)
	span.SetAttributes(attribute.String(attrs.SpanKind, attrs.KindLLM))
	end()
	Flush(ctx)

	var root tracetest.SpanStub
	var found bool
	for _, s := range sink.GetSpans() {
		if spanKindOf(s.Attributes) == attrs.KindWorkflow && !s.Parent.IsValid() {
			root = s
			found = true
		}
	}
	if !found {
		t.Fatal("expected an auto-root workflow span")
	}
	if v, _ := attrString(root.Attributes, attrs.SessionID); v != "ctx_session" {
		t.Errorf("auto-root session = %q, want ctx_session", v)
	}
	if v, _ := attrString(root.Attributes, attrs.EndUserID); v != "ctx_user" {
		t.Errorf("auto-root end_user = %q, want ctx_user", v)
	}
	if v, _ := attrString(root.Attributes, attrs.EndUserMetadata); v != `{"plan":"pro"}` {
		t.Errorf("auto-root end_user.metadata = %q, want JSON", v)
	}
}

// A raw OTel root span (the ADK passthrough case — created directly via the
// global tracer, bypassing Trace() and the WrapGenAI auto-root) still picks up
// identity from Identify(ctx), via the identityProcessor.
func TestIdentityProcessor_StampsRawOTelRoot(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	sd, err := Init(ctx, Config{WorkflowName: "wf"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer sd(ctx)

	ctx = Identify(ctx, IdentifyOptions{
		SessionID:       "adk_session",
		EndUserID:       "adk_user",
		EndUserMetadata: map[string]any{"plan": "pro"},
	})

	// Simulate ADK: a span started straight off the global provider, NOT via
	// neatlogs.Trace or a provider wrapper.
	_, span := otel.Tracer("gcp.vertex.agent").Start(ctx, "adk.invoke")
	span.End()
	Flush(ctx)

	root := byName(sink, "adk.invoke")
	if root.Name != "adk.invoke" {
		t.Fatal("expected the adk.invoke span")
	}
	if v, _ := attrString(root.Attributes, attrs.SessionID); v != "adk_session" {
		t.Errorf("adk root session = %q, want adk_session", v)
	}
	if v, _ := attrString(root.Attributes, attrs.EndUserID); v != "adk_user" {
		t.Errorf("adk root end_user = %q, want adk_user", v)
	}
	if v, _ := attrString(root.Attributes, attrs.EndUserMetadata); v != `{"plan":"pro"}` {
		t.Errorf("adk root end_user.metadata = %q, want JSON", v)
	}
}
