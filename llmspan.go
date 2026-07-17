package neatlogs

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// LLMMessage is one request or response message on an LLM span.
type LLMMessage struct {
	Role    string
	Content string
}

// LLMCallOptions describes a provider LLM call for StartLLMSpan. Only Provider
// and Model are typically required; the rest are recorded when set.
type LLMCallOptions struct {
	// Provider is the neatlogs provider id, e.g. "openai" or "anthropic".
	Provider string
	// System is the provider system label; defaults to Provider when empty.
	System string
	// Model is the model name, e.g. "gpt-5.5" or "claude-sonnet-4-6".
	Model     string
	Streaming bool

	MaxTokens   int
	Temperature *float64
	TopP        *float64

	// Messages are the input messages (including any system message).
	Messages []LLMMessage
	// Name overrides the span name; defaults to "{provider}.chat".
	Name string
}

// LLMSpan is a handle to an LLM span opened by StartLLMSpan. Record the result
// with SetOutputMessage/SetUsage/SetFinishReason (or SetError), then call End
// exactly once (via defer).
//
// It is the Go analogue of the Python/TypeScript SDKs' LLM span: it captures
// provider/model, input and output messages, invocation parameters, token
// usage and finish reason under the neatlogs.* namespace. Use it to instrument
// direct provider SDK calls (OpenAI, Anthropic, ...) that WrapGenAI does not
// cover. The caller imports only this package — no OpenTelemetry types.
type LLMSpan struct {
	span trace.Span
	end  func()
}

// StartLLMSpan opens an LLM span for a provider call, auto-rooting under a
// WORKFLOW span when the context has no active parent (so a standalone call
// still yields a renderable trace) and nesting under an existing parent
// otherwise (e.g. a TOOL span continued from an upstream service). It uses the
// private Neatlogs provider only; the process-global OTel / Datadog context is
// never read or mutated. When Neatlogs is not initialized the span is a no-op.
//
// Call End on the returned LLMSpan exactly once, usually via defer.
func StartLLMSpan(ctx context.Context, opts LLMCallOptions) (context.Context, *LLMSpan) {
	provider := opts.Provider
	system := opts.System
	if system == "" {
		system = provider
	}
	name := opts.Name
	if name == "" {
		name = provider + ".chat"
	}

	ctx, span, end := StartProviderSpan(ctx, name, attrs.KindLLM)
	span.SetAttributes(
		attribute.String(attrs.SpanKind, attrs.KindLLM),
		attribute.String(attrs.LLMProvider, provider),
		attribute.String(attrs.LLMSystem, system),
		attribute.String(attrs.LLMModelName, opts.Model),
		attribute.Bool(attrs.LLMStreaming, opts.Streaming),
	)
	setLLMInvocationParams(span, opts)
	setLLMInputMessages(span, opts.Messages)
	return ctx, &LLMSpan{span: span, end: end}
}

// SetOutputMessage records the assistant's response message (index 0).
func (s *LLMSpan) SetOutputMessage(role, content string) {
	if s == nil || content == "" {
		return
	}
	if role == "" {
		role = "assistant"
	}
	s.span.SetAttributes(
		attribute.String(attrs.LLMOutputMessagePrefix+"0.role", role),
		attribute.String(attrs.LLMOutputMessagePrefix+"0.content", content),
	)
}

// SetUsage records token usage. Pass 0 for any count that is unknown.
func (s *LLMSpan) SetUsage(promptTokens, completionTokens, totalTokens int) {
	if s == nil {
		return
	}
	if promptTokens != 0 {
		s.span.SetAttributes(attribute.Int(attrs.LLMTokenPrompt, promptTokens))
	}
	if completionTokens != 0 {
		s.span.SetAttributes(attribute.Int(attrs.LLMTokenCompletion, completionTokens))
	}
	if totalTokens != 0 {
		s.span.SetAttributes(attribute.Int(attrs.LLMTokenTotal, totalTokens))
	}
}

// SetModel overrides the model name recorded at start. Use it when the
// authoritative model is only known from the response (e.g. a provider that
// resolves an alias, or a fallback path that switches models). No-op for "".
func (s *LLMSpan) SetModel(model string) {
	if s == nil || model == "" {
		return
	}
	s.span.SetAttributes(attribute.String(attrs.LLMModelName, model))
}

// SetProvider overrides the provider/system recorded at start. Use it when the
// serving provider is only known after the call (e.g. a primary→fallback
// switch). No-op for "".
func (s *LLMSpan) SetProvider(provider string) {
	if s == nil || provider == "" {
		return
	}
	s.span.SetAttributes(
		attribute.String(attrs.LLMProvider, provider),
		attribute.String(attrs.LLMSystem, provider),
	)
}

// SetFinishReason records the provider finish/stop reason.
func (s *LLMSpan) SetFinishReason(reason string) {
	if s == nil || reason == "" {
		return
	}
	s.span.SetAttributes(attribute.String(attrs.LLMFinishReason, reason))
}

// SetResponseID records the provider response id.
func (s *LLMSpan) SetResponseID(id string) {
	if s == nil || id == "" {
		return
	}
	s.span.SetAttributes(attribute.String(attrs.LLMResponseID, id))
}

// SetError marks the span failed and records err.
func (s *LLMSpan) SetError(err error) {
	if s == nil || err == nil {
		return
	}
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
}

// End closes the span (and its auto-root, if one was opened). Call exactly once.
func (s *LLMSpan) End() {
	if s == nil {
		return
	}
	s.end()
}

func setLLMInvocationParams(span trace.Span, opts LLMCallOptions) {
	params := map[string]any{}
	if opts.MaxTokens != 0 {
		span.SetAttributes(attribute.Int(attrs.LLMMaxTokens, opts.MaxTokens))
		params["max_tokens"] = opts.MaxTokens
	}
	if opts.Temperature != nil {
		span.SetAttributes(attribute.Float64(attrs.LLMTemperature, *opts.Temperature))
		params["temperature"] = *opts.Temperature
	}
	if opts.TopP != nil {
		span.SetAttributes(attribute.Float64(attrs.LLMTopP, *opts.TopP))
		params["top_p"] = *opts.TopP
	}
	if len(params) > 0 {
		span.SetAttributes(attribute.String(attrs.LLMInvocationParameters, jsonString(params)))
	}
}

func setLLMInputMessages(span trace.Span, messages []LLMMessage) {
	for i, m := range messages {
		role := m.Role
		if role == "" {
			role = "user"
		}
		span.SetAttributes(
			attribute.String(fmt.Sprintf("%s%d.role", attrs.LLMInputMessagePrefix, i), role),
			attribute.String(fmt.Sprintf("%s%d.content", attrs.LLMInputMessagePrefix, i), m.Content),
		)
	}
}
