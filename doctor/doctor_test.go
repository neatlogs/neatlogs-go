package doctor

import (
	"strings"
	"testing"
)

// --- Test helpers -----------------------------------------------------------

// makeSpan builds a Span map with the given key/value pairs. Top-level
// fields (trace_id, span_id, etc.) and attributes are accepted; everything
// else is dropped. Keeps tests concise.
func makeSpan(traceID, spanID, parentID, name, kind string, attrs map[string]any) Span {
	s := Span{
		"trace_id": traceID,
		"span_id":  spanID,
		"name":     name,
		"kind":     kind,
	}
	if parentID != "" {
		s["parent_span_id"] = parentID
	} else {
		s["parent_span_id"] = nil
	}
	if attrs != nil {
		s["attributes"] = attrs
	}
	return s
}

func runDiagnose(t *testing.T, spans ...Span) DoctorReport {
	t.Helper()
	path := writeTemp(t, spans...)
	return Diagnose(path, Options{})
}

func writeTemp(t *testing.T, spans ...Span) string {
	t.Helper()
	var b strings.Builder
	for _, s := range spans {
		appendJSON(&b, s)
		b.WriteByte('\n')
	}
	f := tempFile(t, []byte(b.String()))
	return f
}

func findFinding(t *testing.T, report DoctorReport, code string) DoctorFinding {
	t.Helper()
	for _, f := range report.Findings {
		if f.Code == code {
			return f
		}
	}
	t.Fatalf("expected finding with code %q; got: %s", code, formatCodes(report))
	return DoctorFinding{}
}

func expectCode(t *testing.T, report DoctorReport, code string) {
	t.Helper()
	for _, f := range report.Findings {
		if f.Code == code {
			return
		}
	}
	t.Errorf("expected finding with code %q; got: %s", code, formatCodes(report))
}

func expectNoCode(t *testing.T, report DoctorReport, code string) {
	t.Helper()
	for _, f := range report.Findings {
		if f.Code == code {
			t.Errorf("did not expect finding %q; got: %s", code, formatCodes(report))
		}
	}
}

func formatCodes(r DoctorReport) string {
	var parts []string
	for _, f := range r.Findings {
		parts = append(parts, f.Code)
	}
	return strings.Join(parts, ",")
}

// --- Reading tests ----------------------------------------------------------

func TestReadSpans_Happy(t *testing.T) {
	path := writeTemp(t,
		makeSpan("t1", "s1", "", "root", "workflow", nil),
		makeSpan("t1", "s2", "s1", "child", "tool", nil),
	)
	var findings []DoctorFinding
	spans, invalid := ReadSpans(path, &findings)
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}
	if len(invalid) != 0 {
		t.Fatalf("expected 0 invalid lines, got %v", invalid)
	}
}

func TestReadSpans_MissingFile(t *testing.T) {
	var findings []DoctorFinding
	spans, _ := ReadSpans("/nonexistent/path/that/should/not/exist", &findings)
	if len(spans) != 0 {
		t.Errorf("expected 0 spans, got %d", len(spans))
	}
	expectCode(t, DoctorReport{Findings: findings}, "file-not-found")
}

func TestReadSpans_InvalidLines(t *testing.T) {
	f := tempFile(t, []byte("not json\n"+string(mustJSON(t, makeSpan("t1", "s1", "", "root", "workflow", nil)))+"\n"))
	var findings []DoctorFinding
	spans, invalid := ReadSpans(f, &findings)
	if len(spans) != 1 {
		t.Errorf("expected 1 valid span, got %d", len(spans))
	}
	if len(invalid) != 1 || invalid[0] != 1 {
		t.Errorf("expected invalid[0]=1, got %v", invalid)
	}
}

// --- Visibility / internal filter tests ------------------------------------

