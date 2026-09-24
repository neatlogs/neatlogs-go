package doctor

import (
	"sort"
	"strings"
)

// stageSuggestions maps the dominant pipeline stage to a stage-specific
// suggestion for the pipeline-stage-summary finding.
var stageSuggestions = map[string]string{
	"init": "Move neatlogs.init() to the top of the entry point (before any client is constructed), then re-run the doctor — the rest usually resolves once init is right.",
	"instrument": "Most findings are about wrappers not capturing or being reached. Verify the LLM client is constructed after neatlogs.init() and that the wrapper registered for the framework is actually installed.",
	"span": "Most findings are about the captured span data itself. Check the wrapper's end() and exception-recording paths; a crashed wrapper leaves spans with zero duration and no events.",
	"hierarchy": "Most findings are about parent/child relationships. Verify that each wrapper sets parent_span_id correctly; duplicates and orphan parents usually mean a wrapper is creating spans outside the active context.",
}

// pipelineStageSummary counts findings per pipeline stage using
// FixClassToStage. Returns a map with the 4 stage keys.
func pipelineStageSummary(findings []DoctorFinding) map[string]int {
	out := map[string]int{
		"init": 0, "instrument": 0, "span": 0, "hierarchy": 0,
	}
	for _, f := range findings {
		stage, ok := FixClassToStage[f.FixClass]
		if !ok {
			continue
		}
		out[stage]++
	}
	return out
}

// pipelineStageRunFinding builds the run-level pipeline-stage summary
// finding, or returns nil when no findings cluster at a single stage.
// "Cluster" means a single stage has more findings than all others
// combined (stage_count * 2 > total).
func pipelineStageRunFinding(findings []DoctorFinding) *DoctorFinding {
	if len(findings) == 0 {
		return nil
	}
	counts := pipelineStageSummary(findings)
	total := counts["init"] + counts["instrument"] + counts["span"] + counts["hierarchy"]
	if total == 0 {
		return nil
	}
	// Find the dominant stage: max by count, ties broken by insertion order.
	dominant := ""
	dominantCount := -1
	for _, stage := range []string{"init", "instrument", "span", "hierarchy"} {
		if counts[stage] > dominantCount {
			dominant = stage
			dominantCount = counts[stage]
		}
	}
	if dominantCount*2 <= total {
		return nil
	}
	suggestion, ok := stageSuggestions[dominant]
	if !ok {
		suggestion = "Fix the " + dominant + " stage first; re-run the doctor."
	}
	// related_codes: every finding whose fix_class maps to the dominant
	// stage (uses the same map as the counter — not a hardcoded init set).
	var related []string
	for _, f := range findings {
		if FixClassToStage[f.FixClass] == dominant {
			related = append(related, f.Code)
		}
	}
	evidence := "stage breakdown: init=" + intToString(int64(counts["init"])) +
		", instrument=" + intToString(int64(counts["instrument"])) +
		", span=" + intToString(int64(counts["span"])) +
		", hierarchy=" + intToString(int64(counts["hierarchy"]))
	return &DoctorFinding{
		Severity:     "info",
		Code:         "pipeline-stage-summary",
		Title:        "Most findings cluster at the " + dominant + " stage of the SDK pipeline",
		Evidence:     Truncate(evidence, 400),
		Suggestion:   suggestion,
		FixClass:     "pipeline",
		RelatedCodes: related,
	}
}

