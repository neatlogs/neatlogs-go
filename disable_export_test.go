package neatlogs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// NEATLOGS_DISABLE_EXPORT must disable export the same way it does in the
// Python and TypeScript SDKs: "true", "1", or "yes" (case-insensitive).
func TestDisableExportEnvSet(t *testing.T) {
	for _, value := range []string{"true", "1", "yes", "TRUE", "Yes"} {
		t.Setenv("NEATLOGS_DISABLE_EXPORT", value)
		if !disableExportEnvSet() {
			t.Fatalf("NEATLOGS_DISABLE_EXPORT=%q: disableExportEnvSet() = false, want true", value)
		}
	}
	for _, value := range []string{"", "false", "0", "no", "2", "enabled"} {
		t.Setenv("NEATLOGS_DISABLE_EXPORT", value)
		if disableExportEnvSet() {
			t.Fatalf("NEATLOGS_DISABLE_EXPORT=%q: disableExportEnvSet() = true, want false", value)
		}
	}
}

// The env var feeds the initialization identity: flipping it with an
// unchanged Config must produce a different signature, like Python storing
// the resolved disable_export in its registry values.
func TestDisableExportEnvChangesSignature(t *testing.T) {
	cfg := Config{APIKey: "nl_test"}
	t.Setenv("NEATLOGS_DISABLE_EXPORT", "")
	before := initializationSignature(cfg, initOptions{})
	t.Setenv("NEATLOGS_DISABLE_EXPORT", "true")
	after := initializationSignature(cfg, initOptions{})
	if before == after {
		t.Fatal("initializationSignature unchanged when NEATLOGS_DISABLE_EXPORT is set")
	}
}

// End to end through Init: with the env var set and a valid API key, export
// stays disabled and the runtime initializes and shuts down cleanly.
func TestInitHonorsDisableExportEnv(t *testing.T) {
	t.Setenv("NEATLOGS_DISABLE_EXPORT", "true")
	shutdown, err := Init(context.Background(), Config{APIKey: "nl_test"})
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned unexpected error: %v", err)
	}
}

// Behavioral guard: with NEATLOGS_DISABLE_EXPORT set, a traced span never
// reaches the ingest endpoint. Previously the env var was ignored and the
// span POSTed to /v1/traces anyway (ts and py both honor the var).
func TestDisableExportEnvDropsExport(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("NEATLOGS_DISABLE_EXPORT", "1")
	shutdown, err := Init(context.Background(), Config{
		APIKey:   "nl_test",
		Endpoint: server.URL,
	})
	if err != nil {
		t.Fatalf("Init returned unexpected error: %v", err)
	}
	_, _, end := Trace(context.Background(), "disabled-export-span")
	end()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown returned unexpected error: %v", err)
	}
	if requests != 0 {
		t.Fatalf("ingest received %d requests with NEATLOGS_DISABLE_EXPORT=1, want 0", requests)
	}
}