func TestIsInternal(t *testing.T) {
	// explicit neatlogs.internal=true
	if !IsInternal(makeSpan("t", "s", "", "x", "tool", map[string]any{"neatlogs.internal": true})) {
		t.Error("expected span with neatlogs.internal=true to be internal")
	}
	// name == neatlogs.trace.complete
	if !IsInternal(makeSpan("t", "s", "", "neatlogs.trace.complete", "tool", nil)) {
		t.Error("expected span named neatlogs.trace.complete to be internal")
	}
	// normal span
	if IsInternal(makeSpan("t", "s", "", "x", "tool", nil)) {
		t.Error("expected normal span to not be internal")
	}
}

// --- SpanKind tests --------------------------------------------------------

func TestSpanKind(t *testing.T) {
	// kind field
	if got := SpanKind(makeSpan("t", "s", "", "x", "LLM", nil)); got != "llm" {
		t.Errorf("kind=%q, want %q", got, "llm")
	}
	// falls back to attribute
	s := Span{
		"trace_id": "t", "span_id": "s", "name": "x",
		"attributes": map[string]any{"neatlogs.span.kind": "Tool"},
	}
	if got := SpanKind(s); got != "tool" {
		t.Errorf("kind=%q, want %q", got, "tool")
	}
	// empty
	if got := SpanKind(Span{}); got != "" {
		t.Errorf("kind=%q, want %q", got, "")
	}
}

// --- Rootless-HTTP-only test -----------------------------------------------

func TestRootlessHTTPOnly_Fires(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "s1", "", "http1", "http", nil),
		makeSpan("t1", "s2", "", "http2", "http", nil),
	)
	expectCode(t, report, "rootless-http-only")
}

func TestRootlessHTTPOnly_DoesNotFireWhenRooted(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "root", "", "wf", "workflow", nil),
		makeSpan("t1", "s1", "root", "http", "http", nil),
	)
	expectNoCode(t, report, "rootless-http-only")
}

// --- Missing root kind -----------------------------------------------------

func TestMissingRootKind(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "s1", "", "x", "tool", nil),
	)
	expectCode(t, report, "missing-root-kind")
}

func TestRootKindPresent_NoMissing(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "s1", "", "wf", "workflow", nil),
	)
	expectNoCode(t, report, "missing-root-kind")
}

// --- Hierarchy pathologies -------------------------------------------------

func TestOrphanParent(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "s1", "", "wf", "workflow", nil),
		makeSpan("t1", "s2", "ghost", "tool", "tool", nil), // parent doesn't exist
	)
	expectCode(t, report, "orphan-parent")
}

func TestSelfParent(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "s1", "s1", "wf", "workflow", nil), // self-cycles
	)
	expectCode(t, report, "self-parent")
}

func TestDuplicateSpanID(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "s1", "", "first", "workflow", nil),
		makeSpan("t1", "s1", "root", "second", "tool", nil), // same span_id
		makeSpan("t1", "root", "", "root", "workflow", nil),
	)
	expectCode(t, report, "duplicate-span-id")
}

func TestMultipleRoots(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r1", "", "first", "workflow", nil),
		makeSpan("t1", "r2", "", "second", "workflow", nil),
	)
	expectCode(t, report, "multiple-roots")
}

func TestNoMultipleRoots(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r1", "", "wf", "workflow", nil),
		makeSpan("t1", "r2", "r1", "child", "tool", nil),
	)
	expectNoCode(t, report, "multiple-roots")
}

// --- Cycle detection -------------------------------------------------------

func TestCycle_Simple(t *testing.T) {
	// s1 -> s2 -> s1
	report := runDiagnose(t,
		makeSpan("t1", "s1", "s2", "x", "tool", nil),
		makeSpan("t1", "s2", "s1", "y", "tool", nil),
	)
	expectCode(t, report, "cycle")
}

func TestCycle_DiamondNoFalsePositive(t *testing.T) {
	// root -> a, root -> b, a -> leaf, b -> leaf (diamond — no cycle)
	report := runDiagnose(t,
		makeSpan("t1", "root", "", "wf", "workflow", nil),
		makeSpan("t1", "a", "root", "a", "tool", nil),
		makeSpan("t1", "b", "root", "b", "tool", nil),
		makeSpan("t1", "leaf", "a", "leaf", "tool", nil),
	)
	expectNoCode(t, report, "cycle")
}

