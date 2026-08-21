package neatlogs

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const verificationMarkerAttribute = "neatlogs.verification.marker"

func TestVerificationMarkerResourceReachesGlobalAndClientSpans(t *testing.T) {
	ctx := context.Background()
	const marker = "9e10cc5e-30c1-4bdd-8092-f9542b688f64"
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "existing.resource=value%2Cwith-comma,"+verificationMarkerAttribute+"="+marker)

	globalSink := tracetest.NewInMemoryExporter()
	shutdown, err := Init(ctx, Config{WorkflowName: "marker-global"}, WithExporter(globalSink))
	if err != nil {
		t.Fatal(err)
	}
	_, _, globalEnd := Trace(ctx, "global.marker")
	globalEnd()
	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}
	assertResourceValue(t, byName(globalSink, "global.marker"), verificationMarkerAttribute, marker)
	assertResourceValue(t, byName(globalSink, "global.marker"), "existing.resource", "value,with-comma")
	assertResourceValue(t, byName(globalSink, completionMarkerName), verificationMarkerAttribute, marker)
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	clientSink := tracetest.NewInMemoryExporter()
	client, err := NewClient(ctx, Config{WorkflowName: "marker-client"}, WithExporter(clientSink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	clientCtx := WithClient(ctx, client)
	_, _, clientEnd := Trace(clientCtx, "client.marker")
	clientEnd()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	assertResourceValue(t, byName(clientSink, "client.marker"), verificationMarkerAttribute, marker)
	assertResourceValue(t, byName(clientSink, "client.marker"), "existing.resource", "value,with-comma")
	assertResourceValue(t, byName(clientSink, completionMarkerName), verificationMarkerAttribute, marker)
}

func assertResourceValue(t *testing.T, span tracetest.SpanStub, key, want string) {
	t.Helper()
	if span.Name == "" {
		t.Fatalf("missing span while checking resource %s", key)
	}
	if got := resourceString(span, key); got != want {
		t.Fatalf("resource %s = %q, want %q", key, got, want)
	}
}