// Diagnose is the public entry point. It reads the JSONL span log,
// groups spans by run + trace, and runs all checks. Returns the
// populated DoctorReport. On a missing path, the report still contains
// a file-not-found finding (and SpansRead/TraceCount/RunCount are 0).
func Diagnose(path string, opts Options) DoctorReport {
	var findings []DoctorFinding
	spans, invalidLines := ReadSpans(path, &findings)

	if len(invalidLines) > 0 {
		severity := "error"
		if len(spans) > 0 {
			severity = "warning"
		}
		shown := make([]string, 0, 5)
		for i, ln := range invalidLines {
			if i >= 5 {
				break
			}
			shown = append(shown, intToString(int64(ln)))
		}
		findings = append(findings, DoctorFinding{
			Severity:   severity,
			Code:       "invalid-jsonl",
			Title:      "Span log contains invalid JSON lines",
			Evidence:   Truncate("Invalid line numbers: "+strings.Join(shown, ", "), 400),
			Suggestion: "Use a processed span log written by NEATLOGS_LOG_SPANS_FILE.",
		})
	}

	if len(spans) == 0 && !hasCode(findings, "file-not-found") {
		findings = append(findings, DoctorFinding{
			Severity:   "error",
			Code:       "no-spans",
			Title:      "No spans found",
			Evidence:   Truncate(path+" did not contain any processed span records.", 400),
			Suggestion: "Set NEATLOGS_LOG_SPANS=true, run the app again, then call neatlogs.flush() and neatlogs.shutdown() before the process exits.",
		})
	}

	runs := GroupByRun(spans)

	// Filter to the requested run if specified.
	if opts.RunID != "" {
		if runSpans, ok := runs[opts.RunID]; ok {
			runs = map[string][]Span{opts.RunID: runSpans}
		} else {
			availableKeys := make([]string, 0, len(runs))
			for k := range runs {
				availableKeys = append(availableKeys, k)
			}
			sort.Strings(availableKeys)
			shown := availableKeys
			if len(shown) > 5 {
				shown = shown[:5]
			}
			findings = append(findings, DoctorFinding{
				Severity:   "error",
				Code:       "run-id-not-found",
				Title:      "Requested run id not present in log",
				Evidence:   Truncate("run_id='"+opts.RunID+"' but log has runs: ["+strings.Join(shown, ", ")+"]", 400),
				Suggestion: "Omit --run-id to analyze all runs, or pick one from the list.",
			})
			runs = map[string][]Span{}
		}
	}

	// Multi-run warning.
	if len(runs) > 1 && opts.RunID == "" {
		findings = append(findings, DoctorFinding{
			Severity:   "warning",
			Code:       "multi-run-log",
			Title:      "Log file contains spans from multiple runs",
			Evidence:   Truncate(intToString(int64(len(runs)))+" runs detected, "+intToString(int64(len(spans)))+" spans total. Pass --run-id <id> to scope the report to one run.", 400),
			Suggestion: "Rotate NEATLOGS_LOG_SPANS_FILE between runs, or use --run-id.",
		})
	}

	anyScopeSeen := false
	for runID, runSpans := range runs {
		traces := GroupByTrace(runSpans)
		for traceID, traceSpans := range traces {
			findings = append(findings, diagnoseTrace(traceID, traceSpans, runID, opts.ReadPromptContent)...)
		}
		scopeFindings, scopeSeen := foreignInstrumentationFindings(runSpans, runID)
		anyScopeSeen = anyScopeSeen || scopeSeen
		findings = append(findings, scopeFindings...)
	}

	// Report-level: if NO run had instrumentation_scope, emit a single
	// info finding (not N).
	if !anyScopeSeen && len(spans) > 0 {
		findings = append(findings, DoctorFinding{
			Severity:   "info",
			Code:       "scope-not-preserved",
			Title:      "instrumentation_scope not in the log — foreign detection unavailable",
			Evidence:   Truncate("All "+intToString(int64(len(spans)))+" span(s) lack instrumentation_scope. Foreign-instrumentation detection cannot run.", 400),
			Suggestion: "Update neatlogs to a version that preserves instrumentation_scope in the span log (see neatlogs/core/span_processor.py).",
		})
	}

	// Optional: filter to only foreign-instrumentation findings.
	if opts.ForeignOnly {
		filtered := findings[:0]
		for _, f := range findings {
			if strings.HasPrefix(f.Code, "foreign-instrumentation") {
				filtered = append(filtered, f)
			}
		}
		findings = filtered
	}

	// Run-level pipeline-stage summary. Suppressed under --foreign-only.
	if !opts.ForeignOnly {
		if summary := pipelineStageRunFinding(findings); summary != nil {
			findings = append(findings, *summary)
		}
	}

	// Stable sort: errors first, then warnings, then info; alphabetical by code.
	sort.SliceStable(findings, func(i, j int) bool {
		return severityLess(findings[i], findings[j])
	})

	// Ensure slices are non-nil so JSON serializes them as `[]` not `null`
	// (matches the Python reference's `[]` default).
	if invalidLines == nil {
		invalidLines = []int{}
	}
	if findings == nil {
		findings = []DoctorFinding{}
	}

	return DoctorReport{
		Path:         path,
		SpansRead:    len(spans),
		TraceCount:   CountTraces(runs),
		RunCount:     len(runs),
		InvalidLines: invalidLines,
		Findings:     findings,
	}
}

