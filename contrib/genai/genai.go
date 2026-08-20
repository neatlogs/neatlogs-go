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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"iter"
	"sort"
	"strings"

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
	// stream is injectable only so lazy-consumption lifecycle behavior can be
	// proven without a network call. WrapGenAI always installs the real method.
	stream func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
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
	return &GenAIModels{
		models: client.Models, provider: provider, system: system,
		stream: client.Models.GenerateContentStream,
	}
}

// GenerateContent traces a single content-generation call.
func (g *GenAIModels) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	ctx, span, end := g.startLLMSpan(ctx, model, config, false)
	defer end()
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
		ctx, span, endSpan := g.startLLMSpan(ctx, model, config, true)
		setInputMessages(span, contents, config)
		setInvocationParams(span, config)
		setToolDefinitions(span, config)

		stream := g.stream
		if stream == nil {
			stream = g.models.GenerateContentStream
		}
		seq := stream(ctx, model, contents, config)
		var (
			acc       = newResponseAccumulator()
			sawError  bool
			cancelled bool
			ended     bool
		)
		end := func() {
			if ended {
				return
			}
			ended = true
			finalizeStream(span, acc, cancelled, sawError)
			if sawError {
				endSpan() // status already set by recordError; close span + auto-root
				return
			}
			endSpan()
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				sawError = true
				recordError(span, fmt.Errorf("stream panic: %v", recovered))
				end()
				panic(recovered)
			}
			end()
		}()

		for resp, err := range seq {
			if resp != nil {
				recordStreamChunk(span, acc, resp)
			}
			if err != nil {
				sawError = true
				recordError(span, err)
				yield(resp, err)
				return
			}
			if !yield(resp, nil) {
				cancelled = true
				break // consumer stopped early
			}
		}
		if ctx.Err() != nil {
			cancelled = true
		}
	}
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
		setContentMedia(span, fmt.Sprintf("%s%d", attrs.LLMInputMessagePrefix, idx), config.SystemInstruction, "input")
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
		setContentMedia(span, fmt.Sprintf("%s%d", attrs.LLMInputMessagePrefix, idx), c, "input")
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
	emit := func(toolType, name, description string, schema, definition any) {
		prefix := fmt.Sprintf("%s%d.", attrs.LLMToolPrefix, t)
		span.SetAttributes(
			attribute.String(prefix+"type", toolType),
			attribute.String(prefix+"definition", mustJSON(definition)),
		)
		if name != "" {
			span.SetAttributes(attribute.String(prefix+"name", name))
		}
		if description != "" {
			span.SetAttributes(attribute.String(prefix+"description", description))
		}
		if schema != nil {
			span.SetAttributes(attribute.String(prefix+"input_schema", mustJSON(schema)))
		}
		t++
	}
	for _, tool := range config.Tools {
		if tool == nil {
			continue
		}
		for _, fn := range tool.FunctionDeclarations {
			if fn == nil {
				continue
			}
			emit("function", fn.Name, fn.Description, fn.Parameters, fn)
		}
		variants := make([]struct {
			kind  string
			value any
		}, 0, 10)
		appendVariant := func(kind string, value any) {
			variants = append(variants, struct {
				kind  string
				value any
			}{kind, value})
		}
		if tool.Retrieval != nil {
			appendVariant("retrieval", tool.Retrieval)
		}
		if tool.ComputerUse != nil {
			appendVariant("computer_use", tool.ComputerUse)
		}
		if tool.FileSearch != nil {
			appendVariant("file_search", tool.FileSearch)
		}
		if tool.GoogleSearch != nil {
			appendVariant("google_search", tool.GoogleSearch)
		}
		if tool.GoogleMaps != nil {
			appendVariant("google_maps", tool.GoogleMaps)
		}
		if tool.CodeExecution != nil {
			appendVariant("code_execution", tool.CodeExecution)
		}
		if tool.EnterpriseWebSearch != nil {
			appendVariant("enterprise_web_search", tool.EnterpriseWebSearch)
		}
		if tool.GoogleSearchRetrieval != nil {
			appendVariant("google_search_retrieval", tool.GoogleSearchRetrieval)
		}
		if tool.ParallelAISearch != nil {
			appendVariant("parallel_ai_search", tool.ParallelAISearch)
		}
		if tool.URLContext != nil {
			appendVariant("url_context", tool.URLContext)
		}
		for _, variant := range variants {
			emit(variant.kind, "", "", nil, variant.value)
		}
		if len(tool.MCPServers) > 0 {
			emit("mcp_servers", "", "", nil, tool.MCPServers)
		}
	}
}

