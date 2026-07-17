package neatlogs

import (
	"context"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// The backend's trace-finalizer only renders a trace once it contains a
// PARENTLESS span of a root-eligible kind (workflow / chain / agent / mcp_tool).
// Direct-provider wrappers like WrapGenAI emit only non-root spans (llm /
// embedding / tool), so a bare `gc := WrapGenAI(client); gc.GenerateContent(...)`
// with no surrounding span would produce an orphan llm-rooted trace that the
// finalizer skips ("No WORKFLOW/CHAIN/AGENT/MCP_TOOL root found").
//
// StartProviderSpan transparently opens a WORKFLOW root (named after the
// configured workflow) when the span being started is a parentless non-root
// kind, nesting the provider span under it. It returns the child context, the
// provider span, and an end func that ends the provider span and then the
// auto-root. This mirrors the auto-root in the Python and TypeScript SDKs.
//
// It is the building block SDK wrappers use to emit non-root spans (llm /
// embedding / tool) without producing an orphan trace; the genai wrapper (in
// the neatlogs-go/genai subpackage) is one such caller. Applications typically
// use Trace or StartSpan instead.

// rootKinds are the span kinds that already satisfy the backend's root
// requirement and so must never be wrapped in another root.
var rootKinds = map[string]struct{}{
	attrs.KindWorkflow: {},
	"chain":            {},
	attrs.KindAgent:    {},
	"mcp_tool":         {},
}

var (
	workflowNameMu sync.RWMutex
	workflowName   = "workflow"
)

// setWorkflowName records the resolved workflow name so auto-root spans can be
// named after it. Called by Init.
func setWorkflowName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	workflowNameMu.Lock()
	workflowName = name
	workflowNameMu.Unlock()
}

func resolveRootWorkflowName() string {
	workflowNameMu.RLock()
	defer workflowNameMu.RUnlock()
	return workflowName
}

func autoRootEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("NEATLOGS_AUTO_ROOT"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// StartProviderSpan starts a span of the given kind, opening an auto-root first
// when needed. The returned end func ends the provider span and the auto-root
// (if one was opened); callers must invoke it exactly once. Every provider-span
// kind passes through here, so all wrapper paths (sync, stream finalize, error)
// get auto-root coverage by calling end().
func StartProviderSpan(ctx context.Context, name, kind string) (context.Context, trace.Span, func()) {
	needsRoot := autoRootEnabled()
	if _, isRoot := rootKinds[kind]; isRoot {
		needsRoot = false
	}
	if privateSpanContext(ctx).IsValid() {
		needsRoot = false // a local or extracted remote parent already anchors the trace
	}

	t := tracer()
	if !needsRoot {
		_, span := t.Start(privateStartContext(ctx), name)
		return withPrivateTraceContext(ctx, span.SpanContext()), span, func() { span.End() }
	}

	// Session/end-user identity (bound via Identify) is stamped on this auto-root
	// by the identityProcessor — it reads the start context, so no inline
	// stamping is needed here.
	rootCtx, root := t.Start(privateStartContext(ctx), resolveRootWorkflowName(), trace.WithAttributes(
		attribute.String(attrs.SpanKind, attrs.KindWorkflow),
		attribute.Bool("neatlogs.auto_root", true),
	))
	_, span := t.Start(rootCtx, name)
	return withPrivateTraceContext(ctx, span.SpanContext()), span, func() {
		span.End()
		root.End()
	}
}
