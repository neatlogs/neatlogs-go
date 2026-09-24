// Package doctor is a local, read-only linter for span log files written by
// the Neatlogs log exporter. It is the Go port of neatlogs/doctor.py —
// behavior is intended to be byte-for-byte identical to the Python reference.
//
// Usage (CLI): see cmd/neatlogs-doctor.
//
// Usage (programmatic):
//
//	report, _ := doctor.Diagnose(ctx, "spans.log", doctor.Options{})
//	fmt.Println(doctor.FormatReport(report))
//	if report.HasErrors() { os.Exit(1) }
package doctor

// Span is a single span log entry as read from a JSONL file.
//
// The input is JSON with many optional keys; we represent it as a generic
// map to mirror the Python reference's flexibility. Typed extraction happens
// via helpers in util.go.
type Span = map[string]any

// SpanKind enumerates the canonical span kind values used in the
// neatlogs.span.kind attribute. The doctor treats anything not in this set
// as UNKNOWN (the empty string).
const (
	KindWorkflow  = "workflow"
	KindChain     = "chain"
	KindAgent     = "agent"
	KindTool      = "tool"
	KindLLM       = "llm"
	KindEmbedding = "embedding"
	KindRetriever = "retriever"
	KindHTTP      = "http"
	KindMCPTool   = "mcp_tool"
)

// RootKinds are the span kinds the doctor treats as orchestration roots.
// At least one of these must appear in a trace, or missing-root-kind fires.
var RootKinds = map[string]struct{}{
	KindWorkflow: {},
	KindChain:    {},
	KindAgent:    {},
	KindMCPTool:  {},
}

// IOKinds are the span kinds where input + output attributes are expected.
var IOKinds = map[string]struct{}{
	KindLLM:       {},
	KindTool:      {},
	KindRetriever: {},
	KindEmbedding: {},
}

// NeatlogsScopePrefix is the prefix used to recognize own-scope spans.
// Anything other than "neatlogs" (or "neatlogs.*") is foreign.
const NeatlogsScopePrefix = "neatlogs"

// DefaultSessionID is the sentinel used when a span has neither a session.id
// attribute nor a trace_id to key its run on.
const DefaultSessionID = "<no-session>"

// MaxEvidenceLen is the maximum length of a finding's evidence string.
// Anything longer is truncated with a "..." suffix.
const MaxEvidenceLen = 200

// MinReasonableDurationNS is the expected default duration for an instant
// span. Below this is treated as a zero-duration finding. (Exported for
// tests; not used by the data-integrity check, which compares exact values.)
const MinReasonableDurationNS = 1_000_000

// InitMarkerKeys are the attribute keys the SDK checks before claiming init
// succeeded. If a span is present but none of these are set, the most likely
// cause is a wrapper created BEFORE neatlogs.init().
var InitMarkerKeys = []string{
	"neatlogs.instrumentation.name",
	"neatlogs.span.kind",
	"neatlogs.workflow_name",
}

// RequiredSpanAttributes are the attributes every emitted span should have.
// Used by the attribute-completeness check.
var RequiredSpanAttributes = []string{"neatlogs.span.kind"}

// DoctorFinding is a single diagnostic finding emitted by the doctor.
//
// The optional FixClass, AutomatedFixAvailable, DocURL, and RelatedCodes
// fields make findings self-describing for coding agents and LLMs.
type DoctorFinding struct {
	Severity             string   `json:"severity"`
	Code                 string   `json:"code"`
	Title                string   `json:"title"`
	Evidence             string   `json:"evidence"`
	Suggestion           string   `json:"suggestion"`
	TraceID              string   `json:"trace_id,omitempty"`
	RunID                string   `json:"run_id,omitempty"`
	FixClass             string   `json:"fix_class,omitempty"`
	AutomatedFixAvailable bool    `json:"automated_fix_available,omitempty"`
	DocURL               string   `json:"doc_url,omitempty"`
	RelatedCodes         []string `json:"related_codes,omitempty"`
}

// DoctorReport is the full diagnostic report for one span log file.
type DoctorReport struct {
	Path         string           `json:"path"`
	SpansRead    int              `json:"spans_read"`
	TraceCount   int              `json:"trace_count"`
	RunCount     int              `json:"run_count"`
	InvalidLines []int            `json:"invalid_lines"` // always non-nil; serialized as `[]`
	Findings     []DoctorFinding `json:"findings"`       // always non-nil; serialized as `[]`
}

// HasErrors returns true if any finding has severity "error".
func (r *DoctorReport) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == "error" {
			return true
		}
	}
	return false
}

// FindingsByFixClass groups findings by their FixClass. Findings without
// a FixClass are dropped.
func (r *DoctorReport) FindingsByFixClass() map[string][]DoctorFinding {
	out := map[string][]DoctorFinding{}
	for _, f := range r.Findings {
		if f.FixClass == "" {
			continue
		}
		out[f.FixClass] = append(out[f.FixClass], f)
	}
	return out
}

// FixClassToStage maps the 8 fix_class values to 4 pipeline stages.
var FixClassToStage = map[string]string{
	"init_order":      "init",
	"config":          "init",
	"pipeline":        "init",
	"instrumentation": "instrument",
	"capture":         "instrument",
	"data_integrity":  "span",
	"attribute":       "span",
	"hierarchy":       "hierarchy",
}

// FindingsByPipelineStage groups findings by their pipeline stage, using
// FixClassToStage. Findings whose FixClass is not in the map are dropped.
func (r *DoctorReport) FindingsByPipelineStage() map[string][]DoctorFinding {
	out := map[string][]DoctorFinding{}
	for _, f := range r.Findings {
		stage, ok := FixClassToStage[f.FixClass]
		if !ok {
			continue
		}
		out[stage] = append(out[stage], f)
	}
	return out
}

// severityRank sorts findings: error < warning < info.
var severityRank = map[string]int{"error": 0, "warning": 1, "info": 2}

// SeverityLess reports whether a should appear before b in the sorted
// output (by (severity, code)).
func severityLess(a, b DoctorFinding) bool {
	ra := severityRank[a.Severity]
	rb := severityRank[b.Severity]
	if ra != rb {
		return ra < rb
	}
	return a.Code < b.Code
}

// Options configures a Diagnose call.
type Options struct {
	// RunID, if set, only analyzes spans whose session.id (or trace_id
	// fallback) matches. Useful when one log file contains many runs.
	RunID string
	// ForeignOnly, if true, only returns foreign-instrumentation findings.
	ForeignOnly bool
}
