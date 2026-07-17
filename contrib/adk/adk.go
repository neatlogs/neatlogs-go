// Package adk adds Neatlogs input/output capture to Google ADK.
//
// Deprecated: this integration is incompatible with the isolated Neatlogs SDK
// and no longer works. See DEPRECATED.md. It was built when neatlogs.Init
// registered the process-GLOBAL OpenTelemetry TracerProvider, so ADK's
// global-provider auto-instrumentation flowed into Neatlogs for free and
// WrapModel only had to annotate the live span with message text. The SDK now
// isolates onto a PRIVATE provider it never registers globally (so it can never
// export or parent a co-tenant's spans, e.g. Datadog). As a result ADK's spans
// never reach the Neatlogs provider, WrapModel finds no recording Neatlogs span
// to annotate, and no ADK trace is captured (the suite fails with "no spans
// captured"). The code is retained, compilable, for reference and a possible
// future redesign (e.g. an ADK-side hook that accepts an injected provider);
// do not wire it into new code.
//
// Original behavior: ADK-Go records prompt/completion TEXT only on the OTel
// *logs* signal, never on spans; WrapModel wrote the request/response messages
// onto the live generate_content span as neatlogs.llm.input_messages.* /
// output_messages.* so the I/O landed on the trace where Neatlogs expects it.
package adk

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

const (
	inputMsgPrefix  = "neatlogs.llm.input_messages."
	outputMsgPrefix = "neatlogs.llm.output_messages."
	toolCallPrefix  = "neatlogs.llm.tool_calls."
)

// instrumentedModel wraps an ADK model.LLM, adding I/O capture onto the active
// span. It preserves the wrapped model's Name and streaming semantics.
type instrumentedModel struct {
	inner model.LLM
}

// WrapModel returns a model.LLM that records request/response messages onto the
// generate_content span ADK starts around each call. If inner is nil it is
// returned unchanged.
//
// Deprecated: no longer functional under the isolated Neatlogs SDK — ADK's
// spans are started on the global OTel provider, which Neatlogs no longer owns,
// so there is no recording Neatlogs span to annotate. See the package doc and
// DEPRECATED.md.
func WrapModel(inner model.LLM) model.LLM {
	if inner == nil {
		return inner
	}
	return &instrumentedModel{inner: inner}
}

func (m *instrumentedModel) Name() string { return m.inner.Name() }

func (m *instrumentedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	span := trace.SpanFromContext(ctx)
	// Only annotate a live, recording span (the one ADK started). If there is
	// none, pass through untouched.
	if span == nil || !span.IsRecording() {
		return m.inner.GenerateContent(ctx, req, stream)
	}

	setInputMessages(span, req)

	seq := m.inner.GenerateContent(ctx, req, stream)
	return func(yield func(*model.LLMResponse, error) bool) {
		// ADK ends the span on yield of the final response, so output attributes
		// must be set BEFORE each yield (not after the loop). Partial deltas are
		// accumulated for the case where the final chunk carries no text.
		var streamed string
		var toolCalls []string
		toolIdx := 0

		annotate := func(resp *model.LLMResponse) {
			if resp == nil || resp.Content == nil {
				return
			}
			var text string
			for _, part := range resp.Content.Parts {
				if part == nil {
					continue
				}
				switch {
				case part.Text != "" && !part.Thought:
					text += part.Text
				case part.FunctionCall != nil:
					fc := part.FunctionCall
					span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.name", toolCallPrefix, toolIdx), fc.Name))
					if fc.ID != "" {
						span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.id", toolCallPrefix, toolIdx), fc.ID))
					}
					span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.arguments", toolCallPrefix, toolIdx), mustJSON(fc.Args)))
					toolCalls = append(toolCalls, fc.Name+"("+mustJSON(fc.Args)+")")
					toolIdx++
				}
			}
			if resp.Partial {
				streamed += text
				return
			}
			if text == "" {
				text = streamed
			}
			// A tool-deciding turn produces no text, only function calls. Surface
			// them as the span's output so the LLM row isn't blank.
			if text == "" && len(toolCalls) > 0 {
				text = "Tool calls: " + strings.Join(toolCalls, ", ")
			}
			if text != "" {
				span.SetAttributes(
					attribute.String("neatlogs.output.value", text),
					attribute.String(outputMsgPrefix+"0.role", "assistant"),
					attribute.String(outputMsgPrefix+"0.content", text),
				)
			}
		}

		for resp, err := range seq {
			if err == nil {
				annotate(resp) // must run BEFORE yield — ADK ends the span on yield of a final resp
			}
			if !yield(resp, err) {
				return
			}
		}
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
		span.SetAttributes(attribute.String("neatlogs.input.value", mustJSON(msgs)))
	}
}

// contentText joins the text parts of a Content, falling back to JSON for
// non-text parts.
func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var text string
	hasText := false
	for _, part := range c.Parts {
		if part != nil && part.Text != "" {
			if hasText {
				text += "\n"
			}
			text += part.Text
			hasText = true
		}
	}
	if hasText {
		return text
	}
	return mustJSON(c.Parts)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
