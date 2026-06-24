package attributes

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
)

// mappingJSON is the canonical attribute-mapping config, shared verbatim with
// the Python and TypeScript SDKs (neatlogs/config/attribute-mapping.json). It is
// the single source of truth for how vendor/semconv attributes across every span
// kind (llm, tool, agent, retriever, embedding, reranker, guardrail, mcp,
// vector_store, ...) translate into the neatlogs.* namespace.
//
//go:embed attribute-mapping.json
var mappingJSON []byte

// Mapper translates vendor-specific span attributes into the unified neatlogs.*
// namespace by interpreting the embedded mapping config. It is a faithful port
// of the Python AttributeMapper (neatlogs/config/attribute_mapper.py) and TS
// AttributeMapper, so all three SDKs share one contract.
//
// Deliberately, the Mapper performs NO value rewriting (e.g. it does not coerce
// gen_ai.system "gcp.vertex_ai" into "vertexai") and NO derived computation
// (e.g. it does not synthesize total tokens). It only renames keys per the
// config; the backend owns any value normalization.
type Mapper struct {
	mappings      map[string]any
	keepAsIs      map[string]struct{}
	ignore        []*regexp.Regexp
	mappedSources map[string]struct{} // every source key referenced anywhere in the config

	reMu  sync.Mutex
	reCar map[string]*regexp.Regexp // cache: source pattern -> anchored regexp
}

// span_kind value vocabulary, populated from the config at load time.
var spanKindValues map[string]string

var (
	defaultMapper *Mapper
	defaultOnce   sync.Once
)

// Default returns the process-wide Mapper built from the embedded config.
func Default() *Mapper {
	defaultOnce.Do(func() {
		m, err := newMapper(mappingJSON)
		if err != nil {
			// The config is embedded and tested; a parse failure is a build-time
			// bug. Fall back to an empty mapper so the SDK degrades to passthrough
			// rather than panicking in a user's process.
			m = &Mapper{
				mappings:      map[string]any{},
				keepAsIs:      map[string]struct{}{},
				mappedSources: map[string]struct{}{},
				reCar:         map[string]*regexp.Regexp{},
			}
		}
		defaultMapper = m
	})
	return defaultMapper
}

func newMapper(raw []byte) (*Mapper, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	mappings, _ := doc["mappings"].(map[string]any)
	if mappings == nil {
		mappings = map[string]any{}
	}

	m := &Mapper{
		mappings:      mappings,
		keepAsIs:      map[string]struct{}{},
		mappedSources: map[string]struct{}{},
		reCar:         map[string]*regexp.Regexp{},
	}

	// keep_as_is.attributes
	if kept, ok := mappings["keep_as_is"].(map[string]any); ok {
		for _, a := range toStrings(kept["attributes"]) {
			m.keepAsIs[a] = struct{}{}
		}
	}
	// ignore.patterns -> anchored regexps ("*" => ".*")
	if ig, ok := mappings["ignore"].(map[string]any); ok {
		for _, p := range toStrings(ig["patterns"]) {
			rx, err := regexp.Compile("^" + strings.ReplaceAll(regexp.QuoteMeta(p), `\*`, `.*`) + "$")
			if err == nil {
				m.ignore = append(m.ignore, rx)
			}
		}
	}

	// span_kind values vocabulary (shared package-level cache).
	if sk, ok := mappings["span_kind"].(map[string]any); ok {
		if vals, ok := sk["values"].(map[string]any); ok {
			spanKindValues = map[string]string{}
			for k, v := range vals {
				if s, ok := v.(string); ok {
					spanKindValues[k] = s
				}
			}
		}
	}

	m.collectSources(mappings)
	return m, nil
}

