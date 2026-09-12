package doctor

import "strings"

// hasInput returns true if a span of the given kind has a meaningful
// input attribute. For LLM spans, role alone is metadata; the doctor
// requires at least one non-empty content attribute (Bug #1 fix from the
// Python reference).
func hasInput(kind string, attrs map[string]any) bool {
	if attrs == nil {
		return false
	}
	switch kind {
	case KindLLM:
		return llmHasMeaningfulInput(attrs)
	case KindEmbedding:
		if v, ok := attrs["neatlogs.embedding.text"]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return true
			}
		}
		return false
	case KindTool:
		for _, key := range []string{"neatlogs.tool.input", "neatlogs.tool.parameters"} {
			if v, ok := attrs[key]; ok && isMeaningfulNonEmpty(v) {
				return true
			}
		}
		return false
	case KindRetriever:
		for _, key := range []string{"neatlogs.retriever.input", "neatlogs.retriever.query"} {
			if v, ok := attrs[key]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
			}
		}
		return false
	}
	return false
}

// hasOutput returns true if a span of the given kind has a meaningful
// output attribute.
func hasOutput(kind string, attrs map[string]any) bool {
	if attrs == nil {
		return false
	}
	switch kind {
	case KindLLM:
		// An output_messages.N.content of any non-empty string counts.
		for key, value := range attrs {
			if strings.HasPrefix(key, "neatlogs.llm.output_messages.") && strings.HasSuffix(key, ".content") {
				if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
				if isMeaningfulNonEmpty(value) {
					return true
				}
			}
		}
		if v, ok := attrs["neatlogs.llm.output"]; ok && isMeaningfulNonEmpty(v) {
			return true
		}
		return false
	case KindTool:
		if v, ok := attrs["neatlogs.tool.output"]; ok && isMeaningfulNonEmpty(v) {
			return true
		}
		return false
	case KindRetriever:
		if v, ok := attrs["neatlogs.retriever.output"]; ok && isMeaningfulNonEmpty(v) {
			return true
		}
		for key, value := range attrs {
			if strings.HasPrefix(key, "neatlogs.retriever.documents.") && isMeaningfulNonEmpty(value) {
				return true
			}
		}
		return false
	case KindEmbedding:
		if v, ok := attrs["neatlogs.embedding.dimensions"]; ok && v != nil {
			return true
		}
		if v, ok := attrs["neatlogs.embedding.count"]; ok && v != nil {
			return true
		}
		return false
	}
	return false
}

// llmHasMeaningfulInput is the LLM-specific input check. At least one
// neatlogs.llm.input_messages.N.content with a non-empty string, OR a
// non-empty neatlogs.llm.system_prompt / neatlogs.llm.input. Role alone
// does not count.
func llmHasMeaningfulInput(attrs map[string]any) bool {
	for key, value := range attrs {
		if strings.HasPrefix(key, "neatlogs.llm.input_messages.") && strings.HasSuffix(key, ".content") {
			if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
				return true
			}
			if isStructuredContent(value) {
				return true
			}
		}
	}
	for _, key := range []string{"neatlogs.llm.input", "neatlogs.llm.system_prompt"} {
		if v, ok := attrs[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return true
			}
			if isStructuredContent(v) {
				return true
			}
		}
	}
	return false
}

// isMeaningfulNonEmpty returns true if v is non-nil, non-empty string,
// non-empty slice/map, and not false.
func isMeaningfulNonEmpty(v any) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok && !b {
		return false
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case float64:
		return x != 0
	}
	return true
}

// isStructuredContent returns true for non-empty list/dict values
// (used to allow tool-call JSON, embedding lists, etc. as input/output).
func isStructuredContent(v any) bool {
	if _, ok := v.(bool); ok {
		// booleans are never considered "structured content" here.
		return false
	}
	switch x := v.(type) {
	case string:
		return false
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return v != nil
}

// hasLLMDescendant returns true if any descendant of spanID has kind
// "llm". Iterative DFS with a visited set to handle cross-links safely.
func hasLLMDescendant(spanID string, childMap map[string][]Span, visited map[string]bool) bool {
	if visited == nil {
		visited = map[string]bool{}
	}
	stack := []string{spanID}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		for _, child := range childMap[cur] {
			if SpanKind(child) == KindLLM {
				return true
			}
			if cid := SpanID(child); cid != "" {
				stack = append(stack, cid)
			}
		}
	}
	return false
}
