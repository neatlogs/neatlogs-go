package doctor

import (
	"sort"
	"strings"
)

// initOrderFindings detects wrappers created BEFORE neatlogs.init(). The
// OTel SDK is loaded (we get a span out) but our attribute processor never
// ran, so none of the INIT_MARKER_KEYS are set on the span. Emits ONE
// finding per trace (the rest are downstream of the same root cause).
func initOrderFindings(spans []Span, traceID, runID string) []DoctorFinding {
	for _, s := range spans {
		attrs := Attributes(s)
		hasMarker := false
		if attrs != nil {
			for _, k := range InitMarkerKeys {
				if _, ok := attrs[k]; ok {
					hasMarker = true
					break
				}
			}
		}
		if hasMarker {
			continue
		}
		// Span is present but no init marker (either no attributes, or
		// attributes lack all init marker keys). This is the
		// wrapper-before-init signature. Only emit one per trace.
		return []DoctorFinding{{
			Severity:             "error",
			Code:                 "init-after-client",
			Title:                "Span has no Neatlogs init markers — wrapper likely created before neatlogs.init()",
			Evidence:             Truncate("span '"+SpanName(s)+"' has none of "+keysAsList(InitMarkerKeys), 400),
			Suggestion:           "Move neatlogs.init() to the very top of your entry point, BEFORE constructing any LLM client (openai.Anthropic(), ChatOpenAI(), genai.Client(), etc.). If you cannot reorder, call neatlogs.shutdown() then neatlogs.init() again to re-attach the wrappers.",
			TraceID:              traceID,
			RunID:                runID,
			FixClass:             "init_order",
			AutomatedFixAvailable: true,
			DocURL:               "skills/neatlogs/references/troubleshooting.md#1-import-order-issues-most-common-mistake",
			RelatedCodes:         []string{"no-spans", "missing-root-kind"},
		}}
	}
	return nil
}

// attributeCompletenessFindings emits missing-span-kind when some spans
// lack neatlogs.span.kind. Suppressed when ALL spans miss the kind
// (that's the init-order symptom; init-order check handles it) and when
// no spans miss the kind.
func attributeCompletenessFindings(spans []Span, traceID, runID string) []DoctorFinding {
	missing := 0
	var examples []string
	for _, s := range spans {
		attrs := Attributes(s)
		if attrs == nil {
			missing++
			if len(examples) < 3 {
				examples = append(examples, SpanName(s))
			}
			continue
		}
		if _, ok := attrs["neatlogs.span.kind"]; !ok {
			missing++
			if len(examples) < 3 {
				examples = append(examples, SpanName(s))
			}
		}
	}
	if missing == 0 {
		return nil
	}
	// Suppress when ALL spans miss the kind — that's the init-order symptom.
	if missing == len(spans) {
		return nil
	}
	evidence := intToString(int64(missing)) + " of " + intToString(int64(len(spans))) + " span(s) missing neatlogs.span.kind: " + strings.Join(examples, ", ")
	if missing > 3 {
		evidence += " ..."
	}
	return []DoctorFinding{{
		Severity:   "warning",
		Code:       "missing-span-kind",
		Title:      "Some spans lack neatlogs.span.kind — dashboard will mis-categorize them",
		Evidence:   Truncate(evidence, 400),
		Suggestion: "Set neatlogs.span.kind on every emitted span. @neatlogs.span(kind=...) populates it automatically; if you wrap a third-party client, the wrapper should set it.",
		TraceID:    traceID,
		RunID:      runID,
		FixClass:   "attribute",
		DocURL:     "skills/neatlogs/references/troubleshooting.md#6-common-anti-patterns-table",
	}}
}

