// Package attributes defines the canonical neatlogs.* span attribute keys the
// SDK's own wrappers emit, plus a JSON-driven Mapper (see mapper.go) that
// normalizes attributes from any source — OpenTelemetry GenAI semconv (as
// emitted by Google ADK and others), OpenInference, Traceloop — into the same
// neatlogs.* namespace.
//
// The canonical contract for all span kinds lives in attribute-mapping.json,
// shared verbatim with the Python and TypeScript SDKs. The constants here are
// only the subset the Go wrappers write directly; they intentionally match
// target keys in that JSON.
package attributes

// Span classification.
const (
	SpanKind = "neatlogs.span.kind" // value: "llm", "tool", "agent", "embedding", ...
	Internal = "neatlogs.internal"  // bool: framework-internal span, not user-facing
)

// GenAIOperationName is the OpenTelemetry GenAI semantic-convention attribute
// that OTel-native frameworks (e.g. Google ADK) set to name the operation
// (invoke_agent / generate_content / execute_tool / ...). The mapper reads it to
// classify span kind when no explicit span-kind attribute is present.
const GenAIOperationName = "gen_ai.operation.name"

// LLM attributes emitted by the genai wrapper.
const (
	LLMProvider  = "neatlogs.llm.provider"
	LLMSystem    = "neatlogs.llm.system"
	LLMModelName = "neatlogs.llm.model_name"
	LLMStreaming = "neatlogs.llm.is_streaming"

	LLMTokenPrompt     = "neatlogs.llm.token_count.prompt"
	LLMTokenCompletion = "neatlogs.llm.token_count.completion"
	LLMTokenTotal      = "neatlogs.llm.token_count.total"
	LLMTokenReasoning  = "neatlogs.llm.token_count.reasoning"
	LLMTokenCacheRead  = "neatlogs.llm.token_count.cache_read"

	LLMTemperature          = "neatlogs.llm.temperature"
	LLMTopP                 = "neatlogs.llm.top_p"
	LLMTopK                 = "neatlogs.llm.top_k"
	LLMMaxTokens            = "neatlogs.llm.max_tokens"
	LLMFrequencyPenalty     = "neatlogs.llm.frequency_penalty"
	LLMPresencePenalty      = "neatlogs.llm.presence_penalty"
	LLMInvocationParameters = "neatlogs.llm.invocation_parameters"

	LLMFinishReason = "neatlogs.llm.finish_reason"
	LLMResponseID   = "neatlogs.llm.response_id"
)

// Embedding attributes emitted by the genai wrapper.
const (
	EmbeddingModelName  = "neatlogs.embedding.model_name"
	EmbeddingText       = "neatlogs.embedding.text"
	EmbeddingCount      = "neatlogs.embedding.count"
	EmbeddingDimensions = "neatlogs.embedding.dimensions"
)

// Tool attributes.
const (
	ToolName   = "neatlogs.tool.name"
	ToolInput  = "neatlogs.tool.input"
	ToolOutput = "neatlogs.tool.output"
)

// Identity / session resource attributes set by Init.
const (
	WorkflowName = "neatlogs.workflow_name"
	SessionID    = "neatlogs.session.id"
	Tags         = "neatlogs.tags"
)

// Indexed-attribute prefixes. Callers expand with an integer index, e.g.
// fmt.Sprintf("%s%d.role", LLMInputMessagePrefix, i).
const (
	LLMInputMessagePrefix  = "neatlogs.llm.input_messages."
	LLMOutputMessagePrefix = "neatlogs.llm.output_messages."
	LLMToolCallPrefix      = "neatlogs.llm.tool_calls."
	LLMToolPrefix          = "neatlogs.llm.tools."
)

// Span-kind values (lowercase normalized forms; full vocabulary in the JSON).
const (
	KindLLM       = "llm"
	KindTool      = "tool"
	KindEmbedding = "embedding"
	KindWorkflow  = "workflow"
	KindAgent     = "agent"
)
