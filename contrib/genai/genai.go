// Package genai adds Neatlogs tracing to google.golang.org/genai (Gemini /
// Vertex AI) calls. It lives in its own module so the heavy google.golang.org/
// genai dependency never reaches applications that import only the root
// neatlogs package.
//
//	import nlgenai "github.com/neatlogs/neatlogs-go/contrib/genai"
//	gc := nlgenai.WrapGenAI(client)
//	resp, _ := gc.GenerateContent(ctx, "gemini-2.5-flash", contents, cfg)
package genai

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// Provider/system identifiers, matching the TS SDK: the Vertex AI backend is
// distinguished from Gemini (Google AI Studio) so the two traffic sources are
// separable in the backend.
const (
	geminiProvider = "google_genai"
	geminiSystem   = "google"
	vertexProvider = "vertex_ai"
	vertexSystem   = "vertexai"
)

// GenAIModels mirrors the call surface of (*genai.Client).Models, tracing each
// call. It is returned by WrapGenAI and is a drop-in replacement: the method
// signatures match google.golang.org/genai exactly, so existing call sites
// change by one line (the client they call) and nothing else.
type GenAIModels struct {
	models   *genai.Models
	provider string
	system   string
	stream   func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
}

// WrapGenAI wraps a genai.Client so its model calls emit Neatlogs spans.
//
//	client, _ := genai.NewClient(ctx, &genai.ClientConfig{APIKey: key})
//	gc := nlgenai.WrapGenAI(client)
//	resp, _ := gc.GenerateContent(ctx, "gemini-2.5-flash", contents, cfg)
//
// Spans carry full request/response detail — input/output messages, tool
// definitions and calls, invocation parameters, token usage and finish reason —
// keyed in the neatlogs.* namespace. The Vertex AI backend is detected from the
// client config and tagged distinctly from the Gemini API. Passing a context
// from neatlogs.WithClient routes every wrapped operation to that Client;
// otherwise operations use process-wide neatlogs.Init.
func WrapGenAI(client *genai.Client) *GenAIModels {
	if client == nil {
		return &GenAIModels{provider: geminiProvider, system: geminiSystem}
	}
	provider, system := geminiProvider, geminiSystem
	if client.ClientConfig().Backend == genai.BackendVertexAI {
		provider, system = vertexProvider, vertexSystem
	}
	return &GenAIModels{models: client.Models, provider: provider, system: system, stream: client.Models.GenerateContentStream}
}

// GenerateContent traces a single content-generation call.
func (g *GenAIModels) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	ctx, span, end := g.startLLMSpan(ctx, model, config, false)
	defer end()
	span.SetAttributes(attribute.String(attrs.LLMStreamCompletionState, "not_streamed"))
	setInputMessages(span, contents, config)
	setInvocationParams(span, config)
	setToolDefinitions(span, config)

	resp, err := g.models.GenerateContent(ctx, model, contents, config)
	if err != nil {
		recordError(span, err)
		return resp, err
	}
	finalizeResponse(span, resp)
	return resp, nil
}

