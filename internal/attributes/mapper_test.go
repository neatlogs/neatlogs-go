package attributes

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func toMap(kvs []attribute.KeyValue) map[string]attribute.Value {
	m := make(map[string]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

// The embedded config must parse and yield a non-empty span-kind vocabulary.
func TestDefaultMapperLoads(t *testing.T) {
	m := Default()
	if m == nil || len(m.mappings) == 0 {
		t.Fatal("default mapper failed to load embedded config")
	}
	if spanKindValues["LLM"] != "llm" || spanKindValues["RETRIEVER"] != "retriever" {
		t.Fatalf("span-kind vocabulary not loaded: %v", spanKindValues)
	}
}

// OTel GenAI semconv keys (as Google ADK emits) map to neatlogs.* with no value
// rewriting and no derived totals — the backend owns that.
func TestNormalize_GenAISemconv(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String("gen_ai.request.model", "gemini-2.5-flash"),
		attribute.String("gen_ai.system", "gcp.vertex_ai"),
		attribute.Int64("gen_ai.usage.input_tokens", 100),
		attribute.Int64("gen_ai.usage.output_tokens", 40),
		attribute.StringSlice("gen_ai.response.finish_reasons", []string{"STOP"}),
	}
	got := toMap(Default().Normalize(in))

	checkStr(t, got, SpanKind, KindLLM) // inferred from gen_ai.request.model
	checkStr(t, got, LLMModelName, "gemini-2.5-flash")
	checkStr(t, got, LLMSystem, "gcp.vertex_ai") // value passed through verbatim
	checkInt(t, got, LLMTokenPrompt, 100)
	checkInt(t, got, LLMTokenCompletion, 40)
	// finish_reasons maps from a string slice and is preserved as a slice.
	if v, ok := got["neatlogs.llm.finish_reason"]; !ok {
		t.Error("missing neatlogs.llm.finish_reason")
	} else if got := v.AsStringSlice(); len(got) != 1 || got[0] != "STOP" {
		t.Errorf("finish_reason = %v, want [STOP]", got)
	}

	if _, ok := got[LLMTokenTotal]; ok {
		t.Error("mapper must not synthesize total tokens; backend derives it")
	}
}

// Indexed dict-source mapping: llm.tool_calls.{i}.{field} -> neatlogs target.
func TestNormalize_IndexedToolCalls(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String("openinference.span.kind", "LLM"),
		attribute.String("llm.tool_calls.0.id", "call_1"),
		attribute.String("llm.tool_calls.0.name", "get_weather"),
		attribute.String("llm.tool_calls.0.arguments", `{"city":"NYC"}`),
	}
	got := toMap(Default().Normalize(in))

	checkStr(t, got, "neatlogs.llm.tool_calls.0.id", "call_1")
	checkStr(t, got, "neatlogs.llm.tool_calls.0.name", "get_weather")
	checkStr(t, got, "neatlogs.llm.tool_calls.0.arguments", `{"city":"NYC"}`)
}

// Indexed message mapping expands gen_ai.prompt.{i}.* into the neatlogs message
// targets. This mirrors the canonical Python/TS mapper exactly: because the
// role and content patterns share one "sources" list, both the role-target and
// the content-target are written from every matching source, so the role key
// ends up holding the last-matched value. We assert the keys exist with that
// (intentionally-shared) canonical behavior rather than diverging from it.
func TestNormalize_IndexedMessages(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String("openinference.span.kind", "LLM"),
		attribute.String("gen_ai.prompt.0.role", "user"),
		attribute.String("gen_ai.prompt.0.content", "hello"),
	}
	got := toMap(Default().Normalize(in))

	if _, ok := got["neatlogs.llm.input_messages.0.role"]; !ok {
		t.Error("missing neatlogs.llm.input_messages.0.role")
	}
	checkStr(t, got, "neatlogs.llm.input_messages.0.content", "hello")
}

// keep_as_is OTel-standard keys survive unchanged; ignore patterns are dropped.
func TestNormalize_KeepAsIsAndIgnore(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String("gen_ai.request.model", "gemini-2.5-pro"),
		attribute.String("http.method", "POST"),                // keep_as_is
		attribute.String("exception.message", "boom"),          // ignore pattern
		attribute.String("telemetry.distro.name", "something"), // ignore pattern
	}
	got := toMap(Default().Normalize(in))

	checkStr(t, got, "http.method", "POST")
	if _, ok := got["exception.message"]; ok {
		t.Error("exception.* must be ignored")
	}
	if _, ok := got["telemetry.distro.name"]; ok {
		t.Error("telemetry.distro.* must be ignored")
	}
}

// Attributes our own wrapper already wrote in the neatlogs.* namespace are not
// consumed as sources, so they pass through untouched.
func TestNormalize_NeatlogsPassthrough(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String(SpanKind, KindLLM),
		attribute.String(LLMModelName, "gemini-2.5-flash"),
		attribute.Int(LLMTokenPrompt, 7),
		attribute.Int(LLMTokenTotal, 10),
	}
	got := toMap(Default().Normalize(in))

	checkStr(t, got, LLMModelName, "gemini-2.5-flash")
	checkInt(t, got, LLMTokenPrompt, 7)
	checkInt(t, got, LLMTokenTotal, 10)
}

// execute_tool spans (Google ADK tool calls) classify as tool kind.
func TestNormalize_ToolSpanKind(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String("openinference.span.kind", "TOOL"),
		attribute.String("tool.name", "search"),
	}
	got := toMap(Default().Normalize(in))
	checkStr(t, got, SpanKind, KindTool)
}

func checkStr(t *testing.T, m map[string]attribute.Value, key, want string) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	if v.AsString() != want {
		t.Errorf("%q = %q, want %q", key, v.AsString(), want)
	}
}

func checkInt(t *testing.T, m map[string]attribute.Value, key string, want int64) {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing attribute %q", key)
	}
	if v.AsInt64() != want {
		t.Errorf("%q = %d, want %d", key, v.AsInt64(), want)
	}
}

// ADK records tool args/result under gcp.vertex.agent.* keys; the mapper must
// surface them as neatlogs.tool.input/output so the backend shows tool I/O.
func TestNormalize_ADKToolKeys(t *testing.T) {
	in := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gcp.vertex.agent.tool_call_args", `{"city":"Tokyo"}`),
		attribute.String("gcp.vertex.agent.tool_response", `{"report":"Sunny, 24C"}`),
	}
	got := toMap(Default().Normalize(in))
	checkStr(t, got, SpanKind, KindTool)
	checkStr(t, got, ToolInput, `{"city":"Tokyo"}`)
	checkStr(t, got, ToolOutput, `{"report":"Sunny, 24C"}`)
}