func finalizeResponse(span trace.Span, resp *genai.GenerateContentResponse) {
	if resp == nil {
		span.SetStatus(codes.Ok, "")
		return
	}
	acc := newResponseAccumulator()
	acc.add(resp)
	acc.apply(span)
	span.SetStatus(codes.Ok, "")
}

type accumulatedToolCall struct {
	choiceIndex int
	name        string
	id          string
	arguments   map[string]any
}

type accumulatedChoice struct {
	role              string
	text              string
	thinking          string
	finish            genai.FinishReason
	toolCalls         map[int]*accumulatedToolCall
	toolPositionsByID map[string]int
	nextToolPosition  int
	media             map[string]capturedMedia
}

type responseAccumulator struct {
	choices    map[int]*accumulatedChoice
	usage      *genai.GenerateContentResponseUsageMetadata
	responseID string
	chunkCount int
}

func newResponseAccumulator() *responseAccumulator {
	return &responseAccumulator{choices: make(map[int]*accumulatedChoice)}
}

func (a *responseAccumulator) add(resp *genai.GenerateContentResponse) {
	if resp == nil {
		return
	}
	for position, cand := range resp.Candidates {
		if cand == nil {
			continue
		}
		index := int(cand.Index)
		if index == 0 && position > 0 {
			index = position
		}
		choice := a.choices[index]
		if choice == nil {
			choice = &accumulatedChoice{
				role:              "assistant",
				toolCalls:         make(map[int]*accumulatedToolCall),
				toolPositionsByID: make(map[string]int),
				media:             make(map[string]capturedMedia),
			}
			a.choices[index] = choice
		}
		if cand.Content != nil {
			if cand.Content.Role != "" && cand.Content.Role != "model" {
				choice.role = cand.Content.Role
			}
			for _, part := range cand.Content.Parts {
				if part == nil {
					continue
				}
				for _, media := range partMedia(part, "output") {
					choice.media[media.key()] = media
				}
				switch {
				case part.Thought && part.Text != "":
					choice.thinking += part.Text
				case part.Text != "":
					choice.text += part.Text
				case part.FunctionCall != nil:
					toolPosition, linked := choice.toolPositionsByID[part.FunctionCall.ID]
					if part.FunctionCall.ID == "" || !linked {
						toolPosition = choice.nextToolPosition
						choice.nextToolPosition++
						if part.FunctionCall.ID != "" {
							choice.toolPositionsByID[part.FunctionCall.ID] = toolPosition
						}
					}
					call := choice.toolCalls[toolPosition]
					if call == nil {
						call = &accumulatedToolCall{
							choiceIndex: index,
							arguments:   make(map[string]any),
						}
						choice.toolCalls[toolPosition] = call
					}
					if part.FunctionCall.Name != "" {
						call.name = part.FunctionCall.Name
					}
					if part.FunctionCall.ID != "" {
						call.id = part.FunctionCall.ID
					}
					for key, value := range part.FunctionCall.Args {
						call.arguments[key] = value
					}
				}
			}
		}
		if cand.FinishReason != "" {
			choice.finish = cand.FinishReason
		}
	}
	if resp.UsageMetadata != nil {
		a.usage = resp.UsageMetadata
	}
	if resp.ResponseID != "" {
		a.responseID = resp.ResponseID
	}
	a.chunkCount++
}