// GenerateContentStream traces a streaming generation, accumulating chunks to
// reconstruct the response when the stream is fully consumed.
func (g *GenAIModels) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		streamCtx, span, endSpan := g.startLLMSpan(ctx, model, config, true)
		setInputMessages(span, contents, config)
		setInvocationParams(span, config)
		setToolDefinitions(span, config)
		span.SetAttributes(
			attribute.String(attrs.LLMStreamCompletionState, "consumer_cancelled"),
			attribute.Int(attrs.LLMStreamChunkCount, 0),
			attribute.Bool(attrs.LLMStreamOutputTruncated, false),
		)
		var (
			text      string
			truncated bool
			chunks    int
			finish    genai.FinishReason
			usage     *genai.GenerateContentResponseUsageMetadata
			respID    string
			endOnce   sync.Once
		)
		finishStream := func(state string, err error) {
			endOnce.Do(func() {
				span.SetAttributes(
					attribute.String(attrs.LLMStreamCompletionState, state),
					attribute.Int(attrs.LLMStreamChunkCount, chunks),
					attribute.Bool(attrs.LLMStreamOutputTruncated, truncated),
				)
				if text != "" {
					span.SetAttributes(
						attribute.String(attrs.LLMOutputMessagePrefix+"0.role", "assistant"),
						attribute.String(attrs.LLMOutputMessagePrefix+"0.content", text),
						attribute.String(attrs.LLMStreamPartialOutput, text),
					)
				}
				if finish != "" {
					span.SetAttributes(attribute.String(attrs.LLMFinishReason, string(finish)))
				}
				if respID != "" {
					span.SetAttributes(attribute.String(attrs.LLMResponseID, respID))
				}
				setUsage(span, usage)
				if err != nil {
					recordError(span, err)
				} else {
					span.SetStatus(codes.Ok, "")
				}
				endSpan()
			})
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				finishStream("provider_error", fmt.Errorf("stream panic: %T", recovered))
				panic(recovered)
			}
		}()
		stream := g.stream
		if stream == nil {
			stream = g.models.GenerateContentStream
		}
		seq := stream(streamCtx, model, contents, config)

		for resp, err := range seq {
			if err != nil {
				state := "provider_error"
				if streamCtx.Err() != nil {
					state = "consumer_cancelled"
				}
				finishStream(state, err)
				yield(resp, err)
				return
			}
			chunks++
			if resp != nil {
				text, truncated = appendBounded(text, extractText(resp), maxStreamOutputRunes, truncated)
				span.SetAttributes(
					attribute.Int(attrs.LLMStreamChunkCount, chunks),
					attribute.Bool(attrs.LLMStreamOutputTruncated, truncated),
					attribute.String(attrs.LLMStreamPartialOutput, text),
				)
				if resp.UsageMetadata != nil {
					usage = resp.UsageMetadata
				}
				if resp.ResponseID != "" {
					respID = resp.ResponseID
				}
				if fr := finishReason(resp); fr != "" {
					finish = fr
				}
			}
			continued, panicValue := callConsumer(yield, resp)
			if panicValue != nil {
				finishStream("consumer_cancelled", fmt.Errorf("stream consumer panic: %T", panicValue))
				panic(panicValue)
			}
			if !continued {
				finishStream("consumer_cancelled", context.Canceled)
				return
			}
			if err := streamCtx.Err(); err != nil {
				finishStream("consumer_cancelled", err)
				return
			}
		}
		if err := streamCtx.Err(); err != nil {
			finishStream("consumer_cancelled", err)
			return
		}
		finishStream("complete", nil)
	}
}

const maxStreamOutputRunes = 64 * 1024

func appendBounded(current, addition string, limit int, alreadyTruncated bool) (string, bool) {
	remaining := limit - utf8.RuneCountInString(current)
	if remaining <= 0 {
		return current, alreadyTruncated || addition != ""
	}
	runes := []rune(addition)
	if len(runes) <= remaining {
		return current + addition, alreadyTruncated
	}
	return current + string(runes[:remaining]), true
}

func callConsumer(yield func(*genai.GenerateContentResponse, error) bool, resp *genai.GenerateContentResponse) (continued bool, panicValue any) {
	defer func() { panicValue = recover() }()
	continued = yield(resp, nil)
	return continued, nil
}

// EmbedContent traces an embedding call.
func (g *GenAIModels) EmbedContent(ctx context.Context, model string, contents []*genai.Content, config *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error) {
	ctx, span, end := neatlogs.StartProviderSpan(ctx, "google_genai.models.embed_content", attrs.KindEmbedding)
	defer end()
	span.SetAttributes(
		attribute.String(attrs.SpanKind, attrs.KindEmbedding),
		attribute.String(attrs.LLMProvider, g.provider),
		attribute.String(attrs.EmbeddingModelName, model),
	)

	resp, err := g.models.EmbedContent(ctx, model, contents, config)
	if err != nil {
		recordError(span, err)
		return resp, err
	}
	if resp != nil {
		span.SetAttributes(attribute.Int(attrs.EmbeddingCount, len(resp.Embeddings)))
		if len(resp.Embeddings) > 0 && resp.Embeddings[0] != nil {
			span.SetAttributes(attribute.Int(attrs.EmbeddingDimensions, len(resp.Embeddings[0].Values)))
		}
	}
	span.SetStatus(codes.Ok, "")
	return resp, nil
}