// Normalize maps a span's attributes into the neatlogs.* namespace and returns
// the result as a new slice. It is safe to call on attributes that are already
// neatlogs.* (they pass through unless an explicit mapping or keep/ignore rule
// applies).
func (m *Mapper) Normalize(in []attribute.KeyValue) []attribute.KeyValue {
	attrs := make(map[string]attribute.Value, len(in))
	for _, kv := range in {
		attrs[string(kv.Key)] = kv.Value
	}

	out := map[string]attribute.Value{}
	kind := m.mapSpanKind(attrs)
	out[SpanKind] = attribute.StringValue(kind)

	// Walk every top-level section except the meta sections.
	for name, sec := range m.mappings {
		if name == "span_kind" || name == "keep_as_is" || name == "ignore" {
			continue
		}
		secMap, ok := sec.(map[string]any)
		if !ok {
			continue
		}
		if inner, ok := secMap["mappings"].(map[string]any); ok {
			m.mapNested(inner, attrs, kind, out)
			// Process sibling sub-sections alongside the explicit "mappings".
			for subKey, subVal := range secMap {
				if subKey == "mappings" || subKey == "description" {
					continue
				}
				if sm, ok := subVal.(map[string]any); ok {
					m.mapNode(sm, attrs, kind, out)
				}
			}
		} else if _, ok := secMap["sources"]; ok {
			m.mapLeaf(secMap, attrs, kind, out)
		} else {
			m.mapNested(secMap, attrs, kind, out)
		}
	}

	// Google ADK records tool args/result under its proprietary
	// gcp.vertex.agent.* keys, which the shared attribute-mapping.json does not
	// cover. Map them to the neatlogs.tool.* keys the backend reads for the tool
	// span's input/output, so ADK execute_tool spans show their I/O.
	mapFirst(attrs, out, ToolInput, "gcp.vertex.agent.tool_call_args")
	mapFirst(attrs, out, ToolOutput, "gcp.vertex.agent.tool_response")

	// keep_as_is: copy through OTel-standard keys not already mapped.
	for name, val := range attrs {
		if _, kept := m.keepAsIs[name]; kept {
			if _, exists := out[name]; !exists {
				out[name] = val
			}
		}
	}

	// Unmapped passthrough: any attribute not referenced as a source, not
	// already mapped, and not ignored is carried over verbatim.
	for name, val := range attrs {
		if _, isSrc := m.mappedSources[name]; isSrc {
			continue
		}
		if _, exists := out[name]; exists {
			continue
		}
		if m.shouldIgnore(name) {
			continue
		}
		out[name] = val
	}

	// Drop any ignored keys that slipped in.
	result := make([]attribute.KeyValue, 0, len(out))
	for k, v := range out {
		if m.shouldIgnore(k) {
			continue
		}
		result = append(result, attribute.KeyValue{Key: attribute.Key(k), Value: v})
	}
	return result
}

// mapFirst sets out[target] to the first present source value, unless target is
// already populated. Used for ADK-specific keys outside the shared JSON config.
func mapFirst(attrs, out map[string]attribute.Value, target string, sources ...string) {
	if _, exists := out[target]; exists {
		return
	}
	for _, s := range sources {
		if v, ok := attrs[s]; ok {
			out[target] = v
			return
		}
	}
}

// mapNested recurses through a config subtree, dispatching leaves and branches.
func (m *Mapper) mapNested(cfg map[string]any, attrs map[string]attribute.Value, kind string, out map[string]attribute.Value) {
	for key, val := range cfg {
		switch key {
		case "description", "sources", "target", "indexed", "priority", "values", "applies_to", "target_content":
			continue
		}
		node, ok := val.(map[string]any)
		if !ok {
			continue
		}
		m.mapNode(node, attrs, kind, out)
	}
}

// mapNode handles one config node: a leaf (has "sources") or a nested branch.
func (m *Mapper) mapNode(node map[string]any, attrs map[string]attribute.Value, kind string, out map[string]attribute.Value) {
	if applies, ok := node["applies_to"]; ok {
		if !contains(toStrings(applies), kind) {
			return
		}
	}
	if _, ok := node["sources"]; ok {
		m.mapLeaf(node, attrs, kind, out)
		return
	}
	m.mapNested(node, attrs, kind, out)
}

// mapLeaf applies a single source->target mapping (simple or indexed).
func (m *Mapper) mapLeaf(node map[string]any, attrs map[string]attribute.Value, kind string, out map[string]attribute.Value) {
	indexed, _ := node["indexed"].(bool)
	target, _ := node["target"].(string)

	if indexed {
		m.mapIndexed(node, attrs, target, out)
		if tc, ok := node["target_content"].(string); ok {
			m.mapIndexed(node, attrs, tc, out)
		}
		return
	}

	target = strings.ReplaceAll(target, "{span_kind}", kind)
	if v, ok := m.firstSource(node, attrs); ok {
		out[target] = v
	}
}

// mapIndexed expands indexed source patterns (".{i}.") into concrete keys. It
// supports both source shapes used in the config:
//   - array:  ["llm.input_messages.{i}.message.role", ...] -> target{i}
//   - object: {"id": ["llm.tool_calls.{i}.id"], ...}       -> target{i}.id
func (m *Mapper) mapIndexed(node map[string]any, attrs map[string]attribute.Value, targetBase string, out map[string]attribute.Value) {
	raw := node["sources"]
	switch src := raw.(type) {
	case []any:
		for _, p := range toStrings(src) {
			rx := m.patternRegexp(p)
			for name, val := range attrs {
				if mm := rx.FindStringSubmatch(name); mm != nil {
					out[strings.ReplaceAll(targetBase, "{i}", mm[1])] = val
				}
			}
		}
	case map[string]any:
		for field, patterns := range src {
			for _, p := range toStrings(patterns) {
				rx := m.patternRegexp(p)
				for name, val := range attrs {
					if mm := rx.FindStringSubmatch(name); mm != nil {
						out[strings.ReplaceAll(targetBase, "{i}", mm[1])+"."+field] = val
					}
				}
			}
		}
	}
}

