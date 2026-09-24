// PR #21: OTel GenAI semconv validation + token-waste patterns.
//
// §12.8.2 (PR #21 review): isLLMKind() must run before checking
// gen_ai.operation.name. A tool span with chat op-name is still a
// tool span, not an LLM span.

package doctor

import (
	"encoding/json"
	"sort"
	"strings"
)

// isLLMKind reports whether a span is an LLM operation, by neatlogs
// kind or by an OTel chat-style gen_ai.operation.name.
//
// Order matters: neatlogs.kind=llm takes precedence; a tool span
// with chat op-name is still a tool span.
func isLLMKind(s Span) bool {
	attrs := Attributes(s)
	if attrs["neatlogs.span.kind"] == "llm" {
		return true
	}
	if v, ok := attrs[OTelGenAIOperationName].(string); ok {
		if _, ok := OTelGenaiLLMOperations[v]; ok {
			return true
		}
	}
	return false
}

// otelGenaiFindings validates that LLM-kind spans also carry OTel
// GenAI semconv attrs. Emits otel-genai-missing (warning) when an
// LLM span lacks gen_ai.operation.name, and otel-genai-inconsistent
// (info) when the neatlogs kind and the OTel op-name disagree.
//
// Internal spans are skipped. Foreign-scope spans are skipped
// implicitly: isLLMKind needs neatlogs kind=llm or an OTel chat op.
func otelGenaiFindings(visible []Span, traceID, runID string) []DoctorFinding {
	var findings []DoctorFinding
	var missingCount int
	for _, span := range visible {
		if IsInternal(span) {
			continue
		}
		if !isLLMKind(span) {
			continue
		}
		attrs := Attributes(span)
		otelOp, hasOtelOp := attrs[OTelGenAIOperationName]
		if !hasOtelOp || otelOp == nil {
			missingCount++
			continue
		}
		// Both present: if neatlogs says "llm" and the OTel op is NOT
		// one of the LLM operations, the kinds disagree.
		if attrs["neatlogs.span.kind"] == "llm" {
			if opStr, ok := otelOp.(string); ok {
				if _, isLLMOp := OTelGenaiLLMOperations[opStr]; !isLLMOp {
					findings = append(findings, DoctorFinding{
						Severity:   "info",
						Code:       "otel-genai-inconsistent",
						Title:      "LLM span has mismatched neatlogs/OTel GenAI operation kind",
						Evidence:   Truncate("span '"+SpanName(span)+"' has neatlogs.span.kind='llm' but "+OTelGenAIOperationName+"='"+opStr+"'", 400),
						Suggestion: "Update the wrapper so the neatlogs span kind and the OTel GenAI operation name agree. Reference: https://opentelemetry.io/docs/specs/semconv/gen-ai/",
						TraceID:    traceID,
						RunID:      runID,
						FixClass:   "config",
						RelatedCodes: []string{"missing-span-kind"},
					})
				}
			}
		}
	}
	if missingCount > 0 {
		findings = append(findings, DoctorFinding{
			Severity:   "warning",
			Code:       "otel-genai-missing",
			Title:      "LLM span(s) lack OTel GenAI semantic-convention attributes",
			Evidence:   Truncate(intToString(int64(missingCount))+" LLM span(s) missing "+OTelGenAIOperationName+". Langfuse, Phoenix, and other OTel GenAI tools will skip these.", 400),
			Suggestion: "Set the OTel GenAI attributes on every LLM span. The SDK does this automatically when the span is created via the OTel GenAI instrumentation (e.g. opentelemetry-instrumentation-openai). Reference: https://opentelemetry.io/docs/specs/semconv/gen-ai/",
			TraceID:    traceID,
			RunID:      runID,
			FixClass:   "config",
			RelatedCodes: []string{"otel-genai-inconsistent"},
		})
	}
	return findings
}

// llmPromptSize returns the total char count of an LLM span's prompt.
//
// Walks four locations:
//   - `gen_ai.input.messages` (OTel semconv, list of message dicts)
//   - `neatlogs.llm.input_messages.*` (neatlogs-namespaced; concatenated)
//   - `neatlogs.llm.prompts.*` (older neatlogs layout; concatenated)
//   - `neatlogs.llm.system` (the system prompt)
//
// Returns 0 if no prompt content is found.
func llmPromptSize(s Span) int {
	attrs := Attributes(s)
	n := 0
	// OTel semconv: list of message dicts
	if msgs, ok := attrs["gen_ai.input.messages"].([]any); ok {
		for _, m := range msgs {
			if m, ok := m.(map[string]any); ok {
				if c, ok := m["content"]; ok {
					switch v := c.(type) {
					case string:
						n += len(v)
					case []any:
						for _, part := range v {
							if part, ok := part.(map[string]any); ok {
								if t, ok := part["text"].(string); ok {
									n += len(t)
								}
							}
						}
					}
				}
			}
		}
	}
	// neatlogs namespaced: each numbered attribute holds a serialized message
	for k, v := range attrs {
		if (strings.HasPrefix(k, "neatlogs.llm.input_messages.") ||
			strings.HasPrefix(k, "neatlogs.llm.prompts.")) {
			if str, ok := v.(string); ok {
				n += len(str)
			}
		}
	}
	if sys, ok := attrs["neatlogs.llm.system"].(string); ok {
		n += len(sys)
	}
	return n
}

