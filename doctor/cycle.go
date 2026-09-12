package doctor

import "fmt"

// cycleWalker runs an iterative DFS over the span parent/child tree and
// reports back-edges as cycles. Self-cycles (span == parent) are filtered
// out before the walk; they get their own `self-parent` finding.
//
// Algorithm: O(V + E). Two sets tracked:
//   - inPath: nodes currently on the DFS stack
//   - done:   nodes whose entire subtree has been fully explored
//
// A back-edge is detected when a child id is in inPath. We use an explicit
// stack of (node, child-index) frames to avoid recursion (Python's stack
// overflows around 1K spans; Go has a limit too but it's higher).
type cycleWalker struct {
	inPath map[string]bool
	done   map[string]bool
	nameOf map[string]string
}

// findCycles reports up to 1 cycle per back-edge found. The reference
// implementation emits a finding per cycle path; for portability we
// surface each cycle as a separate finding.
func findCycles(spans []Span, childMap map[string][]string, traceID, runID string) []DoctorFinding {
	// Filter out self-parent spans (sid == pid). They get a `self-parent`
	// finding elsewhere; reporting them as cycles would be a duplicate.
	filtered := make([]Span, 0, len(spans))
	for _, s := range spans {
		if SpanID(s) == ParentSpanID(s) {
			continue
		}
		filtered = append(filtered, s)
	}

	w := &cycleWalker{
		inPath: map[string]bool{},
		done:   map[string]bool{},
		nameOf: map[string]string{},
	}
	for _, s := range filtered {
		if id := SpanID(s); id != "" {
			w.nameOf[id] = SpanName(s)
		}
	}

	var findings []DoctorFinding

	// iterative DFS from every unvisited node
	for _, s := range filtered {
		sid := SpanID(s)
		if sid == "" {
			continue
		}
		if w.done[sid] {
			continue
		}
		w.walk(sid, childMap, &findings, traceID, runID)
	}
	return findings
}

func (w *cycleWalker) walk(start string, childMap map[string][]string, findings *[]DoctorFinding, traceID, runID string) {
	if w.done[start] {
		return
	}
	path := []string{start}
	w.inPath[start] = true
	// frames: stack of (node, child-index)
	type frame struct {
		node string
		i    int
	}
	frames := []frame{{node: start, i: 0}}

	for len(frames) > 0 {
		top := &frames[len(frames)-1]
		kids := childMap[top.node]
		if top.i >= len(kids) {
			// Done with this node's children; pop.
			frames = frames[:len(frames)-1]
			w.inPath[top.node] = false
			w.done[top.node] = true
			path = path[:len(path)-1]
			continue
		}
		cid := kids[top.i]
		top.i++

		if w.done[cid] {
			continue
		}
		if w.inPath[cid] {
			// Back-edge: a cycle. Report and skip.
			cycle := buildCyclePath(path, cid)
			*findings = append(*findings, cycleFinding(cycle, w.nameOf, traceID, runID))
			continue
		}
		// Recurse
		path = append(path, cid)
		w.inPath[cid] = true
		frames = append(frames, frame{node: cid, i: 0})
	}
}

// buildCyclePath extracts the cycle from the current DFS path given the
// back-edge target. The path looks like [...prefix, c1, c2, ..., cN] and
// the back-edge points to cK (where cK is in the path). The cycle is
// cK → cK+1 → ... → cN → cK.
func buildCyclePath(path []string, backTo string) []string {
	for i, n := range path {
		if n == backTo {
			out := append([]string{}, path[i:]...)
			out = append(out, backTo)
			return out
		}
	}
	// Should not happen given the DFS, but fall back to the whole path.
	out := append([]string{}, path...)
	out = append(out, backTo)
	return out
}

func cycleFinding(cycle []string, nameOf map[string]string, traceID, runID string) DoctorFinding {
	name := nameOf[cycle[0]]
	if name == "" {
		name = "<unnamed>"
	}
	// Render: "A → B → C → A"
	joined := ""
	for i, id := range cycle {
		if i > 0 {
			joined += " → "
		}
		joined += id
	}
	display := joined
	if len(cycle) > 6 {
		// Truncate to first 6: "A → B → C → D → E → F → ..."
		parts := make([]string, 0, 7)
		for i := 0; i < 6; i++ {
			parts = append(parts, cycle[i])
		}
		parts = append(parts, "...")
		display = ""
		for i, p := range parts {
			if i > 0 {
				display += " → "
			}
			display += p
		}
	}
	return DoctorFinding{
		Severity:   "error",
		Code:       "cycle",
		Title:      "Span hierarchy contains a cycle",
		Evidence:   Truncate(fmt.Sprintf("span '%s' is in a cycle: %s", name, display), 400),
		Suggestion: "Wrap a function that re-enters itself with a guard, or fix the wrapper that is producing the cycle.",
		TraceID:    traceID,
		RunID:      runID,
	}
}

// buildChildMap constructs a parent_id → []child_id index from a slice of
// spans. The result is a map from parent span id to the ids of its direct
// children (in encounter order).
func buildChildMap(spans []Span) map[string][]string {
	out := map[string][]string{}
	for _, s := range spans {
		pid := ParentSpanID(s)
		if pid == "" {
			continue
		}
		cid := SpanID(s)
		if cid == "" {
			continue
		}
		out[pid] = append(out[pid], cid)
	}
	return out
}