func TestCycle_SelfCycleBecomesSelfParent(t *testing.T) {
	// s1 self-cycles: should be self-parent, NOT cycle.
	report := runDiagnose(t,
		makeSpan("t1", "s1", "s1", "x", "tool", nil),
	)
	expectCode(t, report, "self-parent")
	expectNoCode(t, report, "cycle")
}

// --- Agent-without-llm -----------------------------------------------------

func TestAgentWithoutLLM(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "a", "", "agent", "agent", nil),
	)
	expectCode(t, report, "agent-without-llm")
}

func TestAgentWithLLM_NoFinding(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "a", "", "agent", "agent", nil),
		makeSpan("t1", "l", "a", "llm", "llm", nil),
	)
	expectNoCode(t, report, "agent-without-llm")
}

// --- Missing I/O -----------------------------------------------------------

func TestLLMMissingIO(t *testing.T) {
	// LLM span with no input/output attributes
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		makeSpan("t1", "l", "r", "llm", "llm", nil),
	)
	expectCode(t, report, "llm-missing-io")
}

func TestLLMWithIO_NoFinding(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		makeSpan("t1", "l", "r", "llm", "llm", map[string]any{
			"neatlogs.llm.input_messages.0.content":  "hi",
			"neatlogs.llm.output_messages.0.content": "hello",
		}),
	)
	expectNoCode(t, report, "llm-missing-io")
}

func TestLLMSystemPromptCountsAsInput(t *testing.T) {
	// Per Bug #1: system_prompt alone counts as input (no content needed).
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		makeSpan("t1", "l", "r", "llm", "llm", map[string]any{
			"neatlogs.llm.system_prompt":            "be concise",
			"neatlogs.llm.output_messages.0.content": "ok",
		}),
	)
	expectNoCode(t, report, "llm-missing-io")
}

func TestLLMRoleAloneDoesNotCountAsInput(t *testing.T) {
	// Per Bug #1: role-only is metadata, not input. Should still fire.
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		makeSpan("t1", "l", "r", "llm", "llm", map[string]any{
			"neatlogs.llm.input_messages.0.role":    "user",
			"neatlogs.llm.output_messages.0.content": "ok",
		}),
	)
	expectCode(t, report, "llm-missing-io")
}

func TestToolMissingIO(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		makeSpan("t1", "t", "r", "tool", "tool", nil),
	)
	expectCode(t, report, "tool-missing-io")
}

func TestRetrieverMissingIO(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		makeSpan("t1", "s", "r", "search", "retriever", nil),
	)
	expectCode(t, report, "retriever-missing-io")
}

func TestMissingIOSuppressedWhenNoIOKindsInTrace(t *testing.T) {
	// A trace with no llm/tool/retriever should NOT fire any missing-io.
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		makeSpan("t1", "a", "r", "a", "agent", nil),
	)
	expectNoCode(t, report, "llm-missing-io")
	expectNoCode(t, report, "tool-missing-io")
	expectNoCode(t, report, "retriever-missing-io")
}

// --- Foreign instrumentation ----------------------------------------------

func TestForeignInstrumentation(t *testing.T) {
	spans := []Span{
		makeSpan("t1", "r", "", "wf", "workflow", nil),
	}
	spans[0]["instrumentation_scope"] = map[string]any{"name": "openlit", "version": "1.0"}
	spans = append(spans, makeSpan("t1", "l", "r", "llm", "llm", map[string]any{
		"neatlogs.llm.input_messages.0.content":  "hi",
		"neatlogs.llm.output_messages.0.content": "ok",
	}))
	report := runDiagnose(t, spans...)
	expectCode(t, report, "foreign-instrumentation-detected")
}

func TestNeatlogsScope_NoForeign(t *testing.T) {
	spans := []Span{
		makeSpan("t1", "r", "", "wf", "workflow", nil),
	}
	spans[0]["instrumentation_scope"] = map[string]any{"name": "neatlogs.core", "version": "1.0"}
	report := runDiagnose(t, spans...)
	expectNoCode(t, report, "foreign-instrumentation-detected")
}