// llmSystemPrompt returns the system prompt text for an LLM span, or
// empty string. Looks at `neatlogs.llm.system` (neatlogs) and
// `gen_ai.system_instructions` (OTel semconv). The OTel content may be
// a string OR a list of `{type, text}` parts, joined with "\n".
func llmSystemPrompt(s Span) string {
	attrs := Attributes(s)
	if sys, ok := attrs["neatlogs.llm.system"].(string); ok && sys != "" {
		return sys
	}
	si, ok := attrs["gen_ai.system_instructions"].([]any)
	if !ok || len(si) == 0 {
		return ""
	}
	var parts []string
	for _, m := range si {
		if m, ok := m.(map[string]any); ok {
			if c, ok := m["content"]; ok {
				switch v := c.(type) {
				case string:
					parts = append(parts, v)
				case []any:
					for _, part := range v {
						if part, ok := part.(map[string]any); ok {
							if t, ok := part["text"].(string); ok {
								parts = append(parts, t)
							}
						}
					}
				}
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return ""
}

// llmToolDefinitions returns the set of tool names defined on an LLM span.
//
// Reads:
//   - `gen_ai.tool.definitions` (OTel semconv, list of {name, ...} dicts)
//   - `neatlogs.llm.tools` (JSON string, list of {function: {name, ...}})
func llmToolDefinitions(s Span) map[string]struct{} {
	out := map[string]struct{}{}
	attrs := Attributes(s)
	if td, ok := attrs["gen_ai.tool.definitions"].([]any); ok {
		for _, t := range td {
			if t, ok := t.(map[string]any); ok {
				if name, ok := t["name"].(string); ok {
					out[name] = struct{}{}
				}
			}
		}
	}
	if tools, ok := attrs["neatlogs.llm.tools"].(string); ok && tools != "" {
		// Best-effort JSON parse; malformed JSON → ignore that span's tools.
		var parsed []any
		if err := json.Unmarshal([]byte(tools), &parsed); err == nil {
			for _, t := range parsed {
				if t, ok := t.(map[string]any); ok {
					if fn, ok := t["function"].(map[string]any); ok {
						if name, ok := fn["name"].(string); ok {
							out[name] = struct{}{}
							continue
						}
					}
					if name, ok := t["name"].(string); ok {
						out[name] = struct{}{}
					}
				}
			}
		}
	}
	return out
}

// llmToolCalls returns the set of tool names called in this span
// (assistant message). Reads:
//   - OTel: `gen_ai.output.messages` with `tool_calls[*].function.name`
//   - neatlogs: each `neatlogs.llm.tool_calls.*` is a JSON string of
//     `{function: {name, ...}}`. Parsed with best-effort.
func llmToolCalls(s Span) map[string]struct{} {
	out := map[string]struct{}{}
	attrs := Attributes(s)
	if msgs, ok := attrs["gen_ai.output.messages"].([]any); ok {
		for _, m := range msgs {
			if m, ok := m.(map[string]any); ok {
				if tcs, ok := m["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						if tc, ok := tc.(map[string]any); ok {
							if fn, ok := tc["function"].(map[string]any); ok {
								if name, ok := fn["name"].(string); ok {
									out[name] = struct{}{}
								}
							}
						}
					}
				}
			}
		}
	}
	for k, v := range attrs {
		if !strings.HasPrefix(k, "neatlogs.llm.tool_calls.") {
			continue
		}
		str, ok := v.(string)
		if !ok {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(str), &parsed); err != nil {
			continue
		}
		if fn, ok := parsed["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				out[name] = struct{}{}
			}
		}
	}
	return out
}

