package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neatlogs/neatlogs-go/doctor"
)

func TestRunMain_MissingPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain([]string{}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit code 2 for missing path, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("expected usage message on stderr, got: %s", stderr.String())
	}
}

func TestRunMain_FileNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"/no/such/file/path"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1 for file-not-found, got %d", code)
	}
	// stdout is the human report; stderr is for flag-parser errors only.
	if !strings.Contains(stdout.String(), "file not found") {
		t.Errorf("expected human report on stdout to contain file-not-found title, got: %s", stdout.String())
	}
}

func TestRunMain_HumanReport(t *testing.T) {
	path := writeSpanFile(t)
	var stdout, stderr bytes.Buffer
	code := runMain([]string{path}, &stdout, &stderr)
	if code != 1 { // init-after-client fires (error severity)
		t.Errorf("expected exit code 1, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Trace Doctor") {
		t.Errorf("expected human report header, got: %s", out)
	}
	// The human report uses the finding's title, not its code.
	if !strings.Contains(out, "init markers") {
		t.Errorf("expected init-after-client title in human report, got: %s", out)
	}
}

func TestRunMain_JSONReport(t *testing.T) {
	path := writeSpanFile(t)
	var stdout, stderr bytes.Buffer
	code := runMain([]string{path, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	out := stdout.String()
	// Should be a JSON object with a "findings" array.
	if !strings.Contains(out, "\"findings\"") {
		t.Errorf("expected JSON report with findings field, got: %s", out)
	}
	if !strings.Contains(out, "\"init-after-client\"") {
		t.Errorf("expected init-after-client code in JSON, got: %s", out)
	}
}

func TestRunMain_NoErrorsExitZero(t *testing.T) {
	// A clean trace: workflow root + 1 tool with full I/O → no errors.
	path := writeCleanTrace(t)
	var stdout, stderr bytes.Buffer
	code := runMain([]string{path, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0 for clean trace, got %d; stdout: %s", code, stdout.String())
	}
}

func TestRunMain_RunIDFilter(t *testing.T) {
	// Two runs, filter to one.
	path := writeMultiRunFile(t)
	var stdout, stderr bytes.Buffer
	code := runMain([]string{path, "--run-id", "sess-1", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit 0, got %d; stdout: %s", code, stdout.String())
	}
	out := stdout.String()
	// Should mention only sess-1 traces.
	if strings.Contains(out, "sess-2") {
		t.Errorf("--run-id sess-1 should exclude sess-2, but output: %s", out)
	}
}

func TestRunMain_ForeignOnly(t *testing.T) {
	// Add a span with foreign scope.
	path := writeForeignSpanFile(t)
	var stdout, _ bytes.Buffer
	code := runMain([]string{path, "--foreign-only", "--json"}, &stdout, &bytes.Buffer{})
	if code != 0 { // foreign-only: no error severity
		t.Errorf("expected exit 0, got %d; stdout: %s", code, stdout.String())
	}
	out := stdout.String()
	// Should only contain foreign-instrumentation findings.
	if !strings.Contains(out, "foreign-instrumentation-detected") {
		t.Errorf("expected foreign-instrumentation-detected in JSON output, got: %s", out)
	}
	if strings.Contains(out, "missing-root-kind") {
		t.Errorf("foreign-only should suppress missing-root-kind, got: %s", out)
	}
}

func TestRunMain_BadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"--unknown-flag"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit 2 for bad flag, got %d", code)
	}
}

// --- PR #21: --emit-fix and --read-prompt-content -------------------------

func TestRunMain_EmitFix_KnownCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"--emit-fix", "init-after-client"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit 0 for known --emit-fix code, got %d", code)
	}
	if !strings.Contains(stdout.String(), "# Finding: init-after-client") {
		t.Errorf("expected snippet on stdout, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "neatlogs.init()") {
		t.Errorf("snippet should mention neatlogs.init(), got: %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr for known code, got: %s", stderr.String())
	}
}

func TestRunMain_EmitFix_UnknownCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"--emit-fix", "unknown-code"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("expected exit 2 for unknown --emit-fix code, got %d", code)
	}
	if !strings.Contains(stderr.String(), "Unknown finding code") {
		t.Errorf("expected 'Unknown finding code' on stderr, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "init-after-client") {
		t.Errorf("stderr should list known codes (init-after-client), got: %s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("expected empty stdout for unknown code, got: %s", stdout.String())
	}
}

func TestRunMain_EmitFix_NoPathRequired(t *testing.T) {
	// --emit-fix bypasses the path requirement; should not print "usage:".
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"--emit-fix", "missing-span-kind"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "# Finding: missing-span-kind") {
		t.Errorf("expected snippet on stdout, got: %s", stdout.String())
	}
}

func TestRunMain_EmitFix_EqualsForm(t *testing.T) {
	// --emit-fix=CODE form should also work.
	var stdout, _ bytes.Buffer
	var stderr bytes.Buffer
	code := runMain([]string{"--emit-fix=zero-duration-span"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "zero-duration-span") {
		t.Errorf("expected snippet for zero-duration-span, got: %s", stdout.String())
	}
}

func TestRunMain_ReadPromptContentFlag(t *testing.T) {
	// Build a trace with 12 LLM spans sharing the same system prompt.
	// Without --read-prompt-content → no finding. With → finding.
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	var lines []string
	lines = append(lines, `{"trace_id":"t1","span_id":"r","parent_span_id":null,"name":"root","kind":"workflow","attributes":{"neatlogs.span.kind":"workflow"}}`)
	sys := "You are a helpful assistant."
	for i := 0; i < 12; i++ {
		lines = append(lines, fmt.Sprintf(`{"trace_id":"t1","span_id":"s%d","parent_span_id":"r","name":"call-%d","kind":"llm","attributes":{"neatlogs.span.kind":"llm","neatlogs.llm.system":%q}}`, i, i, sys))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Without flag → no repeated-system-prompt.
	var stdout1, _ bytes.Buffer
	code := runMain([]string{path, "--json"}, &stdout1, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if strings.Contains(stdout1.String(), "repeated-system-prompt") {
		t.Errorf("without --read-prompt-content, no repeated-system-prompt expected, got: %s", stdout1.String())
	}

	// With flag → repeated-system-prompt fires.
	var stdout2, _ bytes.Buffer
	code = runMain([]string{path, "--read-prompt-content", "--json"}, &stdout2, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout2.String(), "repeated-system-prompt") {
		t.Errorf("with --read-prompt-content, expected repeated-system-prompt in output, got: %s", stdout2.String())
	}
}

// --- helpers ---

func writeSpanFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	content := `{"trace_id":"t1","span_id":"s1","parent_span_id":null,"name":"x","kind":"tool","attributes":{"some.attr":"v"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write span file: %v", err)
	}
	return path
}

func writeCleanTrace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	content := `{"trace_id":"t1","span_id":"r","parent_span_id":null,"name":"wf","kind":"workflow","attributes":{"neatlogs.span.kind":"workflow"}}` + "\n" +
		`{"trace_id":"t1","span_id":"t","parent_span_id":"r","name":"tool","kind":"tool","attributes":{"neatlogs.span.kind":"tool","neatlogs.tool.input":"{}","neatlogs.tool.output":"{}"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write clean trace: %v", err)
	}
	return path
}

func writeMultiRunFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	lines := []string{
		`{"trace_id":"t1","span_id":"a","parent_span_id":null,"name":"wf1","kind":"workflow","attributes":{"neatlogs.span.kind":"workflow","session.id":"sess-1"}}`,
		`{"trace_id":"t2","span_id":"b","parent_span_id":null,"name":"wf2","kind":"workflow","attributes":{"neatlogs.span.kind":"workflow","session.id":"sess-2"}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write multi-run file: %v", err)
	}
	return path
}

func writeForeignSpanFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spans.jsonl")
	lines := []string{
		`{"trace_id":"t1","span_id":"r","parent_span_id":null,"name":"wf","kind":"workflow","attributes":{"neatlogs.span.kind":"workflow"},"instrumentation_scope":{"name":"openlit","version":"1.0"}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}
	return path
}

func init() {
	// Sanity: ensure the doctor package is in use (silences unused-import linter).
	_ = doctor.DefaultSessionID
}