// dataIntegrityFindings runs the three data-integrity sub-checks per
// visible span (internal spans are excluded): zero-duration,
// error-status-no-event, and latency-mismatch. Each sub-check emits ONE
// finding per trace.
func dataIntegrityFindings(spans []Span, traceID, runID string) []DoctorFinding {
	var zeroDur, errorNoEvent, latencyMismatch []string
	for _, s := range spans {
		if IsInternal(s) {
			continue
		}
		name := SpanName(s)
		// start/end/duration are int or float (ns). Booleans not possible.
		startRaw, hasStart := s["start_time"]
		endRaw, hasEnd := s["end_time"]
		duration, hasDuration := s["duration_ns"]
		status, _ := s["status"]
		// events is []any of dicts
		hasException := HasExceptionEvent(s)

		// a) zero duration: duration_ns == 0 OR (start and end present, equal)
		if isZeroDuration(duration, hasDuration, startRaw, hasStart, endRaw, hasEnd) {
			zeroDur = append(zeroDur, name)
		}
		// c) latency mismatch: end < start (both numbers)
		if isLatencyMismatch(startRaw, hasStart, endRaw, hasEnd) {
			latencyMismatch = append(latencyMismatch, name)
		}
		// b) error status without exception event
		if SpanStatusIsError(status) && !hasException {
			errorNoEvent = append(errorNoEvent, name)
		}
	}
	var findings []DoctorFinding
	if len(zeroDur) > 0 {
		evidence := intToString(int64(len(zeroDur))) + " span(s) with zero duration: " + strings.Join(firstN(zeroDur, 3), ", ")
		if len(zeroDur) > 3 {
			evidence += " ..."
		}
		findings = append(findings, DoctorFinding{
			Severity:     "warning",
			Code:         "zero-duration-span",
			Title:        "Some spans ended instantly (duration_ns == 0)",
			Evidence:     Truncate(evidence, 400),
			Suggestion:   "Wrapper likely crashed before span.end(), or an async wrapper did not await. Check the wrapper's exception path and register it with @contextlib.asynccontextmanager if the client is async.",
			TraceID:      traceID,
			RunID:        runID,
			FixClass:     "data_integrity",
			RelatedCodes: []string{"error-status-no-event"},
		})
	}
	if len(errorNoEvent) > 0 {
		evidence := intToString(int64(len(errorNoEvent))) + " span(s): " + strings.Join(firstN(errorNoEvent, 3), ", ")
		if len(errorNoEvent) > 3 {
			evidence += " ..."
		}
		findings = append(findings, DoctorFinding{
			Severity:     "warning",
			Code:         "error-status-no-event",
			Title:        "Spans marked ERROR but no exception event recorded",
			Evidence:     Truncate(evidence, 400),
			Suggestion:   "Attach an exception event with stack trace when marking a span ERROR. Without it, the dashboard's error view shows the span as red but offers no detail. Use opentelemetry's record_exception() inside the wrapper's except block.",
			TraceID:      traceID,
			RunID:        runID,
			FixClass:     "data_integrity",
			RelatedCodes: []string{"zero-duration-span"},
		})
	}
	if len(latencyMismatch) > 0 {
		evidence := intToString(int64(len(latencyMismatch))) + " span(s) with end < start: " + strings.Join(firstN(latencyMismatch, 3), ", ")
		if len(latencyMismatch) > 3 {
			evidence += " ..."
		}
		findings = append(findings, DoctorFinding{
			Severity:   "error",
			Code:       "latency-mismatch",
			Title:      "Span end_time is before start_time",
			Evidence:   Truncate(evidence, 400),
			Suggestion: "Clock issue: the wrapper captured start_time and end_time from different clocks. Call time.time_ns() (or perf_counter_ns()) once per phase and use that one source for both.",
			TraceID:    traceID,
			RunID:      runID,
			FixClass:   "data_integrity",
		})
	}
	return findings
}

func isZeroDuration(duration any, hasDuration bool, start any, hasStart bool, end any, hasEnd bool) bool {
	if hasDuration {
		if n, ok := toNumber(duration); ok && n == 0 {
			return true
		}
	}
	if hasStart && hasEnd {
		s, sok := toNumber(start)
		e, eok := toNumber(end)
		if sok && eok && s == e {
			return true
		}
	}
	return false
}

func isLatencyMismatch(start any, hasStart bool, end any, hasEnd bool) bool {
	if !hasStart || !hasEnd {
		return false
	}
	s, sok := toNumber(start)
	e, eok := toNumber(end)
	if !sok || !eok {
		return false
	}
	return e < s
}

// toNumber coerces int, int64, float64 to int64 (the only numeric types
// JSON decodes ints/floats to in Go). Other types return false.
func toNumber(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		// JSON ints decode to float64; we want equality comparisons, so
		// truncate to int64. (Sub-ns precision is not relevant here.)
		return int64(x), true
	case float32:
		return int64(x), true
	}
	return 0, false
}

// keysAsList formats a []string as a Python-list-style string for evidence
// fields, e.g. ['a', 'b'].
func keysAsList(keys []string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = "'" + k + "'"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// sortedKeys returns the keys of a map in sorted order (for stable
// output, e.g. in finding evidence).
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
