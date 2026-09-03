package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	exit := run([]string{"doctor", "--local", "--json"})
	os.Stdout = original
	_ = write.Close()
	payload, readErr := io.ReadAll(read)
	_ = read.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if exit != 0 {
		t.Fatalf("local exit = %d, want 0", exit)
	}
	var result neatlogs.DoctorV2Result
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode Doctor result: %v\n%s", err, payload)
	}
	if result.Capture == nil || result.Capture.SpanCount != 4 {
		t.Fatalf("local capture = %#v, want four spans", result.Capture)
	}
	const controlledFixtureDigest = "sha256:7163d2de42c4165f3ae552279fdde2ec0839413ce608c6e5d71f3fb532df319b"
	if result.Capture.SemanticDigest != controlledFixtureDigest {
		t.Fatalf("controlled fixture digest = %s, want %s", result.Capture.SemanticDigest, controlledFixtureDigest)
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
	if result.Capture != nil {
		t.Fatalf("credential preflight created a misleading capture: %#v", result.Capture)
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
	if result.Capture != nil {
		t.Fatalf("endpoint preflight created a misleading capture: %#v", result.Capture)
	}
}

func TestProbeClassifiesRejectedProjectKeyThroughWriteAndReadRoutes(t *testing.T) {
	const projectKey = "test-project-key-must-not-leak"
	var posts atomic.Int32
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/traces":
			posts.Add(1)
			if request.Header.Get("x-api-key") != projectKey || request.Header.Get("x-neatlogs-doctor") != "v1" {
				t.Errorf("Doctor write headers were incomplete")
			}
			response.WriteHeader(http.StatusUnauthorized)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/traces/v3/"):
			reads.Add(1)
			if request.Header.Get("x-api-key") != projectKey {
				t.Errorf("Doctor read credential was not forwarded")
			}
			response.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("NEATLOGS_API_KEY", projectKey)
	t.Setenv("NEATLOGS_ENDPOINT", server.URL)

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
		t.Fatalf("probe exit = %d, want 3\n%s", exit, payload)
	}
	var result neatlogs.DoctorV2Result
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode Doctor result: %v\n%s", err, payload)
	}
	if posts.Load() == 0 || reads.Load() == 0 {
		t.Fatalf("route coverage: posts=%d reads=%d", posts.Load(), reads.Load())
	}
	if result.FirstFailure == nil || *result.FirstFailure != "AUTH_FAILED" {
		t.Fatalf("first failure = %v, want AUTH_FAILED\n%s", result.FirstFailure, payload)
	}
	if strings.Contains(string(payload), projectKey) {
		t.Fatal("Doctor JSON leaked the project key")
	}
}
