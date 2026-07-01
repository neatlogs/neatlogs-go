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
// startProviderSpan transparently opens a WORKFLOW root (named after the
// configured workflow) when the span being started is a parentless non-root
// kind, nesting the provider span under it. It returns the child context, the
// provider span, and an end func that ends the provider span and then the
// auto-root. This mirrors the auto-root in the Python and TypeScript SDKs.

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

// startProviderSpan starts a span of the given kind, opening an auto-root first
// when needed. The returned end func ends the provider span and the auto-root
// (if one was opened); callers must invoke it exactly once. Every provider-span
// kind passes through here, so all wrapper paths (sync, stream finalize, error)
// get auto-root coverage by calling end().
func startProviderSpan(ctx context.Context, name, kind string) (context.Context, trace.Span, func()) {
	needsRoot := autoRootEnabled()
	if _, isRoot := rootKinds[kind]; isRoot {
		needsRoot = false
	}
	if parent := trace.SpanFromContext(ctx); parent != nil && parent.SpanContext().IsValid() && parent.IsRecording() {
		needsRoot = false // a recording parent already anchors the trace
	}

	t := tracer()
	if !needsRoot {
		ctx, span := t.Start(ctx, name)
		return ctx, span, func() { span.End() }
	}

	// Session/end-user identity (bound via Identify) is stamped on this auto-root
	// by the identityProcessor — it reads the start context, so no inline
	// stamping is needed here.
	rootCtx, root := t.Start(ctx, resolveRootWorkflowName(), trace.WithAttributes(
		attribute.String(attrs.SpanKind, attrs.KindWorkflow),
		attribute.Bool("neatlogs.auto_root", true),
	))
	childCtx, span := t.Start(rootCtx, name)
	return childCtx, span, func() {
		span.End()
		root.End()
	}
}
