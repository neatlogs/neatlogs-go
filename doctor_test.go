package neatlogs

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDoctorStableLocalResult(t *testing.T) {
	result := Doctor(context.Background(), Config{DisableExport: true, Endpoint: "https://ingest.neatlogs.com"})
	if result.FormatVersion != DoctorFormatVersion || result.SDKVersion != Version || !result.Ready {
		t.Fatalf("unexpected result header: %#v", result)
	}
	wantNames := []string{"runtime", "module", "schema", "transport", "endpoint", "sampler", "ownership", "queue", "export_health", "root"}
	if len(result.Checks) != len(wantNames) {
		t.Fatalf("checks = %d, want %d", len(result.Checks), len(wantNames))
	}
	for index, name := range wantNames {
		if result.Checks[index].Name != name || result.Checks[index].ReasonCode == "" || result.Checks[index].Message == "" {
			t.Fatalf("check[%d] = %#v", index, result.Checks[index])
		}
	}
	if got := doctorCheckNamed(t, result, "queue"); got.Status != DoctorWarn || got.ReasonCode != "EXPORT_QUEUE_DISABLED" {
		t.Fatalf("queue = %#v", got)
	}
	if got := doctorCheckNamed(t, result, "export_health"); got.Status != DoctorUnknown {
		t.Fatalf("export health = %#v", got)
	}
	if got := doctorCheckNamed(t, result, "root"); got.Status != DoctorUnknown {
		t.Fatalf("root = %#v", got)
	}
}

func TestDoctorRejectsEndpointAndSamplerWithoutLeakingInput(t *testing.T) {
	secret := "doctor-secret"
	rate := math.NaN()
	result := Doctor(context.Background(), Config{Endpoint: "https://user:" + secret + "@ingest.neatlogs.com/path?token=" + secret, SampleRate: &rate})
	if result.Ready {
		t.Fatal("invalid configuration reported ready")
	}
	if got := doctorCheckNamed(t, result, "endpoint"); got.Status != DoctorFail || got.ReasonCode != "ENDPOINT_INVALID" {
		t.Fatalf("endpoint = %#v", got)
	}
	if got := doctorCheckNamed(t, result, "sampler"); got.Status != DoctorFail || got.ReasonCode != "SAMPLER_INVALID" {
		t.Fatalf("sampler = %#v", got)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("doctor result leaked endpoint credentials")
	}
}

func TestDoctorObservesSelectedClientHealthAndRoot(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	client, err := NewClient(ctx, Config{Endpoint: "https://ingest.neatlogs.com"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)

	clientCtx := WithClient(ctx, client)
	rootCtx, _, end := Trace(clientCtx, "doctor.root")
	result := Doctor(rootCtx, Config{Endpoint: "https://ingest.neatlogs.com"})
	if !result.Ready {
		t.Fatalf("active client not ready: %#v", result)
	}
	root := doctorCheckNamed(t, result, "root")
	if root.Status != DoctorPass || root.ReasonCode != "ROOT_IDS_VALID" || len(root.Details["trace_id"]) != 32 || len(root.Details["span_id"]) != 16 {
		t.Fatalf("root = %#v", root)
	}
	if health := doctorCheckNamed(t, result, "export_health"); health.Status != DoctorPass || health.ReasonCode != "EXPORT_HEALTHY" {
		t.Fatalf("health = %#v", health)
	}
	end()
}

func TestDoctorReportsExistingExportFailure(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(ctx, Config{Endpoint: "https://ingest.neatlogs.com"}, WithExporter(&secretFailingExporter{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Shutdown(ctx)
	clientCtx := WithClient(ctx, client)
	_, _, end := Trace(clientCtx, "doctor.failure")
	end()
	_ = client.Flush(ctx)

	result := Doctor(clientCtx, Config{Endpoint: "https://ingest.neatlogs.com"})
	check := doctorCheckNamed(t, result, "export_health")
	if result.Ready || check.Status != DoctorFail || check.ReasonCode != "EXPORT_HEALTH_UNHEALTHY" || check.Details["export_failures"] == "0" {
		t.Fatalf("health = %#v, ready=%v", check, result.Ready)
	}
}

func doctorCheckNamed(t *testing.T, result DoctorResult, name string) DoctorCheck {
	t.Helper()
	for _, check := range result.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor check %q missing", name)
	return DoctorCheck{}
}
