package main

import (
	"encoding/json"
	"io"
	"os"
	"testing"

	neatlogs "github.com/neatlogs/neatlogs-go"
)

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
	if result.Capture == nil || result.Capture.SpanCount != 3 {
		t.Fatalf("local capture = %#v, want three spans", result.Capture)
	}
}