// firstSource returns the value of the first present source key for a leaf.
func (m *Mapper) firstSource(node map[string]any, attrs map[string]attribute.Value) (attribute.Value, bool) {
	for _, s := range flattenSources(node["sources"]) {
		if v, ok := attrs[s]; ok {
			return v, true
		}
	}
	return attribute.Value{}, false
}

// mapSpanKind extracts and normalizes the span kind, mirroring the Python/TS
// logic: honor explicit openinference/neatlogs kinds, else infer LLM, else
// "unknown".
func (m *Mapper) mapSpanKind(attrs map[string]attribute.Value) string {
	var raw string
	if v, ok := attrs["openinference.span.kind"]; ok {
		raw = v.AsString()
	}
	if raw == "" {
		if v, ok := attrs["traceloop.span.kind"]; ok {
			raw = v.AsString()
		}
	}
	if raw == "" {
		if v, ok := attrs[SpanKind]; ok { // a wrapper may set neatlogs.span.kind directly
			raw = v.AsString()
		}
	}
	// Preserve a kind a wrapper/processor set deliberately that is not part of
	// the normalized vocabulary (e.g. the "Neatlogs.INTERNAL" completion marker).
	// Without this it would be clobbered to "unknown".
	if raw == "Neatlogs.INTERNAL" {
		return raw
	}
	if raw != "" {
		if mapped, ok := spanKindValues[raw]; ok {
			return mapped
		}
		// Already the normalized lowercase form?
		for _, val := range spanKindValues {
			if val == raw {
				return raw
			}
		}
	}

	// Classify from the OpenTelemetry GenAI operation name, which is what
	// OTel-native frameworks (notably Google ADK) set instead of an explicit
	// span-kind attribute. Without this, ADK's invoke_agent/generate_content/
	// execute_tool spans fall through to "unknown" — and an unknown-kind root
	// is dropped by the backend trace-finalizer (it renders only
	// WORKFLOW/CHAIN/AGENT/MCP_TOOL roots), so the whole ADK trace disappears.
	if v, ok := attrs[GenAIOperationName]; ok {
		if kind := operationNameToKind(v.AsString()); kind != "" {
			return kind
		}
	}

	for _, k := range []string{
		"llm.model_name", "gen_ai.request.model",
		"llm.token_count.prompt", "llm.token_count.completion",
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens",
		LLMModelName, LLMTokenPrompt, LLMTokenCompletion,
	} {
		if _, ok := attrs[k]; ok {
			return KindLLM
		}
	}
	return "unknown"
}

// operationNameToKind maps an OTel GenAI gen_ai.operation.name onto a neatlogs
// span kind. Returns "" when the operation is unrecognized so the caller can
// fall back to attribute-based inference.
func operationNameToKind(op string) string {
	switch op {
	case "invoke_agent", "create_agent":
		return KindAgent
	case "execute_tool":
		return KindTool
	case "embeddings":
		return KindEmbedding
	case "generate_content", "chat", "text_completion":
		return KindLLM
	default:
		return ""
	}
}

func (m *Mapper) shouldIgnore(name string) bool {
	for _, rx := range m.ignore {
		if rx.MatchString(name) {
			return true
		}
	}
	return false
}

// collectSources records every source key referenced anywhere in the config so
// unmapped passthrough can skip keys that were intentionally consumed.
func (m *Mapper) collectSources(node map[string]any) {
	for key, val := range node {
		if key == "sources" {
			for _, s := range flattenSources(val) {
				m.mappedSources[s] = struct{}{}
			}
			continue
		}
		if child, ok := val.(map[string]any); ok {
			m.collectSources(child)
		}
	}
}

// patternRegexp compiles (and caches) an anchored regexp for an indexed source
// pattern, turning "{i}" into a capture group.
func (m *Mapper) patternRegexp(pattern string) *regexp.Regexp {
	m.reMu.Lock()
	defer m.reMu.Unlock()
	if rx, ok := m.reCar[pattern]; ok {
		return rx
	}
	escaped := regexp.QuoteMeta(pattern)
	escaped = strings.ReplaceAll(escaped, `\{i\}`, `(\d+)`)
	rx := regexp.MustCompile("^" + escaped + "$")
	m.reCar[pattern] = rx
	return rx
}

// ── small helpers ────────────────────────────────────────────────────────

func flattenSources(v any) []string {
	switch s := v.(type) {
	case []any:
		return toStrings(s)
	case map[string]any:
		var out []string
		for _, arr := range s {
			out = append(out, toStrings(arr)...)
		}
		return out
	default:
		return nil
	}
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