func TestScopeNotPreserved(t *testing.T) {
	// No scope on any span → scope-not-preserved (single report-level info).
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
	)
	expectCode(t, report, "scope-not-preserved")
}

// --- File-level findings ---------------------------------------------------

func TestFileNotFound(t *testing.T) {
	report := Diagnose("/no/such/file", Options{})
	expectCode(t, report, "file-not-found")
}

func TestNoSpans_EmptyFile(t *testing.T) {
	path := tempFile(t, []byte(""))
	var findings []DoctorFinding
	spans, _ := ReadSpans(path, &findings)
	report := DoctorReport{
		Path: path, SpansRead: len(spans), Findings: findings,
	}
	if report.SpansRead != 0 {
		t.Errorf("expected 0 spans, got %d", report.SpansRead)
	}
}

func TestInvalidJSONL(t *testing.T) {
	path := tempFile(t, []byte("not json\n"))
	report := Diagnose(path, Options{})
	expectCode(t, report, "invalid-jsonl")
}

func TestMultiRunLog(t *testing.T) {
	a := makeSpan("t1", "a", "", "wf", "workflow", map[string]any{"session.id": "sess-1"})
	b := makeSpan("t1", "b", "", "wf", "workflow", map[string]any{"session.id": "sess-2"})
	report := runDiagnose(t, a, b)
	expectCode(t, report, "multi-run-log")
}

func TestRunIDNotFound(t *testing.T) {
	a := makeSpan("t1", "a", "", "wf", "workflow", map[string]any{"session.id": "sess-1"})
	report := runDiagnoseForPath(t, writeTemp(t, a), Options{RunID: "sess-X"})
	expectCode(t, report, "run-id-not-found")
}

func runDiagnoseForPath(t *testing.T, path string, opts Options) DoctorReport {
	t.Helper()
	return Diagnose(path, opts)
}

// --- Foreign-only filter --------------------------------------------------

func TestForeignOnlyFilter(t *testing.T) {
	spans := []Span{makeSpan("t1", "r", "", "wf", "workflow", nil)}
	spans[0]["instrumentation_scope"] = map[string]any{"name": "openlit", "version": "1.0"}
	path := writeTemp(t, spans...)
	report := Diagnose(path, Options{ForeignOnly: true})
	for _, f := range report.Findings {
		if !strings.HasPrefix(f.Code, "foreign-instrumentation") {
			t.Errorf("foreign-only should drop %q", f.Code)
		}
	}
	// pipeline-stage-summary should be suppressed under foreign-only.
	for _, f := range report.Findings {
		if f.Code == "pipeline-stage-summary" {
			t.Errorf("pipeline-stage-summary should be suppressed under foreign-only")
		}
	}
}

// --- New dimensions: init-order, attribute-completeness, data-integrity ---

func TestInitAfterClient(t *testing.T) {
	// Span exists but has no init marker keys.
	report := runDiagnose(t,
		Span{
			"trace_id":               "t1",
			"span_id":                "s1",
			"parent_span_id":         nil,
			"name":                   "x",
			"kind":                   "tool",
			"attributes":             map[string]any{"some.attr": "v"},
			"instrumentation_scope":  map[string]any{"name": "neatlogs-go"},
		},
	)
	f := findFinding(t, report, "init-after-client")
	if f.Severity != "error" {
		t.Errorf("init-after-client severity = %q, want error", f.Severity)
	}
	if !f.AutomatedFixAvailable {
		t.Error("init-after-client should be automated_fix_available=true")
	}
	if f.FixClass != "init_order" {
		t.Errorf("init-after-client fix_class = %q, want init_order", f.FixClass)
	}
	if strings.Contains(f.Suggestion, "force_reload") {
		t.Errorf("init-after-client suggestion must not mention force_reload: %s", f.Suggestion)
	}
	if strings.Contains(f.DocURL, "docs.neatlogs.com") {
		t.Errorf("init-after-client doc_url must not point to docs.neatlogs.com: %s", f.DocURL)
	}
}

