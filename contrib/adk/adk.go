// Package adk instruments Google ADK with Neatlogs-owned spans. It never reads
// or replaces the process-global OpenTelemetry provider.
package adk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"

	neatlogs "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

const (
	inputMsgPrefix  = "neatlogs.llm.input_messages."
	outputMsgPrefix = "neatlogs.llm.output_messages."
	toolCallPrefix  = "neatlogs.llm.tool_calls."
	maxOutputBytes  = 1 << 20
	maxToolCalls    = 128
)

// instrumentedModel wraps an ADK model.LLM, adding I/O capture onto the active
// span. It preserves the wrapped model's Name and streaming semantics.
type instrumentedModel struct {
	inner model.LLM
}

// WrapModel returns a model.LLM that records each ADK model call on the private
// Neatlogs provider selected by ctx. It preserves streaming and never changes
// the process-global OpenTelemetry provider. If inner is nil, it returns nil.
func WrapModel(inner model.LLM) model.LLM {
	if inner == nil {
		return inner
	}
	if _, ok := inner.(*instrumentedModel); ok {
		return inner
	}
	return &instrumentedModel{inner: inner}
}

func (m *instrumentedModel) Name() string { return m.inner.Name() }

func (m *instrumentedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		modelName := m.Name()
		if req != nil && strings.TrimSpace(req.Model) != "" {
			modelName = req.Model
		}
		spanCtx, span, end := neatlogs.StartProviderSpan(ctx, "google.adk.generate_content", "llm")
		defer end()
		span.SetAttributes(
			attribute.String("neatlogs.span.kind", "llm"),
			attribute.String("neatlogs.llm.provider", "google"),
			attribute.String("neatlogs.llm.system", "google_adk"),
			attribute.String("neatlogs.llm.model_name", modelName),
			attribute.Bool("neatlogs.llm.is_streaming", stream),
		)
		setInputMessages(span, req)
		setInvocationParameters(span, req)

		capture := responseCapture{}
		for resp, err := range m.inner.GenerateContent(spanCtx, req, stream) {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				capture.failed = true
			} else {
				capture.add(span, resp)
			}
			if !yield(resp, err) {
				span.SetAttributes(attribute.Bool("neatlogs.stream.cancelled", true))
				capture.cancelled = true
				break
			}
		}
		capture.apply(span)
		if !capture.failed && !capture.cancelled {
			span.SetStatus(codes.Ok, "")
		}
	}
}

type responseCapture struct {
	output       strings.Builder
	toolCalls    []string
	toolCallKeys map[string]int
	truncated    bool
	failed       bool
	cancelled    bool
	usage        *genai.GenerateContentResponseUsageMetadata
	finishReason string
	droppedTools int
}

func (c *responseCapture) add(span trace.Span, resp *model.LLMResponse) {
	if resp == nil {
		return
	}
	if resp.ModelVersion != "" {
		span.SetAttributes(attribute.String("neatlogs.llm.model_name", resp.ModelVersion))
	}
	if resp.UsageMetadata != nil {
		c.usage = resp.UsageMetadata
	}
	if resp.FinishReason != "" {
		c.finishReason = string(resp.FinishReason)
	}
	if resp.ErrorCode != "" || resp.ErrorMessage != "" {
		err := errors.New(strings.TrimSpace(resp.ErrorCode + ": " + resp.ErrorMessage))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		c.failed = true
	}
	if resp.Content == nil {
		return
	}
	for _, part := range resp.Content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" && !part.Thought {
			c.appendOutput(part.Text)
		}
		if part.FunctionCall != nil {
			c.addToolCall(span, part.FunctionCall)
		}
	}
}

func (c *responseCapture) appendOutput(value string) {
	remaining := maxOutputBytes - c.output.Len()
	if remaining <= 0 {
		c.truncated = true
		return
	}
	if len(value) > remaining {
		c.output.WriteString(boundedTextTo(value, remaining))
		c.truncated = true
		return
	}
	c.output.WriteString(value)
}

func (c *responseCapture) addToolCall(span trace.Span, call *genai.FunctionCall) {
	if call == nil {
		return
	}
	if c.toolCallKeys == nil {
		c.toolCallKeys = make(map[string]int)
	}
	key := strings.TrimSpace(call.ID)
	if key == "" {
		key = strings.TrimSpace(call.Name) + ":" + boundedJSON(call.Args)
	}
	if index, exists := c.toolCallKeys[key]; exists {
		args := boundedJSON(call.Args)
		span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.arguments", toolCallPrefix, index), args))
		c.toolCalls[index] = call.Name + "(" + args + ")"
		return
	}
	if len(c.toolCalls) >= maxToolCalls {
		c.droppedTools++
		return
	}
	index := len(c.toolCalls)
	c.toolCallKeys[key] = index
	args := boundedJSON(call.Args)
	span.SetAttributes(
		attribute.String(fmt.Sprintf("%s%d.name", toolCallPrefix, index), call.Name),
		attribute.String(fmt.Sprintf("%s%d.arguments", toolCallPrefix, index), args),
	)
	if call.ID != "" {
		span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.id", toolCallPrefix, index), call.ID))
	}
	c.toolCalls = append(c.toolCalls, call.Name+"("+args+")")
}