func (a *responseAccumulator) apply(span trace.Span) {
	span.SetAttributes(attribute.String("neatlogs.capture_fidelity", "native"))
	indexes := make([]int, 0, len(a.choices))
	for index := range a.choices {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	toolIndex := 0
	for _, index := range indexes {
		choice := a.choices[index]
		prefix := fmt.Sprintf("%s%d.", attrs.LLMOutputMessagePrefix, index)
		span.SetAttributes(attribute.String(prefix+"role", choice.role))
		if choice.text != "" {
			span.SetAttributes(attribute.String(prefix+"content", choice.text))
		}
		if choice.thinking != "" {
			span.SetAttributes(attribute.String(prefix+"thinking", choice.thinking))
		}
		mediaKeys := make([]string, 0, len(choice.media))
		for key := range choice.media {
			mediaKeys = append(mediaKeys, key)
		}
		sort.Strings(mediaKeys)
		for mediaIndex, key := range mediaKeys {
			setMediaAttributes(span, fmt.Sprintf("%smedia.%d.", prefix, mediaIndex), choice.media[key])
		}
		if choice.finish != "" {
			span.SetAttributes(attribute.String(fmt.Sprintf("%s%d.finish_reason", attrs.LLMChoicePrefix, index), string(choice.finish)))
			if index == indexes[0] {
				span.SetAttributes(attribute.String(attrs.LLMFinishReason, string(choice.finish)))
			}
		}
		toolPositions := make([]int, 0, len(choice.toolCalls))
		for position := range choice.toolCalls {
			toolPositions = append(toolPositions, position)
		}
		sort.Ints(toolPositions)
		for _, position := range toolPositions {
			call := choice.toolCalls[position]
			callPrefix := fmt.Sprintf("%s%d.", attrs.LLMToolCallPrefix, toolIndex)
			arguments := mustJSON(call.arguments)
			synthetic := false
			if call.id == "" {
				context := span.SpanContext()
				argumentDigest := sha256.Sum256([]byte(arguments))
				identity := fmt.Sprintf(
					"%s:%s:%d:%d:%s:%x",
					context.TraceID(), context.SpanID(), index, position, call.name, argumentDigest,
				)
				digest := sha256.Sum256([]byte(identity))
				call.id = fmt.Sprintf("nl_%x", digest[:12])
				synthetic = true
			}
			span.SetAttributes(
				attribute.String(callPrefix+"id", call.id),
				attribute.String(callPrefix+"name", call.name),
				attribute.Int(callPrefix+"choice_index", call.choiceIndex),
				attribute.Int(callPrefix+"tool_call_index", position),
				attribute.String(callPrefix+"arguments", arguments),
			)
			if synthetic {
				span.SetAttributes(attribute.Bool(callPrefix+"id_synthetic", true))
			}
			toolIndex++
		}
	}
	if a.responseID != "" {
		span.SetAttributes(attribute.String(attrs.LLMResponseID, a.responseID))
	}
	setUsage(span, a.usage)
}

const maxSemanticStreamEvents = 128

func recordStreamChunk(span trace.Span, acc *responseAccumulator, resp *genai.GenerateContentResponse) {
	chunkIndex := acc.chunkCount
	acc.add(resp)
	if chunkIndex >= maxSemanticStreamEvents {
		return
	}
	span.AddEvent(attrs.StreamChunkEvent, trace.WithAttributes(
		attribute.Int(attrs.StreamChunkIndex, chunkIndex),
		attribute.String(attrs.StreamChunkSummary, semanticChunkSummary(resp)),
	))
}

func finalizeStream(span trace.Span, acc *responseAccumulator, cancelled, sawError bool) {
	acc.apply(span)
	span.SetAttributes(attribute.Int(attrs.StreamChunkCount, acc.chunkCount))
	if acc.chunkCount > maxSemanticStreamEvents {
		span.SetAttributes(attribute.Int(attrs.StreamEventsDropped, acc.chunkCount-maxSemanticStreamEvents))
	}
	if sawError {
		return
	}
	if cancelled {
		span.SetAttributes(attribute.Bool(attrs.StreamCancelled, true))
		span.SetStatus(codes.Unset, "")
		return
	}
	span.SetStatus(codes.Ok, "")
}

func semanticChunkSummary(resp *genai.GenerateContentResponse) string {
	type choiceSummary struct {
		Index         int    `json:"choice_index"`
		TextBytes     int    `json:"text_bytes"`
		ThinkingBytes int    `json:"thinking_bytes"`
		ToolCalls     int    `json:"tool_calls"`
		FinishReason  string `json:"finish_reason,omitempty"`
	}
	summary := struct {
		Choices []choiceSummary `json:"choices"`
		Usage   bool            `json:"usage"`
	}{Usage: resp != nil && resp.UsageMetadata != nil}
	if resp == nil {
		return mustJSON(summary)
	}
	for position, candidate := range resp.Candidates {
		if candidate == nil {
			continue
		}
		index := int(candidate.Index)
		if index == 0 && position > 0 {
			index = position
		}
		choice := choiceSummary{Index: index, FinishReason: string(candidate.FinishReason)}
		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				if part == nil {
					continue
				}
				if part.Thought {
					choice.ThinkingBytes += len(part.Text)
				} else {
					choice.TextBytes += len(part.Text)
				}
				if part.FunctionCall != nil {
					choice.ToolCalls++
				}
			}
		}
		summary.Choices = append(summary.Choices, choice)
	}
	return mustJSON(summary)
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

