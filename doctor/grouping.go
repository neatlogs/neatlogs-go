package doctor

// GroupByRun groups spans by run (session.id when present, else trace_id
// fallback, else the DefaultSessionID sentinel).
//
// A "run" is one execution of the user's app. The doctor treats each run
// independently so cross-run pollution does not look like a real bug.
func GroupByRun(spans []Span) map[string][]Span {
	runs := map[string][]Span{}
	for _, span := range spans {
		if session := SessionID(span); session != "" {
			runs[session] = append(runs[session], span)
			continue
		}
		key := TraceID(span)
		if key == "" {
			key = DefaultSessionID
		}
		runs[key] = append(runs[key], span)
	}
	return runs
}

// GroupByTrace groups spans by trace_id within a single run.
// Missing trace_id maps to "unknown" (matching the Python reference).
func GroupByTrace(spans []Span) map[string][]Span {
	traces := map[string][]Span{}
	for _, span := range spans {
		tid := TraceID(span)
		if tid == "" {
			tid = "unknown"
		}
		traces[tid] = append(traces[tid], span)
	}
	return traces
}

// CountTraces returns the number of distinct (trace_id, run_id) pairs,
// matching the Python reference's _iter_traces: a trace_id is counted
// once per run that contains it (so two runs sharing a trace_id are
// counted as 2 traces — the cross-run pollution the multi-run-log
// finding warns about).
func CountTraces(runs map[string][]Span) int {
	total := 0
	for _, runSpans := range runs {
		seen := map[string]struct{}{}
		for _, span := range runSpans {
			tid := TraceID(span)
			if tid == "" {
				tid = "unknown"
			}
			seen[tid] = struct{}{}
		}
		total += len(seen)
	}
	return total
}