func TestInitAfterClient_SuppressedWhenMarkersPresent(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "s1", "", "x", "tool", map[string]any{
			"neatlogs.span.kind": "tool",
		}),
	)
	expectNoCode(t, report, "init-after-client")
}

func TestMissingSpanKind_Partial(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", map[string]any{
			"neatlogs.span.kind": "workflow",
		}),
		makeSpan("t1", "c", "r", "tool", "tool", nil), // no neatlogs.span.kind
	)
	expectCode(t, report, "missing-span-kind")
}

func TestMissingSpanKind_AllMissingSuppressed(t *testing.T) {
	// All spans miss the kind → init-order handles it, not missing-span-kind.
	report := runDiagnose(t,
		Span{
			"trace_id": "t1", "span_id": "s1", "name": "x", "kind": "tool",
		},
	)
	// First, find the init-after-client finding and check it.
	_ = findFinding(t, report, "init-after-client")
	expectNoCode(t, report, "missing-span-kind")
}

func TestDataIntegrity_ZeroDuration(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		zeroDurSpan(),
	)
	expectCode(t, report, "zero-duration-span")
}

func TestDataIntegrity_LatencyMismatch(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		latencyMismatchSpan(),
	)
	expectCode(t, report, "latency-mismatch")
}

func TestDataIntegrity_ErrorNoEvent(t *testing.T) {
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		errorNoEventSpan(),
	)
	expectCode(t, report, "error-status-no-event")
}

func TestDataIntegrity_InternalExcluded(t *testing.T) {
	s := zeroDurSpan()
	s["attributes"] = map[string]any{"neatlogs.internal": true}
	s["name"] = "neatlogs.trace.complete"
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
		s,
	)
	expectNoCode(t, report, "zero-duration-span")
}

// --- Pipeline-stage summary ----------------------------------------------

func TestPipelineStageSummary_Init(t *testing.T) {
	// 6 traces with init-order (init stage) and 0 instrument-stage
	// findings. Init should clearly dominate.
	report := runDiagnose(t,
		Span{"trace_id": "t1", "span_id": "s1", "name": "x", "kind": "chain"},
		Span{"trace_id": "t2", "span_id": "s2", "name": "y", "kind": "chain"},
		Span{"trace_id": "t3", "span_id": "s3", "name": "z", "kind": "chain"},
		Span{"trace_id": "t4", "span_id": "s4", "name": "w", "kind": "chain"},
		Span{"trace_id": "t5", "span_id": "s5", "name": "v", "kind": "chain"},
		Span{"trace_id": "t6", "span_id": "s6", "name": "u", "kind": "chain"},
	)
	found := false
	for _, f := range report.Findings {
		if f.Code == "pipeline-stage-summary" {
			found = true
			if !strings.Contains(f.Title, "init") {
				t.Errorf("summary title should mention init stage: %s", f.Title)
			}
		}
	}
	if !found {
		t.Errorf("expected pipeline-stage-summary; got: %s", formatCodes(report))
	}
}

// --- Sorting stability ----------------------------------------------------

func TestFindingsSortStable(t *testing.T) {
	// Create a multi-finding report and verify the sort order.
	report := runDiagnose(t,
		Span{
			"trace_id": "t1", "span_id": "s1", "name": "x", "kind": "tool",
		},
	)
	// Should have at least init-after-client + pipeline-stage-summary.
	// Verify the relative order: error < warning < info.
	var lastRank = -1
	for _, f := range report.Findings {
		r := severityRank[f.Severity]
		if r < lastRank {
			t.Errorf("findings out of order: %s after a higher rank (got rank %d after %d)", f.Code, r, lastRank)
		}
		lastRank = r
	}
}

// --- SpanStatusIsError tolerance tests -----------------------------------

func TestSpanStatusIsError_NeatlogsFormat(t *testing.T) {
	if !SpanStatusIsError(map[string]any{"code": "ERROR"}) {
		t.Error("expected ERROR code to be detected")
	}
}

func TestSpanStatusIsError_OTelSDKFormat(t *testing.T) {
	if !SpanStatusIsError(map[string]any{
		"status_code": map[string]any{"name": "ERROR", "value": 2},
	}) {
		t.Error("expected OTel SDK format ERROR to be detected")
	}
}

