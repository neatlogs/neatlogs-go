package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	neatlogs "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
)

func TestDoctorSpanAttributesArePersistablePerSpan(t *testing.T) {
	values := map[string]any{}
	for _, item := range doctorSpanAttributes("TOOL") {
		switch item.Value.Type() {
		case attribute.BOOL:
			values[string(item.Key)] = item.Value.AsBool()
		case attribute.STRING:
			values[string(item.Key)] = item.Value.AsString()
		}
	}
	if values["neatlogs.doctor"] != true ||
		values["neatlogs.doctor.version"] != "v1" ||
		values["telemetry.sdk.language"] != "go" ||
		values["neatlogs.span.type"] != "TOOL" {
		t.Fatalf("Doctor span metadata = %#v", values)
	}
}

func TestLocalDoctorUsesIsolatedSyntheticCapture(t *testing.T) {
	t.Setenv("NEATLOGS_API_KEY", "")
	t.Setenv("NEATLOGS_ENDPOINT", "http://127.0.0.1:1")
	if exit := run([]string{"doctor", "--local", "--json"}); exit != 0 {
		t.Fatalf("local exit = %d, want 0", exit)
	}
}

func TestInvalidDoctorInvocationExitsFour(t *testing.T) {
	if exit := run([]string{"doctor", "--local", "--probe"}); exit != 4 {
		t.Fatalf("invalid exit = %d, want 4", exit)
	}
}

func TestProbeWithoutCredentialReportsCanonicalFirstFailure(t *testing.T) {
	t.Setenv("NEATLOGS_API_KEY", "")
	t.Setenv("NEATLOGS_ENDPOINT", "http://127.0.0.1:1")

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	exit := run([]string{"doctor", "--probe", "--json"})
	os.Stdout = original
	_ = write.Close()
	payload, readErr := io.ReadAll(read)
	_ = read.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if exit != 3 {
		t.Fatalf("probe exit = %d, want 3", exit)
	}
	var result neatlogs.DoctorV2Result
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode Doctor result: %v\n%s", err, payload)
	}
	if result.FirstFailure == nil || *result.FirstFailure != "CREDENTIAL_MISSING" {
		t.Fatalf("first failure = %v, want CREDENTIAL_MISSING", result.FirstFailure)
	}
	if result.Capture == nil || result.Capture.SpanCount != 4 {
		t.Fatalf("local capture = %#v, want four spans", result.Capture)
	}
}

func TestProbeRejectsInvalidEndpointBeforeExport(t *testing.T) {
	t.Setenv("NEATLOGS_API_KEY", "test-key")
	t.Setenv("NEATLOGS_ENDPOINT", "not-a-url")

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	exit := run([]string{"doctor", "--probe", "--json"})
	os.Stdout = original
	_ = write.Close()
	payload, readErr := io.ReadAll(read)
	_ = read.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if exit != 3 {
		t.Fatalf("probe exit = %d, want 3", exit)
	}
	var result neatlogs.DoctorV2Result
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode Doctor result: %v\n%s", err, payload)
	}
	if result.FirstFailure == nil || *result.FirstFailure != "ENDPOINT_INVALID" {
		t.Fatalf("first failure = %v, want ENDPOINT_INVALID", result.FirstFailure)
	}
}