// tokenWasteFindings detects token-waste patterns in LLM spans. Three
// findings:
//   - `oversized-prompt` (warning): prompt > OversizedPromptCharThreshold
//   - `repeated-system-prompt` (info, PII-gated): same sys prompt
//     RepeatedSystemPromptThreshold+ times
//   - `unused-tool-definition` (info): a defined tool is never called
//
// Internal spans are excluded.
func tokenWasteFindings(visible []Span, traceID, runID string, readPromptContent bool) []DoctorFinding {
	var findings []DoctorFinding
	var oversized []string
	systemPromptCounts := map[string]int{}
	allDefined := map[string]struct{}{}
	allCalled := map[string]struct{}{}

	for _, span := range visible {
		if IsInternal(span) {
			continue
		}
		if !isLLMKind(span) {
			continue
		}
		name := SpanName(span)
		// Oversized — always runs, no PII.
		size := llmPromptSize(span)
		if size > OversizedPromptCharThreshold {
			oversized = append(oversized, name+" ("+intToString(int64(size))+" chars)")
		}
		// Repeated system-prompt — only with opt-in (PII).
		if readPromptContent {
			if sys := llmSystemPrompt(span); sys != "" {
				systemPromptCounts[sys]++
			}
		}
		// Tool definitions vs. calls — no PII (just tool names).
		for t := range llmToolDefinitions(span) {
			allDefined[t] = struct{}{}
		}
		for t := range llmToolCalls(span) {
			allCalled[t] = struct{}{}
		}
	}

	if len(oversized) > 0 {
		shown := oversized
		if len(shown) > 3 {
			shown = shown[:3]
		}
		ev := intToString(int64(len(oversized))) +
			" LLM span(s) exceed " + intToString(int64(OversizedPromptCharThreshold)) +
			" chars in prompt: " + strings.Join(mapTruncate(shown), ", ")
		if len(oversized) > 3 {
			ev += " ..."
		}
		findings = append(findings, DoctorFinding{
			Severity:   "warning",
			Code:       "oversized-prompt",
			Title:      "LLM span(s) have oversized prompt content",
			Evidence:   Truncate(ev, 400),
			Suggestion: "Almost certainly a bug: usually a leaked retrieved document, CSV, or log dump. Cap the prompt with the wrapper's max_input_chars or truncate the source data before it reaches the LLM.",
			TraceID:    traceID,
			RunID:      runID,
			FixClass:   "config",
		})
	}

	if readPromptContent {
		var repeated []struct {
			text string
			n    int
		}
		for text, n := range systemPromptCounts {
			if n >= RepeatedSystemPromptThreshold {
				repeated = append(repeated, struct {
					text string
					n    int
				}{text, n})
			}
		}
		if len(repeated) > 0 {
			sort.Slice(repeated, func(i, j int) bool {
				return repeated[i].n > repeated[j].n
			})
			top := repeated[0]
			findings = append(findings, DoctorFinding{
				Severity:   "info",
				Code:       "repeated-system-prompt",
				Title:      "Same system prompt content sent many times — consider prompt caching",
				Evidence:   Truncate(intToString(int64(len(repeated)))+" distinct system prompt(s) repeated >= "+intToString(int64(RepeatedSystemPromptThreshold))+" times. Top repeat: "+intToString(int64(top.n))+" times ("+intToString(int64(len(top.text)))+" chars each).", 400),
				Suggestion: "If the system prompt is static, enable your provider's prompt caching (OpenAI cached_prompt_tokens, Anthropic cache_control, Gemini cachedContent). Repeated prefixes over ~1k tokens are usually free or heavily discounted at the provider.",
				TraceID:    traceID,
				RunID:      runID,
				FixClass:   "config",
			})
		}
	}

	// Unused = defined - called.
	var unused []string
	for t := range allDefined {
		if _, called := allCalled[t]; !called {
			unused = append(unused, t)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		shown := unused
		if len(shown) > 3 {
			shown = shown[:3]
		}
		ev := intToString(int64(len(unused))) + " tool(s) defined but not called: " + strings.Join(shown, ", ")
		if len(unused) > 3 {
			ev += " ..."
		}
		findings = append(findings, DoctorFinding{
			Severity:   "info",
			Code:       "unused-tool-definition",
			Title:      "Tool(s) defined in prompt but never called",
			Evidence:   Truncate(ev, 400),
			Suggestion: "Either the model chose not to call them (drop them from the prompt to save tokens) or the wrapper is silently dropping tool calls (check the wrapper's tool-call routing).",
			TraceID:    traceID,
			RunID:      runID,
			FixClass:   "config",
			RelatedCodes: []string{"missing-span-kind"},
		})
	}

	return findings
}

// mapTruncate is a small helper: applies truncate() to each string and
// returns the resulting slice.
func mapTruncate(strs []string) []string {
	out := make([]string, len(strs))
	for i, s := range strs {
		out[i] = truncate(s)
	}
	return out
}