// CountTokens traces a token-counting call.
func (g *GenAIModels) CountTokens(ctx context.Context, model string, contents []*genai.Content, config *genai.CountTokensConfig) (*genai.CountTokensResponse, error) {
	ctx, span, end := neatlogs.StartProviderSpan(ctx, "google_genai.models.count_tokens", attrs.KindLLM)
	defer end()
	span.SetAttributes(
		attribute.String(attrs.SpanKind, attrs.KindLLM),
		attribute.String(attrs.LLMProvider, g.provider),
		attribute.String(attrs.LLMModelName, model),
	)

	resp, err := g.models.CountTokens(ctx, model, contents, config)
	if err != nil {
		recordError(span, err)
		return resp, err
	}
	if resp != nil {
		span.SetAttributes(attribute.Int(attrs.LLMTokenPrompt, int(resp.TotalTokens)))
	}
	span.SetStatus(codes.Ok, "")
	return resp, nil
}

// Raw returns the underlying genai.Models for any method this wrapper does not
// trace (e.g. cached-content operations), so callers are never blocked.
func (g *GenAIModels) Raw() *genai.Models { return g.models }

// ── helpers ──────────────────────────────────────────────────────────────

func (g *GenAIModels) startLLMSpan(ctx context.Context, model string, _ *genai.GenerateContentConfig, streaming bool) (context.Context, trace.Span, func()) {
	ctx, span, end := neatlogs.StartProviderSpan(ctx, "google_genai.models.generate_content", attrs.KindLLM)
	span.SetAttributes(
		attribute.String(attrs.SpanKind, attrs.KindLLM),
		attribute.String(attrs.LLMProvider, g.provider),
		attribute.String(attrs.LLMSystem, g.system),
		attribute.String(attrs.LLMModelName, model),
		attribute.Bool(attrs.LLMStreaming, streaming),
	)
	return ctx, span, end
}

func setInputMessages(span trace.Span, contents []*genai.Content, config *genai.GenerateContentConfig) {
	idx := 0
	if config != nil && config.SystemInstruction != nil {
		span.SetAttributes(
			attribute.String(fmt.Sprintf("%s%d.role", attrs.LLMInputMessagePrefix, idx), "system"),
			attribute.String(fmt.Sprintf("%s%d.content", attrs.LLMInputMessagePrefix, idx), contentText(config.SystemInstruction)),
		)
		idx++
	}
	for _, c := range contents {
		if c == nil {
			continue
		}
		role := c.Role
		if role == "" {
			role = "user"
		}
		span.SetAttributes(
			attribute.String(fmt.Sprintf("%s%d.role", attrs.LLMInputMessagePrefix, idx), role),
			attribute.String(fmt.Sprintf("%s%d.content", attrs.LLMInputMessagePrefix, idx), contentText(c)),
		)
		idx++
	}
}

func setInvocationParams(span trace.Span, config *genai.GenerateContentConfig) {
	if config == nil {
		return
	}
	params := map[string]any{}
	if config.Temperature != nil {
		span.SetAttributes(attribute.Float64(attrs.LLMTemperature, float64(*config.Temperature)))
		params["temperature"] = *config.Temperature
	}
	if config.TopP != nil {
		span.SetAttributes(attribute.Float64(attrs.LLMTopP, float64(*config.TopP)))
		params["top_p"] = *config.TopP
	}
	if config.TopK != nil {
		span.SetAttributes(attribute.Float64(attrs.LLMTopK, float64(*config.TopK)))
		params["top_k"] = *config.TopK
	}
	if config.MaxOutputTokens != 0 {
		span.SetAttributes(attribute.Int(attrs.LLMMaxTokens, int(config.MaxOutputTokens)))
		params["max_tokens"] = config.MaxOutputTokens
	}
	if config.FrequencyPenalty != nil {
		span.SetAttributes(attribute.Float64(attrs.LLMFrequencyPenalty, float64(*config.FrequencyPenalty)))
		params["frequency_penalty"] = *config.FrequencyPenalty
	}
	if config.PresencePenalty != nil {
		span.SetAttributes(attribute.Float64(attrs.LLMPresencePenalty, float64(*config.PresencePenalty)))
		params["presence_penalty"] = *config.PresencePenalty
	}
	if len(params) > 0 {
		span.SetAttributes(attribute.String(attrs.LLMInvocationParameters, mustJSON(params)))
	}
}