func TestSpanStatusIsError_OK(t *testing.T) {
	if SpanStatusIsError(map[string]any{"code": "OK"}) {
		t.Error("OK should not be an error")
	}
	if SpanStatusIsError(nil) {
		t.Error("nil should not be an error")
	}
}

// --- IsNeatlogsScope ------------------------------------------------------

func TestIsNeatlogsScope(t *testing.T) {
	if !IsNeatlogsScope("neatlogs") {
		t.Error(`"neatlogs" should be own scope`)
	}
	if !IsNeatlogsScope("neatlogs.core") {
		t.Error(`"neatlogs.core" should be own scope`)
	}
	if IsNeatlogsScope("openlit") {
		t.Error(`"openlit" should be foreign`)
	}
}

// --- To_dict output shape -------------------------------------------------

func TestDoctorFinding_TraceIDOnPerTraceFindings(t *testing.T) {
	// Per-trace findings carry trace_id; report-level findings (no specific
	// trace) do not.
	report := runDiagnose(t,
		makeSpan("t1", "r", "", "wf", "workflow", nil),
	)
	perTrace := map[string]bool{
		"rootless-http-only": true, "missing-root-kind": true,
		"orphan-parent": true, "self-parent": true,
		"duplicate-span-id": true, "multiple-roots": true,
		"cycle": true, "agent-without-llm": true,
		"llm-missing-io": true, "tool-missing-io": true,
		"retriever-missing-io": true,
		"init-after-client": true, "missing-span-kind": true,
		"zero-duration-span": true, "error-status-no-event": true,
		"latency-mismatch": true, "foreign-instrumentation-detected": true,
	}
	for _, f := range report.Findings {
		if perTrace[f.Code] && f.TraceID == "" {
			t.Errorf("per-trace finding %q should have trace_id", f.Code)
		}
	}
}

// --- Performance smoke test (1K, 5K, 10K) --------------------------------

func TestPerformance_LinearScaling(t *testing.T) {
	for _, n := range []int{1000, 5000, 10000} {
		spans := makeLargeTrace(n)
		path := writeTemp(t, spans...)
		// Just make sure it completes; we don't assert specific timing here.
		_ = Diagnose(path, Options{})
	}
	// Larger sizes (50K, 100K) are exercised in benchmark tests.
}

