// PR #21: manual-fix snippets for `--emit-fix` (handoff §4.16.C).
//
// §12.8.3: snippet source is plain text with newlines; the renderer
// joins with "\n" and the CLI writes the result to stdout verbatim.
// Do NOT JSON-encode the snippet. We intentionally skip AST-based
// auto-fix: rewrites are fragile across project structures (Jupyter,
// K8s init, generated code). The user copy-pastes the snippet — correct
// by construction.

package doctor

import (
	"fmt"
	"sort"
	"strings"
)

// FixSnippet is a registered manual-fix snippet.
type FixSnippet struct {
	Description string
	Before      string
	After       string
}

// fixSnippets holds the 4 registered snippets. Codes match the
// DoctorFinding.Code of the issue they fix.
var fixSnippets = map[string]FixSnippet{
	"init-after-client": {
		Description: "Move neatlogs.init() to the top of the entry point (before any LLM client is constructed).",
		Before: "from openai import OpenAI\n" +
			"import neatlogs\n" +
			"neatlogs.init(api_key=os.environ['NEATLOGS_API_KEY'])\n",
		After: "import neatlogs\n" +
			"neatlogs.init(api_key=os.environ['NEATLOGS_API_KEY'])\n" +
			"from openai import OpenAI\n",
	},
	"missing-span-kind": {
		Description: "Set neatlogs.span.kind on every emitted span, either via the @neatlogs.span decorator or the wrapper.",
		Before:     "from neatlogs import trace\n\n@trace\ndef my_function():\n    ...\n",
		After:      "from neatlogs import trace\n\n@trace(kind='TOOL')\ndef my_function():\n    ...\n",
	},
	"zero-duration-span": {
		Description: "The wrapper exited the span before calling .end() — fix the exception path.",
		Before: "def patched(*args, **kwargs):\n" +
			"    span = tracer.start_span('my_op')\n" +
			"    response = orig(*args, **kwargs)\n" +
			"    return response  # bug: span.end() never called on the error path\n",
		After: "def patched(*args, **kwargs):\n" +
			"    span = tracer.start_span('my_op')\n" +
			"    try:\n" +
			"        return orig(*args, **kwargs)\n" +
			"    finally:\n" +
			"        span.end()\n",
	},
	"error-status-no-event": {
		Description: "Call record_exception() inside the wrapper's except block so the error view shows the stack trace.",
		Before: "try:\n" +
			"    response = orig(*args, **kwargs)\n" +
			"except Exception as e:\n" +
			"    span.set_status(StatusCode.ERROR)\n" +
			"    raise\n",
		After: "try:\n" +
			"    response = orig(*args, **kwargs)\n" +
			"except Exception as e:\n" +
			"    span.set_status(StatusCode.ERROR, str(e))\n" +
			"    span.record_exception(e)\n" +
			"    raise\n",
	},
}

// FixSnippetCodes returns a sorted list of all registered snippet codes.
// Used by the CLI to build the "known codes" stderr message.
func FixSnippetCodes() []string {
	out := make([]string, 0, len(fixSnippets))
	for k := range fixSnippets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RenderFixSnippet renders a manual-fix snippet for the given code, or
// empty string if the code has no registered snippet. The output is
// plain text suitable for piping to a file or for the user to copy-paste.
func RenderFixSnippet(code string) string {
	s, ok := fixSnippets[code]
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Finding: %s\n", code)
	fmt.Fprintf(&b, "# Suggested: %s\n\n", s.Description)
	fmt.Fprintf(&b, "# BEFORE:\n%s\n\n", s.Before)
	fmt.Fprintf(&b, "# AFTER:\n%s\n", s.After)
	return b.String()
}