func setToolDefinitions(span trace.Span, config *genai.GenerateContentConfig) {
	if config == nil {
		return
	}
	t := 0
	for _, tool := range config.Tools {
		if tool == nil {
			continue
		}
		for _, fn := range tool.FunctionDeclarations {
			if fn == nil {
				continue
			}
			span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.name", attrs.LLMToolPrefix, t), fn.Name))
			if fn.Description != "" {
				span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.description", attrs.LLMToolPrefix, t), fn.Description))
			}
			if fn.Parameters != nil {
				span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.input_schema", attrs.LLMToolPrefix, t), mustJSON(fn.Parameters)))
			}
			t++
		}
	}
}

func finalizeResponse(span trace.Span, resp *genai.GenerateContentResponse) {
	if resp == nil {
		span.SetStatus(codes.Ok, "")
		return
	}
	var text string
	toolIdx := 0
	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part == nil {
				continue
			}
			switch {
			case part.Thought && part.Text != "":
				span.SetAttributes(attribute.String(attrs.LLMOutputMessagePrefix+"0.thinking", part.Text))
			case part.Text != "":
				text += part.Text
			case part.FunctionCall != nil:
				fc := part.FunctionCall
				span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.name", attrs.LLMToolCallPrefix, toolIdx), fc.Name))
				if fc.ID != "" {
					span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.id", attrs.LLMToolCallPrefix, toolIdx), fc.ID))
				}
				span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.arguments", attrs.LLMToolCallPrefix, toolIdx), mustJSON(fc.Args)))
				toolIdx++
			}
		}
		if cand.FinishReason != "" {
			span.SetAttributes(attribute.String(attrs.LLMFinishReason, string(cand.FinishReason)))
		}
	}
	if text != "" {
		span.SetAttributes(
			attribute.String(attrs.LLMOutputMessagePrefix+"0.role", "assistant"),
			attribute.String(attrs.LLMOutputMessagePrefix+"0.content", text),
		)
	}
	if resp.ResponseID != "" {
		span.SetAttributes(attribute.String(attrs.LLMResponseID, resp.ResponseID))
	}
	setUsage(span, resp.UsageMetadata)
	span.SetStatus(codes.Ok, "")
}

// setUsage maps Gemini UsageMetadata onto neatlogs token-count attributes.
// reasoning (thoughts) tokens are reported separately; total is taken from the
// response when present.
func setUsage(span trace.Span, usage *genai.GenerateContentResponseUsageMetadata) {
	if usage == nil {
		return
	}
	if usage.PromptTokenCount != 0 {
		span.SetAttributes(attribute.Int(attrs.LLMTokenPrompt, int(usage.PromptTokenCount)))
	}
	if usage.CandidatesTokenCount != 0 {
		span.SetAttributes(attribute.Int(attrs.LLMTokenCompletion, int(usage.CandidatesTokenCount)))
	}
	if usage.TotalTokenCount != 0 {
		span.SetAttributes(attribute.Int(attrs.LLMTokenTotal, int(usage.TotalTokenCount)))
	}
	if usage.ThoughtsTokenCount != 0 {
		span.SetAttributes(attribute.Int(attrs.LLMTokenReasoning, int(usage.ThoughtsTokenCount)))
	}
	if usage.CachedContentTokenCount != 0 {
		span.SetAttributes(attribute.Int(attrs.LLMTokenCacheRead, int(usage.CachedContentTokenCount)))
	}
}

func extractText(resp *genai.GenerateContentResponse) string {
	var text string
	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part != nil && part.Text != "" && !part.Thought {
				text += part.Text
			}
		}
	}
	return text
}

func finishReason(resp *genai.GenerateContentResponse) genai.FinishReason {
	for _, cand := range resp.Candidates {
		if cand != nil && cand.FinishReason != "" {
			return cand.FinishReason
		}
	}
	return ""
}

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

// recordError marks the span as failed. It does NOT end the span; callers end
// via the auto-root-aware end func so the workflow root is also closed.
func recordError(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