// makeLargeTrace builds a synthetic "workflow with N tool children" trace
// — a realistic shape for perf testing.
func makeLargeTrace(n int) []Span {
	spans := make([]Span, 0, n+1)
	spans = append(spans, makeSpan("t", "root", "", "wf", "workflow", map[string]any{
		"neatlogs.span.kind": "workflow",
	}))
	for i := 0; i < n; i++ {
		spans = append(spans, makeSpan("t", "child"+itoa(i), "root", "t", "tool", map[string]any{
			"neatlogs.span.kind":     "tool",
			"neatlogs.tool.input":    `{"x":1}`,
			"neatlogs.tool.output":   `{"y":2}`,
		}))
	}
	return spans
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// PR #21: OTel GenAI semconv validation
// ---------------------------------------------------------------------------

func TestOtelGenaiMissing_Fires(t *testing.T) {
	// LLM span without gen_ai.operation.name → otel-genai-missing fires.
	spans := []Span{
		makeSpan("t1", "r", "", "root", "workflow", map[string]any{
			"neatlogs.span.kind": "workflow",
		}),
		makeSpan("t1", "l", "r", "chat", "llm", map[string]any{
			"neatlogs.span.kind": "llm",
		}),
	}
	report := runDiagnose(t, spans...)
	f := findFinding(t, report, "otel-genai-missing")
	if f.Severity != "warning" {
		t.Errorf("otel-genai-missing: expected severity warning, got %q", f.Severity)
	}
	if f.FixClass != "config" {
		t.Errorf("otel-genai-missing: expected fix_class config, got %q", f.FixClass)
	}
}

func TestOtelGenaiMissing_DoesNotFireWhenPresent(t *testing.T) {
	spans := []Span{
		makeSpan("t1", "l", "", "chat", "llm", map[string]any{
			"neatlogs.span.kind":     "llm",
			"gen_ai.operation.name": "chat",
		}),
	}
	report := runDiagnose(t, spans...)
	expectNoCode(t, report, "otel-genai-missing")
}

func TestOtelGenaiMissing_DoesNotFireOnNonLLM(t *testing.T) {
	// Tool span without gen_ai attr — not LLM-kind, so no finding.
	spans := []Span{
		makeSpan("t1", "t", "", "tool", "tool", map[string]any{
			"neatlogs.span.kind": "tool",
		}),
	}
	report := runDiagnose(t, spans...)
	expectNoCode(t, report, "otel-genai-missing")
}

func TestOtelGenaiInconsistent_Fires(t *testing.T) {
	// neatlogs=llm but OTel op-name=embeddings → inconsistent.
	spans := []Span{
		makeSpan("t1", "l", "", "mismatch", "llm", map[string]any{
			"neatlogs.span.kind":     "llm",
			"gen_ai.operation.name": "embeddings",
		}),
	}
	report := runDiagnose(t, spans...)
	f := findFinding(t, report, "otel-genai-inconsistent")
	if f.Severity != "info" {
		t.Errorf("otel-genai-inconsistent: expected severity info, got %q", f.Severity)
	}
	if !strings.Contains(f.Evidence, "neatlogs.span.kind='llm'") {
		t.Errorf("otel-genai-inconsistent: evidence should mention kind='llm', got: %s", f.Evidence)
	}
	if !strings.Contains(f.Evidence, "gen_ai.operation.name='embeddings'") {
		t.Errorf("otel-genai-inconsistent: evidence should mention op-name, got: %s", f.Evidence)
	}
}

// §12.8.2 (PR #21 review): the walker must check isLLMKind() BEFORE
// looking at op-name. A tool span with chat op-name is still a tool span.
func TestOtelGenaiInconsistent_DoesNotFireOnToolSpan(t *testing.T) {
	spans := []Span{
		makeSpan("t1", "t", "", "my-tool", "tool", map[string]any{
			"neatlogs.span.kind":     "tool",
			"gen_ai.operation.name": "chat",
		}),
	}
	report := runDiagnose(t, spans...)
	expectNoCode(t, report, "otel-genai-inconsistent")
}

// ---------------------------------------------------------------------------
// PR #21: token-waste findings
// ---------------------------------------------------------------------------

func TestOversizedPrompt_Fires(t *testing.T) {
	big := strings.Repeat("x", OversizedPromptCharThreshold+1)
	spans := []Span{
		makeSpan("t1", "l", "", "huge", "llm", map[string]any{
			"neatlogs.span.kind":   "llm",
			"neatlogs.llm.system": big,
		}),
	}
	report := runDiagnose(t, spans...)
	f := findFinding(t, report, "oversized-prompt")
	if f.Severity != "warning" {
		t.Errorf("oversized-prompt: expected severity warning, got %q", f.Severity)
	}
}

func TestOversizedPrompt_DoesNotFireOnSmall(t *testing.T) {
	spans := []Span{
		makeSpan("t1", "l", "", "small", "llm", map[string]any{
			"neatlogs.span.kind":   "llm",
			"neatlogs.llm.system": "You are a helpful assistant.",
		}),
	}
	report := runDiagnose(t, spans...)
	expectNoCode(t, report, "oversized-prompt")
}

func TestRepeatedSystemPrompt_PIIGated_DefaultOff(t *testing.T) {
	sys := "You are a helpful assistant."
	spans := make([]Span, 0, 15)
	for i := 0; i < 15; i++ {
		spans = append(spans, makeSpan("t1", "s"+intToString(int64(i)), "", "call", "llm", map[string]any{
			"neatlogs.span.kind":   "llm",
			"neatlogs.llm.system": sys,
		}))
	}
	// Default Options: ReadPromptContent=false → no finding.
	report := runDiagnose(t, spans...)
	expectNoCode(t, report, "repeated-system-prompt")
}

func TestRepeatedSystemPrompt_FiresWithReadPromptContent(t *testing.T) {
	sys := "You are a helpful assistant."
	spans := make([]Span, 0, 12)
	for i := 0; i < 12; i++ {
		spans = append(spans, makeSpan("t1", "s"+intToString(int64(i)), "", "call", "llm", map[string]any{
			"neatlogs.span.kind":   "llm",
			"neatlogs.llm.system": sys,
		}))
	}
	path := writeTemp(t, spans...)
	report := Diagnose(path, Options{ReadPromptContent: true})
	f := findFinding(t, report, "repeated-system-prompt")
	if f.Severity != "info" {
		t.Errorf("repeated-system-prompt: expected info, got %q", f.Severity)
	}
	if !strings.Contains(f.Evidence, "12 times") {
		t.Errorf("repeated-system-prompt: evidence should mention 12 times, got: %s", f.Evidence)
	}
}

func TestUnusedToolDefinition_Fires(t *testing.T) {
	spans := []Span{
		makeSpan("t1", "l", "", "with-tools", "llm", map[string]any{
			"neatlogs.span.kind":     "llm",
			"gen_ai.operation.name": "chat",
			"gen_ai.tool.definitions": []any{
				map[string]any{"name": "get_weather"},
				map[string]any{"name": "get_news"},
			},
			"gen_ai.output.messages": []any{
				map[string]any{"finish_reason": "stop", "tool_calls": []any{}},
			},
		}),
	}
	report := runDiagnose(t, spans...)
	f := findFinding(t, report, "unused-tool-definition")
	if !strings.Contains(f.Evidence, "get_weather") {
		t.Errorf("unused-tool-definition: evidence should mention get_weather, got: %s", f.Evidence)
	}
}

func TestUnusedToolDefinition_DoesNotFireWhenAllCalled(t *testing.T) {
	spans := []Span{
		makeSpan("t1", "l", "", "all-used", "llm", map[string]any{
			"neatlogs.span.kind":     "llm",
			"gen_ai.operation.name": "chat",
			"gen_ai.tool.definitions": []any{
				map[string]any{"name": "get_weather"},
			},
			"gen_ai.output.messages": []any{
				map[string]any{"tool_calls": []any{
					map[string]any{"function": map[string]any{"name": "get_weather"}},
				}},
			},
		}),
	}
	report := runDiagnose(t, spans...)
	expectNoCode(t, report, "unused-tool-definition")
}

// ---------------------------------------------------------------------------
// PR #21: RenderFixSnippet
// ---------------------------------------------------------------------------

func TestRenderFixSnippet_AllCodes(t *testing.T) {
	for _, code := range FixSnippetCodes() {
		snippet := RenderFixSnippet(code)
		if snippet == "" {
			t.Errorf("RenderFixSnippet(%q) returned empty string", code)
			continue
		}
		// §12.8.3: plain text with literal newlines, not JSON-escaped.
		if !strings.Contains(snippet, "\n") {
			t.Errorf("RenderFixSnippet(%q): missing newline", code)
		}
		if strings.Contains(snippet, `\n`) {
			t.Errorf("RenderFixSnippet(%q): contains JSON-escaped newline", code)
		}
		if !strings.Contains(snippet, "# Finding: "+code) {
			t.Errorf("RenderFixSnippet(%q): missing header", code)
		}
		if !strings.Contains(snippet, "# BEFORE:") {
			t.Errorf("RenderFixSnippet(%q): missing BEFORE section", code)
		}
		if !strings.Contains(snippet, "# AFTER:") {
			t.Errorf("RenderFixSnippet(%q): missing AFTER section", code)
		}
	}
}

func TestRenderFixSnippet_UnknownReturnsEmpty(t *testing.T) {
	if got := RenderFixSnippet("not-a-real-code"); got != "" {
		t.Errorf("RenderFixSnippet on unknown code should return empty string, got: %q", got)
	}
	if got := RenderFixSnippet("init-after-client-typo"); got != "" {
		t.Errorf("RenderFixSnippet on typo'd code should return empty string, got: %q", got)
	}
}
