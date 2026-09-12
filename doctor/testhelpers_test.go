package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempFile writes data to a temp file and returns its path. The file is
// cleaned up automatically when the test ends.
func tempFile(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "spans-*.jsonl")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	return f.Name()
}

// appendJSON writes a span as a single JSON line to b (no trailing newline).
func appendJSON(b *strings.Builder, span Span) {
	raw, err := json.Marshal(span)
	if err != nil {
		// In tests, a marshal failure means a bug in the test fixture.
		panic("appendJSON: " + err.Error())
	}
	b.Write(raw)
}

// mustJSON returns the JSON encoding of v and fails the test on error.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustJSON: %v", err)
	}
	return b
}

// zeroDurSpan returns a span whose start == end (in nanoseconds).
func zeroDurSpan() Span {
	s := makeSpan("t", "z", "", "x", "tool", nil)
	s["start_time"] = int64(1000)
	s["end_time"] = int64(1000)
	return s
}

// latencyMismatchSpan returns a span where end_time < start_time.
func latencyMismatchSpan() Span {
	s := makeSpan("t", "m", "", "x", "tool", nil)
	s["start_time"] = int64(2000)
	s["end_time"] = int64(1000)
	return s
}

// errorNoEventSpan returns a span whose status is ERROR but has no
// exception event.
func errorNoEventSpan() Span {
	s := makeSpan("t", "e", "", "x", "tool", nil)
	s["status"] = map[string]any{"code": "ERROR", "description": "boom"}
	return s
}

// Resolve a stable temp dir under the workspace (keeps test paths short).
var testBase = func() string {
	d := filepath.Join(os.TempDir(), "neatlogs-doctor-tests")
	_ = os.MkdirAll(d, 0o755)
	return d
}()
