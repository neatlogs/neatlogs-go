package doctor

import (
	"sort"
	"strings"
)

// isRootlessHTTPOnly returns true when all visible spans in a trace are
// rootless HTTP. This is the "auto-instrumented requests without a
// parent" pattern.
func isRootlessHTTPOnly(spans []Span) bool {
	if len(spans) == 0 {
		return false
	}
	for _, s := range spans {
		if SpanKind(s) != KindHTTP {
			return false
		}
		if ParentSpanID(s) != "" {
			return false
		}
	}
	return true
}

// findRootSpans returns the visible root spans (parent_span_id empty).
func findRootSpans(spans []Span) []Span {
	var roots []Span
	for _, s := range spans {
		if ParentSpanID(s) == "" {
			roots = append(roots, s)
		}
	}
	return roots
}

// rootKindsSet returns the set of span kinds among the visible roots.
func rootKindsSet(roots []Span) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range roots {
		out[SpanKind(s)] = struct{}{}
	}
	return out
}

// buildSpanChildMap constructs a span_id → []Span index from visible spans.
func buildSpanChildMap(spans []Span) map[string][]Span {
	out := map[string][]Span{}
	for _, s := range spans {
		pid := ParentSpanID(s)
		if pid == "" {
			continue
		}
		cid := SpanID(s)
		if cid == "" {
			continue
		}
		out[pid] = append(out[pid], s)
	}
	return out
}

// buildSpanIDSets returns (span_ids_set, duplicate_span_ids_list) over the
// visible spans. Self-parent spans are NOT filtered here — that's the
// self_parent check.
func buildSpanIDSets(spans []Span) (map[string]bool, []string) {
	seen := map[string]bool{}
	duplicates := []string{}
	for _, s := range spans {
		id := SpanID(s)
		if id == "" {
			continue
		}
		if seen[id] {
			duplicates = append(duplicates, id)
		} else {
			seen[id] = true
		}
	}
	return seen, duplicates
}

