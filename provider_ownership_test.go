package neatlogs

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const foreignProviderHelperEnv = "NEATLOGS_TEST_FOREIGN_PROVIDER_HELPER"

func TestPrivateProvidersRemainIsolatedFromForeignGlobalProvider(t *testing.T) {
	if os.Getenv(foreignProviderHelperEnv) == "1" {
		runForeignProviderIsolationAssertions(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrivateProvidersRemainIsolatedFromForeignGlobalProvider$")
	cmd.Env = append(os.Environ(), foreignProviderHelperEnv+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("foreign-provider helper failed: %v\n%s", err, output)
	}
}

func runForeignProviderIsolationAssertions(t *testing.T) {
	ctx := context.Background()
	foreignSink := tracetest.NewInMemoryExporter()
	foreignProvider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(foreignSink))
	otel.SetTracerProvider(foreignProvider)
	defer foreignProvider.Shutdown(ctx)

	globalSink := tracetest.NewInMemoryExporter()
	shutdown, err := Init(ctx, Config{WorkflowName: "private-global"}, WithExporter(globalSink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)
	clientSink := tracetest.NewInMemoryExporter()
	client, err := NewClient(ctx, Config{WorkflowName: "private-client"}, WithExporter(clientSink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)

	foreignCtx, foreignSpan := otel.Tracer("foreign-owner").Start(ctx, "foreign.span")
	_, _, globalEnd := Trace(foreignCtx, "neatlogs.global")
	globalEnd()
	_, _, clientEnd := Trace(WithClient(foreignCtx, client), "neatlogs.client")
	clientEnd()
	foreignSpan.End()

	if err := Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := foreignProvider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}

	if got := otel.GetTracerProvider(); got != foreignProvider {
		t.Fatal("Neatlogs replaced the foreign global TracerProvider")
	}
	if byName(foreignSink, "foreign.span").Name == "" {
		t.Fatal("foreign provider did not retain its own span")
	}
	for _, name := range []string{"neatlogs.global", "neatlogs.client", completionMarkerName} {
		if byName(foreignSink, name).Name != "" {
			t.Fatalf("foreign provider received private span %q", name)
		}
	}
	if byName(globalSink, "foreign.span").Name != "" || byName(clientSink, "foreign.span").Name != "" {
		t.Fatal("private provider consumed foreign global span")
	}
	globalSpan := byName(globalSink, "neatlogs.global")
	clientSpan := byName(clientSink, "neatlogs.client")
	if globalSpan.Name == "" || clientSpan.Name == "" {
		t.Fatal("private providers did not export their own spans")
	}
	if globalSpan.Parent.IsValid() || clientSpan.Parent.IsValid() {
		t.Fatal("private provider adopted foreign active span as parent")
	}
	if globalSpan.SpanContext.TraceID() == clientSpan.SpanContext.TraceID() {
		t.Fatal("global Init and context-scoped Client shared a trace")
	}
}