func (c *responseCapture) apply(span trace.Span) {
	output := c.output.String()
	if output == "" && len(c.toolCalls) > 0 {
		output = "Tool calls: " + strings.Join(c.toolCalls, ", ")
	}
	if output != "" {
		span.SetAttributes(
			attribute.String("neatlogs.output.value", output),
			attribute.String(outputMsgPrefix+"0.role", "assistant"),
			attribute.String(outputMsgPrefix+"0.content", output),
		)
	}
	if c.truncated {
		span.SetAttributes(attribute.Bool("neatlogs.output.truncated", true))
	}
	if c.finishReason != "" {
		span.SetAttributes(attribute.String("neatlogs.llm.finish_reason", c.finishReason))
	}
	if c.droppedTools > 0 {
		span.SetAttributes(attribute.Int("neatlogs.llm.tool_calls_truncated_count", c.droppedTools))
	}
	if c.usage != nil {
		span.SetAttributes(
			attribute.Int("neatlogs.llm.token_count.prompt", int(c.usage.PromptTokenCount)),
			attribute.Int("neatlogs.llm.token_count.completion", int(c.usage.CandidatesTokenCount)),
			attribute.Int("neatlogs.llm.token_count.total", int(c.usage.TotalTokenCount)),
			attribute.Int("neatlogs.llm.token_count.reasoning", int(c.usage.ThoughtsTokenCount)),
			attribute.Int("neatlogs.llm.token_count.cache_read", int(c.usage.CachedContentTokenCount)),
		)
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// setInputMessages writes the request messages as indexed input_messages.{i} and
// as neatlogs.input.value, a JSON array of {role, content}. The backend renders
// the table INPUT column from that array (its user-turn extractor parses the
// {role,content} shape), so the user message survives even when the system
// instruction is treated as a prompt template.
func setInputMessages(span trace.Span, req *model.LLMRequest) {
	if req == nil {
		return
	}
	idx := 0
	var msgs []chatMessage
	add := func(role, content string) {
		content = boundedText(content)
		span.SetAttributes(
			attribute.String(fmt.Sprintf("%s%d.role", inputMsgPrefix, idx), role),
			attribute.String(fmt.Sprintf("%s%d.content", inputMsgPrefix, idx), content),
		)
		if content != "" {
			msgs = append(msgs, chatMessage{Role: role, Content: content})
		}
		idx++
	}

	if req.Config != nil && req.Config.SystemInstruction != nil {
		add("system", contentText(req.Config.SystemInstruction))
	}
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		role := c.Role
		if role == "" {
			role = "user"
		}
		add(role, contentText(c))
	}

	if len(msgs) > 0 {
		span.SetAttributes(attribute.String("neatlogs.input.value", boundedJSON(msgs)))
	}
}

func setInvocationParameters(span trace.Span, req *model.LLMRequest) {
	if req == nil || req.Config == nil {
		return
	}
	config := req.Config
	params := make(map[string]any)
	if config.Temperature != nil {
		span.SetAttributes(attribute.Float64("neatlogs.llm.temperature", float64(*config.Temperature)))
		params["temperature"] = *config.Temperature
	}
	if config.TopP != nil {
		span.SetAttributes(attribute.Float64("neatlogs.llm.top_p", float64(*config.TopP)))
		params["top_p"] = *config.TopP
	}
	if config.TopK != nil {
		span.SetAttributes(attribute.Float64("neatlogs.llm.top_k", float64(*config.TopK)))
		params["top_k"] = *config.TopK
	}
	if config.MaxOutputTokens > 0 {
		span.SetAttributes(attribute.Int("neatlogs.llm.max_tokens", int(config.MaxOutputTokens)))
		params["max_output_tokens"] = config.MaxOutputTokens
	}
	if config.CandidateCount > 0 {
		params["candidate_count"] = config.CandidateCount
	}
	if len(params) > 0 {
		span.SetAttributes(attribute.String("neatlogs.llm.invocation_parameters", boundedJSON(params)))
	}
}

// contentText joins text without serializing binary or provider-owned payloads
// into span attributes. Non-text media remains represented by the surrounding
// ADK request rather than being copied into telemetry.
func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var text strings.Builder
	hasText := false
	for _, part := range c.Parts {
		if part != nil && part.Text != "" {
			if hasText {
				text.WriteByte('\n')
			}
			remaining := maxOutputBytes - text.Len()
			if remaining <= 0 {
				break
			}
			text.WriteString(boundedTextTo(part.Text, remaining))
			hasText = true
		}
	}
	if hasText {
		return text.String()
	}
	if len(c.Parts) > 0 {
		return "[non-text content]"
	}
	return ""
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"neatlogs_serialization_error":true}`
	}
	return string(b)
}

func boundedJSON(v any) string {
	value := mustJSON(v)
	if len(value) <= maxOutputBytes {
		return value
	}
	preview := boundedTextTo(value, maxOutputBytes-128)
	return mustJSON(map[string]any{
		"neatlogs_truncated": true,
		"preview":            preview,
	})
}

func boundedText(value string) string {
	return boundedTextTo(value, maxOutputBytes)
}

func boundedTextTo(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && (value[limit]&0xc0) == 0x80 {
		limit--
	}
	return value[:limit]
}