type capturedMedia struct {
	id         string
	kind       string
	source     string
	mimeType   string
	byteLength int
	sha256     string
	reference  string
	purpose    string
	state      string
}

func (m capturedMedia) key() string { return m.sha256 + ":" + m.reference + ":" + m.kind }

func mediaKind(mimeType string) string {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return "image"
	case strings.HasPrefix(mimeType, "audio/"):
		return "audio"
	case strings.HasPrefix(mimeType, "video/"):
		return "video"
	case mimeType == "application/pdf", strings.HasPrefix(mimeType, "text/"):
		return "document"
	default:
		return "media"
	}
}

func partMedia(part *genai.Part, purpose string) []capturedMedia {
	if part == nil {
		return nil
	}
	media := make([]capturedMedia, 0, 2)
	if part.InlineData != nil {
		digest := sha256.Sum256(part.InlineData.Data)
		media = append(media, capturedMedia{
			id: fmt.Sprintf("nl_media_%x", digest[:12]), kind: mediaKind(part.InlineData.MIMEType),
			source: "inline", mimeType: part.InlineData.MIMEType, byteLength: len(part.InlineData.Data),
			sha256: fmt.Sprintf("%x", digest), purpose: purpose, state: "inline",
		})
	}
	if part.FileData != nil && part.FileData.FileURI != "" {
		digest := sha256.Sum256([]byte(part.FileData.FileURI))
		source := "provider"
		if strings.HasPrefix(part.FileData.FileURI, "http://") || strings.HasPrefix(part.FileData.FileURI, "https://") {
			source = "url"
		}
		media = append(media, capturedMedia{
			id: fmt.Sprintf("nl_media_%x", digest[:12]), kind: mediaKind(part.FileData.MIMEType),
			source: source, mimeType: part.FileData.MIMEType, reference: part.FileData.FileURI,
			purpose: purpose, state: "available",
		})
	}
	return media
}

func setContentMedia(span trace.Span, prefix string, content *genai.Content, purpose string) {
	if content == nil {
		return
	}
	index := 0
	seen := make(map[string]struct{})
	for _, part := range content.Parts {
		for _, media := range partMedia(part, purpose) {
			if _, exists := seen[media.key()]; exists {
				continue
			}
			seen[media.key()] = struct{}{}
			setMediaAttributes(span, fmt.Sprintf("%s.media.%d.", prefix, index), media)
			index++
		}
	}
}

func setMediaAttributes(span trace.Span, prefix string, media capturedMedia) {
	values := []attribute.KeyValue{
		attribute.String(prefix+"id", media.id), attribute.String(prefix+"type", media.kind),
		attribute.String(prefix+"source", media.source), attribute.String(prefix+"mime_type", media.mimeType),
		attribute.String(prefix+"purpose", media.purpose), attribute.String(prefix+"state", media.state),
	}
	if media.byteLength > 0 {
		values = append(values, attribute.Int(prefix+"byte_length", media.byteLength))
	}
	if media.sha256 != "" {
		values = append(values, attribute.String(prefix+"sha256", media.sha256))
	}
	if media.reference != "" {
		values = append(values, attribute.String(prefix+"reference", media.reference))
	}
	span.SetAttributes(values...)
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
