package neatlogs

import (
	"context"
	"strings"
	"testing"
)

// initEndpoint mirrors the Python/TypeScript contract: a resolved endpoint
// must be a bare base URL or an OTLP traces URL ending in /v1/traces. Any
// other path is rejected instead of being silently dropped by the exporter.
func TestInitEndpointPathContract(t *testing.T) {
	accepted := []string{
		"https://ingest.neatlogs.com",
		"https://ingest.example.com/",
		"http://127.0.0.1:4318",
		"https://ingest.example.com/v1/traces",
		"https://ingest.example.com/v1/traces/",
	}
	for _, endpoint := range accepted {
		shutdown, err := Init(context.Background(), Config{
			APIKey:        "nl_test",
			Endpoint:      endpoint,
			DisableExport: true,
		})
		if err != nil {
			t.Fatalf("Init(%q) returned unexpected error: %v", endpoint, err)
		}
		if err := shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown after Init(%q): %v", endpoint, err)
		}
	}

	rejected := []string{
		"http://host/proxy",
		"http://host/proxy/sub",
		"https://ingest.example.com/v1/metrics",
		"not a url",
		"host/proxy",
	}
	for _, endpoint := range rejected {
		_, err := Init(context.Background(), Config{
			APIKey:        "nl_test",
			Endpoint:      endpoint,
			DisableExport: true,
		})
		if err == nil {
			t.Fatalf("Init(%q) succeeded, want endpoint error", endpoint)
		}
		if !strings.Contains(err.Error(), "/v1/traces") {
			t.Fatalf("Init(%q) error %q does not name the /v1/traces contract", endpoint, err)
		}
	}
}