// diagnoseTrace runs the per-trace checks in the order specified in the
// handoff §6.1. Empty visible → no findings. Then build child_map, then
// the 5 pre-launch reliability dimensions (always run), then
// rootless-http-only (early return), then missing-root-kind, then
// hierarchy pathologies, then agent-without-llm, then missing-io.
//
// PR #21 added the OTel GenAI (3d) and token-waste (3e) dimensions; they
// run alongside the existing 3 and also before the early return.
func diagnoseTrace(traceID string, spans []Span, runID string, readPromptContent bool) []DoctorFinding {
	var findings []DoctorFinding

	// Apply visibility filter.
	visible := make([]Span, 0, len(spans))
	for _, s := range spans {
		if !IsInternal(s) {
			visible = append(visible, s)
		}
	}
	if len(visible) == 0 {
		return findings
	}

	roots := findRootSpans(visible)
	_ = rootKindsSet(roots) // rootKinds set is read inside the missing-root-kind check below

	// 0. Build child map, span_ids, duplicates.
	spanChildMap := buildSpanChildMap(visible)
	spanIDs, duplicates := buildSpanIDSets(visible)

	// 1-5. Pre-launch reliability dimensions (always run, even on
	// unusual trace shapes). PR #21 added steps 3d (OTel GenAI) and
	// 3e (token-waste).
	findings = append(findings, initOrderFindings(visible, traceID, runID)...)
	findings = append(findings, attributeCompletenessFindings(visible, traceID, runID)...)
	findings = append(findings, dataIntegrityFindings(visible, traceID, runID)...)
	findings = append(findings, otelGenaiFindings(visible, traceID, runID)...)
	findings = append(findings, tokenWasteFindings(visible, traceID, runID, readPromptContent)...)

	// 4. rootless-http-only check → early return.
	if isRootlessHTTPOnly(visible) {
		findings = append(findings, DoctorFinding{
			Severity:   "warning",
			Code:       "rootless-http-only",
			Title:      "Trace only contains rootless HTTP spans",
			Evidence:   Truncate(intToString(int64(len(visible)))+" HTTP span(s) have no traced parent.", 400),
			Suggestion: "Wrap the request, job, or script entry point in @span(kind=\"WORKFLOW\") so HTTP calls belong to an application trace.",
			TraceID:    traceID,
			RunID:      runID,
		})
		return findings
	}

	// 5. missing-root-kind check.
	rootKinds := rootKindsSet(roots)
	hasRoot := false
	for k := range rootKinds {
		if _, ok := RootKinds[k]; ok {
			hasRoot = true
			break
		}
	}
	if !hasRoot {
		kinds := make([]string, 0, len(rootKinds))
		for k := range rootKinds {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		evidence := "Root span kinds: " + strings.Join(kinds, ", ")
		if len(kinds) == 0 {
			evidence = "Root span kinds: none"
		}
		findings = append(findings, DoctorFinding{
			Severity:   "warning",
			Code:       "missing-root-kind",
			Title:      "Trace has no workflow, chain, agent, or MCP tool root",
			Evidence:   Truncate(evidence, 400),
			Suggestion: "Add @span(kind=\"WORKFLOW\") to the entry point, or use a supported provider wrapper that creates an automatic root span.",
			TraceID:    traceID,
			RunID:      runID,
		})
	}

	// 6. Hierarchy pathologies (in order).
	findings = append(findings, orphanParentFindings(visible, spanIDs, traceID, runID)...)
	findings = append(findings, selfParentFindings(visible, traceID, runID)...)
	findings = append(findings, duplicateSpanIDFindings(duplicates, visible, traceID, runID)...)
	findings = append(findings, multipleRootsFindings(roots, traceID, runID)...)

	// Cycle detection: build id-only child map for the walker.
	idChildMap := map[string][]string{}
	for pid, kids := range spanChildMap {
		ids := make([]string, 0, len(kids))
		for _, c := range kids {
			if id := SpanID(c); id != "" {
				ids = append(ids, id)
			}
		}
		idChildMap[pid] = ids
	}
	findings = append(findings, findCycles(visible, idChildMap, traceID, runID)...)

	// 7. agent-without-llm (subtree-based).
	findings = append(findings, agentWithoutLLMFindings(visible, spanChildMap, traceID, runID)...)

	// 8. missing-io.
	findings = append(findings, missingIOFindings(visible, traceID, runID)...)

	return findings
}

// hasCode reports whether the given findings slice already contains a
// finding with the specified code (used to avoid duplicates, e.g. file-not-found).
func hasCode(findings []DoctorFinding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