// missingIOFindings emits llm/tool/retriever-missing-io for spans of the
// listed IO kinds that lack input or output. Suppressed when the trace
// has no spans of those kinds (to avoid false positives on traces that
// never did those operations).
func missingIOFindings(spans []Span, traceID, runID string) []DoctorFinding {
	// Suppress: no spans of an IO kind → no missing-IO findings.
	hasIO := false
	for _, s := range spans {
		if _, ok := IOKinds[SpanKind(s)]; ok {
			hasIO = true
			break
		}
	}
	if !hasIO {
		return nil
	}

	missingByKind := map[string][]string{}
	for _, s := range spans {
		kind := SpanKind(s)
		if _, ok := IOKinds[kind]; !ok {
			continue
		}
		attrs := Attributes(s)
		if !hasInput(kind, attrs) || !hasOutput(kind, attrs) {
			missingByKind[kind] = append(missingByKind[kind], SpanName(s))
		}
	}

	var findings []DoctorFinding
	// Sort by kind name for stable output (matches Python's sorted()).
	keys := make([]string, 0, len(missingByKind))
	for k := range missingByKind {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, kind := range keys {
		names := missingByKind[kind]
		shown := strings.Join(firstN(names, 3), ", ")
		suffix := ""
		if len(names) > 3 {
			suffix = " and " + intToString(int64(len(names)-3)) + " more"
		}
		findings = append(findings, DoctorFinding{
			Severity:   "warning",
			Code:       kind + "-missing-io",
			Title:      strings.ToUpper(kind) + " spans are missing input or output",
			Evidence:   Truncate(intToString(int64(len(names)))+" span(s): "+shown+suffix, 400),
			Suggestion: "Check that the SDK call completed, capture_input/capture_output is enabled, and the provider integration supports this operation.",
			TraceID:    traceID,
			RunID:      runID,
			FixClass:   "capture",
		})
	}
	return findings
}

// orphanParentFindings emits orphan-parent for each visible span whose
// parent_span_id does not resolve to any visible span.
func orphanParentFindings(spans []Span, spanIDs map[string]bool, traceID, runID string) []DoctorFinding {
	var findings []DoctorFinding
	for _, s := range spans {
		pid := ParentSpanID(s)
		if pid == "" {
			continue
		}
		if !spanIDs[pid] {
			findings = append(findings, DoctorFinding{
				Severity:   "warning",
				Code:       "orphan-parent",
				Title:      "Span has a parent_span_id that does not exist in this trace",
				Evidence:   Truncate("span '"+SpanName(s)+"' has parent_span_id='"+pid+"' but no span with that id was found", 400),
				Suggestion: "This usually means a wrapper ended a span twice, or two wrappers produced overlapping traces. Inspect the call site for the named span.",
				TraceID:    traceID,
				RunID:      runID,
			})
		}
	}
	return findings
}

// selfParentFindings emits self-parent for the FIRST visible span whose
// span_id == parent_span_id (one finding per trace).
func selfParentFindings(spans []Span, traceID, runID string) []DoctorFinding {
	var findings []DoctorFinding
	for _, s := range spans {
		sid := SpanID(s)
		pid := ParentSpanID(s)
		if sid != "" && pid != "" && sid == pid {
			findings = append(findings, DoctorFinding{
				Severity:   "error",
				Code:       "self-parent",
				Title:      "Span has parent_span_id equal to its own span_id",
				Evidence:   Truncate("span '"+SpanName(s)+"' self-cycles", 400),
				Suggestion: "This is a serious instrumentation bug — the wrapper is using the wrong field or initializing twice. Open an issue on the neatlogs repo with this trace_id.",
				TraceID:    traceID,
				RunID:      runID,
			})
			return findings
		}
	}
	return findings
}

// duplicateSpanIDFindings emits duplicate-span-id (one finding per trace)
// when any span_id appears more than once.
func duplicateSpanIDFindings(duplicates []string, _ []Span, traceID, runID string) []DoctorFinding {
	if len(duplicates) == 0 {
		return nil
	}
	uniq := map[string]struct{}{}
	for _, d := range duplicates {
		uniq[d] = struct{}{}
	}
	ids := make([]string, 0, len(uniq))
	for k := range uniq {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	shown := strings.Join(firstN(ids, 5), ", ")
	return []DoctorFinding{{
		Severity:   "error",
		Code:       "duplicate-span-id",
		Title:      "Two or more spans share the same span_id",
		Evidence:   Truncate("span_id(s) appearing more than once: "+shown, 400),
		Suggestion: "Indicates a duplicate export or a wrapper that emits a new span without a unique id. The hierarchy check is unreliable for this trace.",
		TraceID:    traceID,
		RunID:      runID,
	}}
}

// multipleRootsFindings emits multiple-roots when a trace has more than
// one root span.
func multipleRootsFindings(roots []Span, traceID, runID string) []DoctorFinding {
	if len(roots) <= 1 {
		return nil
	}
	names := make([]string, 0, len(roots))
	for _, r := range roots {
		names = append(names, SpanName(r))
	}
	return []DoctorFinding{{
		Severity:   "warning",
		Code:       "multiple-roots",
		Title:      "Trace has more than one root span",
		Evidence:   Truncate(intToString(int64(len(roots)))+" root spans: "+strings.Join(firstN(names, 3), ", "), 400),
		Suggestion: "Either two entry points ran in parallel, or the trace_id is being shared across processes. Add a single @span(kind='WORKFLOW') at the top level, or generate a unique trace_id per execution.",
		TraceID:    traceID,
		RunID:      runID,
	}}
}

// agentWithoutLLMFindings emits agent-without-llm for each agent span
// whose subtree has no LLM descendant. Subtree-walked (per-agent, not
// per-trace) — bug #2 fix from the Python reference.
func agentWithoutLLMFindings(spans []Span, childMap map[string][]Span, traceID, runID string) []DoctorFinding {
	var findings []DoctorFinding
	for _, s := range spans {
		if SpanKind(s) != KindAgent {
			continue
		}
		sid := SpanID(s)
		if sid == "" {
			continue
		}
		if !hasLLMDescendant(sid, childMap, nil) {
			findings = append(findings, DoctorFinding{
				Severity:   "warning",
				Code:       "agent-without-llm",
				Title:      "Agent span ended without any LLM call in its subtree",
				Evidence:   Truncate("agent '"+SpanName(s)+"' has no LLM descendant", 400),
				Suggestion: "Check import order: the LLM client must be created AFTER neatlogs.init(). Also verify the LLM library is in the `instrumentations=[...]` list.",
				TraceID:    traceID,
				RunID:      runID,
			})
		}
	}
	return findings
}

// foreignInstrumentationFindings returns (findings, scopes_seen). When
// scopes_seen is false, no scope info was present in this run, and the
// caller emits a single report-level scope-not-preserved finding.
func foreignInstrumentationFindings(spans []Span, runID string) ([]DoctorFinding, bool) {
	scopeCounts := map[string]int{}
	scopesSeen := false
	for _, s := range spans {
		name := InstrumentationScopeName(s)
		if name == "" {
			continue
		}
		scopesSeen = true
		scopeCounts[name]++
	}
	if !scopesSeen {
		return nil, false
	}
	foreign := map[string]int{}
	for name, n := range scopeCounts {
		if !IsNeatlogsScope(name) {
			foreign[name] = n
		}
	}
	if len(foreign) == 0 {
		return nil, true
	}
	parts := make([]string, 0, len(foreign))
	for name, n := range foreign {
		parts = append(parts, intToString(int64(n))+" spans from '"+name+"'")
	}
	neatlogsN := scopeCounts[NeatlogsScopePrefix]
	evidence := Truncate(intToString(int64(len(spans)))+" total spans: "+strings.Join(parts, ", ")+" (+ "+intToString(int64(neatlogsN))+" neatlogs spans)", 400)
	return []DoctorFinding{{
		Severity:   "warning",
		Code:       "foreign-instrumentation-detected",
		Title:      "Foreign instrumentation is polluting the neatlogs trace",
		Evidence:   evidence,
		Suggestion: "Either disable the foreign instrumentations in this process, or set NEATLOGS_FILTER_SCOPE=neatlogs to scope the dashboard filter.",
		RunID:      runID,
	}}, true
}

// firstN returns the first n elements of a string slice, or all of it if
// len < n.
func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
