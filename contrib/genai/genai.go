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
	"errors"
	"fmt"
	"iter"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
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
	toolDefinitions, truncatedTools := collectToolDefinitions(config)

	resp, err := g.models.GenerateContent(ctx, model, contents, config)
	if err != nil {
		recordError(span, err)
		setCapturedToolDefinitions(span, toolDefinitions, truncatedTools)
		return resp, err
	}
	finalizeResponse(span, resp)
	setCapturedToolDefinitions(span, toolDefinitions, truncatedTools)
	return resp, nil
}

// GenerateContentStream traces a streaming generation, accumulating chunks to
// reconstruct the response when the stream is fully consumed.
func (g *GenAIModels) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		ctx, span, endSpan := g.startLLMSpan(ctx, model, config, true)
		setInputMessages(span, contents, config)
		setInvocationParams(span, config)
		toolDefinitions, truncatedTools := collectToolDefinitions(config)

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
			// Response and usage attributes are authoritative. Write bounded tool
			// definitions afterwards so they cannot consume the default OTel
			// 128-attribute budget first.
			setCapturedToolDefinitions(span, toolDefinitions, truncatedTools)
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
				if isContextCancellation(ctx, err) {
					cancelled = true
					yield(resp, err)
					return
				}
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

func isContextCancellation(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded)
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

const (
	maxCapturedToolDefinitions = 8
	maxCapturedMediaReferences = 8
)

type capturedToolDefinition struct {
	toolType          string
	name              string
	description       string
	inputSchema       string
	configurationJSON string
}

// The DTOs below are deliberately independent of provider JSON structs. New
// Google GenAI fields are excluded until explicitly reviewed, and known auth
// material (keys, tokens, headers, and Secret Manager references) has no field
// through which it can enter telemetry.
type safeFunctionDefinition struct {
	// Name, description, and input schema are intentionally absent: their
	// canonical attributes are the only copies the backend must classify.
	Response           *genai.Schema  `json:"response,omitempty"`
	ResponseJSONSchema any            `json:"responseJsonSchema,omitempty"`
	Behavior           genai.Behavior `json:"behavior,omitempty"`
}

type safeExternalAPIDefinition struct {
	APISpec             genai.APISpec                    `json:"apiSpec,omitempty"`
	ElasticSearchParams *safeExternalAPISearchDefinition `json:"elasticSearchParams,omitempty"`
	Endpoint            string                           `json:"endpoint,omitempty"`
	SimpleSearchParams  *struct{}                        `json:"simpleSearchParams,omitempty"`
}

type safeExternalAPISearchDefinition struct {
	Index          string `json:"index,omitempty"`
	NumHits        *int32 `json:"numHits,omitempty"`
	SearchTemplate string `json:"searchTemplate,omitempty"`
}

type safeVertexAISearchDataStoreDefinition struct {
	DataStore string `json:"dataStore,omitempty"`
	Filter    string `json:"filter,omitempty"`
}

type safeVertexAISearchDefinition struct {
	DataStoreSpecs []safeVertexAISearchDataStoreDefinition `json:"dataStoreSpecs,omitempty"`
	Datastore      string                                  `json:"datastore,omitempty"`
	Engine         string                                  `json:"engine,omitempty"`
	Filter         string                                  `json:"filter,omitempty"`
	MaxResults     *int32                                  `json:"maxResults,omitempty"`
}

type safeRAGResourceDefinition struct {
	RAGCorpus  string   `json:"ragCorpus,omitempty"`
	RAGFileIDs []string `json:"ragFileIds,omitempty"`
}

type safeRAGFilterDefinition struct {
	MetadataFilter            string   `json:"metadataFilter,omitempty"`
	VectorDistanceThreshold   *float64 `json:"vectorDistanceThreshold,omitempty"`
	VectorSimilarityThreshold *float64 `json:"vectorSimilarityThreshold,omitempty"`
}

type safeRAGRankingDefinition struct {
	LlmRanker   *safeRAGRankerDefinition `json:"llmRanker,omitempty"`
	RankService *safeRAGRankerDefinition `json:"rankService,omitempty"`
}

type safeRAGRankerDefinition struct {
	ModelName string `json:"modelName,omitempty"`
}

type safeRAGHybridSearchDefinition struct {
	Alpha *float32 `json:"alpha,omitempty"`
}

type safeRAGRetrievalConfigDefinition struct {
	Filter       *safeRAGFilterDefinition       `json:"filter,omitempty"`
	HybridSearch *safeRAGHybridSearchDefinition `json:"hybridSearch,omitempty"`
	Ranking      *safeRAGRankingDefinition      `json:"ranking,omitempty"`
	TopK         *int32                         `json:"topK,omitempty"`
}

type safeVertexRAGStoreDefinition struct {
	RAGCorpora              []string                          `json:"ragCorpora,omitempty"`
	RAGResources            []safeRAGResourceDefinition       `json:"ragResources,omitempty"`
	RAGRetrievalConfig      *safeRAGRetrievalConfigDefinition `json:"ragRetrievalConfig,omitempty"`
	SimilarityTopK          *int32                            `json:"similarityTopK,omitempty"`
	StoreContext            *bool                             `json:"storeContext,omitempty"`
	VectorDistanceThreshold *float64                          `json:"vectorDistanceThreshold,omitempty"`
}

type safeRetrievalDefinition struct {
	DisableAttribution bool                          `json:"disableAttribution,omitempty"`
	ExternalAPI        *safeExternalAPIDefinition    `json:"externalApi,omitempty"`
	VertexAISearch     *safeVertexAISearchDefinition `json:"vertexAiSearch,omitempty"`
	VertexRAGStore     *safeVertexRAGStoreDefinition `json:"vertexRagStore,omitempty"`
}

type safeComputerUseDefinition struct {
	Environment                    genai.Environment `json:"environment,omitempty"`
	ExcludedPredefinedFunctions    []string          `json:"excludedPredefinedFunctions,omitempty"`
	EnablePromptInjectionDetection *bool             `json:"enablePromptInjectionDetection,omitempty"`
}

type safeFileSearchDefinition struct {
	FileSearchStoreNames []string `json:"fileSearchStoreNames,omitempty"`
	TopK                 *int32   `json:"topK,omitempty"`
	MetadataFilter       string   `json:"metadataFilter,omitempty"`
}

type safeSearchTypesDefinition struct {
	WebSearch   *struct{} `json:"webSearch,omitempty"`
	ImageSearch *struct{} `json:"imageSearch,omitempty"`
}

type safeIntervalDefinition struct {
	StartTime string `json:"startTime,omitempty"`
	EndTime   string `json:"endTime,omitempty"`
}

type safeGoogleSearchDefinition struct {
	SearchTypes        *safeSearchTypesDefinition `json:"searchTypes,omitempty"`
	BlockingConfidence genai.PhishBlockThreshold  `json:"blockingConfidence,omitempty"`
	ExcludeDomains     []string                   `json:"excludeDomains,omitempty"`
	TimeRangeFilter    *safeIntervalDefinition    `json:"timeRangeFilter,omitempty"`
}

type safeGoogleMapsDefinition struct {
	EnableWidget *bool `json:"enableWidget,omitempty"`
}

type safeEnterpriseWebSearchDefinition struct {
	BlockingConfidence genai.PhishBlockThreshold `json:"blockingConfidence,omitempty"`
	ExcludeDomains     []string                  `json:"excludeDomains,omitempty"`
}

type safeGoogleSearchRetrievalDefinition struct {
	DynamicThreshold *float32                         `json:"dynamicThreshold,omitempty"`
	Mode             genai.DynamicRetrievalConfigMode `json:"mode,omitempty"`
}

type safeMCPServerDefinition struct {
	Name                    string                                 `json:"name,omitempty"`
	StreamableHTTPTransport *safeStreamableHTTPTransportDefinition `json:"streamableHttpTransport,omitempty"`
}

type safeStreamableHTTPTransportDefinition struct {
	URL              string `json:"url,omitempty"`
	SSEReadTimeout   string `json:"sseReadTimeout,omitempty"`
	Timeout          string `json:"timeout,omitempty"`
	TerminateOnClose *bool  `json:"terminateOnClose,omitempty"`
}

func setToolDefinitions(span trace.Span, config *genai.GenerateContentConfig) {
	definitions, truncated := collectToolDefinitions(config)
	setCapturedToolDefinitions(span, definitions, truncated)
}

func collectToolDefinitions(config *genai.GenerateContentConfig) ([]capturedToolDefinition, int) {
	if config == nil {
		return nil, 0
	}
	definitions := make([]capturedToolDefinition, 0, maxCapturedToolDefinitions)
	truncated := 0
	capture := func(toolType, name, description string, schema, configuration any) {
		if len(definitions) >= maxCapturedToolDefinitions {
			truncated++
			return
		}
		captured := capturedToolDefinition{
			toolType: toolType, name: name, description: description,
			configurationJSON: mustJSON(configuration),
		}
		if schema != nil {
			captured.inputSchema = mustJSON(schema)
		}
		definitions = append(definitions, captured)
	}
	for _, tool := range config.Tools {
		if tool == nil {
			continue
		}
		for _, fn := range tool.FunctionDeclarations {
			if fn == nil {
				continue
			}
			var schema any
			if fn.Parameters != nil {
				schema = fn.Parameters
			} else {
				schema = fn.ParametersJsonSchema
			}
			capture("function", fn.Name, fn.Description, schema, safeFunctionDefinition{
				Response: fn.Response, ResponseJSONSchema: fn.ResponseJsonSchema, Behavior: fn.Behavior,
			})
		}
		if tool.Retrieval != nil {
			capture("retrieval", "", "", nil, safeRetrieval(tool.Retrieval))
		}
		if tool.ComputerUse != nil {
			capture("computer_use", "", "", nil, safeComputerUseDefinition{
				Environment:                    tool.ComputerUse.Environment,
				ExcludedPredefinedFunctions:    append([]string(nil), tool.ComputerUse.ExcludedPredefinedFunctions...),
				EnablePromptInjectionDetection: tool.ComputerUse.EnablePromptInjectionDetection,
			})
		}
		if tool.FileSearch != nil {
			capture("file_search", "", "", nil, safeFileSearchDefinition{
				FileSearchStoreNames: append([]string(nil), tool.FileSearch.FileSearchStoreNames...),
				TopK:                 tool.FileSearch.TopK, MetadataFilter: tool.FileSearch.MetadataFilter,
			})
		}
		if tool.GoogleSearch != nil {
			capture("google_search", "", "", nil, safeGoogleSearch(tool.GoogleSearch))
		}
		if tool.GoogleMaps != nil {
			capture("google_maps", "", "", nil, safeGoogleMapsDefinition{EnableWidget: tool.GoogleMaps.EnableWidget})
		}
		if tool.CodeExecution != nil {
			capture("code_execution", "", "", nil, struct{}{})
		}
		if tool.EnterpriseWebSearch != nil {
			capture("enterprise_web_search", "", "", nil, safeEnterpriseWebSearchDefinition{
				BlockingConfidence: tool.EnterpriseWebSearch.BlockingConfidence,
				ExcludeDomains:     append([]string(nil), tool.EnterpriseWebSearch.ExcludeDomains...),
			})
		}
		if tool.GoogleSearchRetrieval != nil {
			definition := safeGoogleSearchRetrievalDefinition{}
			if dynamic := tool.GoogleSearchRetrieval.DynamicRetrievalConfig; dynamic != nil {
				definition.DynamicThreshold = dynamic.DynamicThreshold
				definition.Mode = dynamic.Mode
			}
			capture("google_search_retrieval", "", "", nil, definition)
		}
		if tool.ParallelAISearch != nil {
			// APIKey and arbitrary CustomConfigs are intentionally omitted. The
			// latter is an untyped escape hatch that cannot be safely allowlisted.
			capture("parallel_ai_search", "", "", nil, struct{}{})
		}
		if tool.URLContext != nil {
			capture("url_context", "", "", nil, struct{}{})
		}
		if len(tool.MCPServers) > 0 {
			capture("mcp_servers", "", "", nil, safeMCPServers(tool.MCPServers))
		}
	}
	return definitions, truncated
}

func setCapturedToolDefinitions(span trace.Span, definitions []capturedToolDefinition, truncated int) {
	for index, definition := range definitions {
		prefix := fmt.Sprintf("%s%d.", attrs.LLMToolPrefix, index)
		span.SetAttributes(
			attribute.String(prefix+"type", definition.toolType),
			attribute.String(prefix+"configuration", definition.configurationJSON),
		)
		if definition.name != "" {
			span.SetAttributes(attribute.String(prefix+"name", definition.name))
		}
		if definition.description != "" {
			span.SetAttributes(attribute.String(prefix+"description", definition.description))
		}
		if definition.inputSchema != "" {
			span.SetAttributes(attribute.String(prefix+"input_schema", definition.inputSchema))
		}
	}
	if truncated > 0 {
		span.SetAttributes(attribute.Int(attrs.LLMToolsTruncated, truncated))
	}
}

func safeRetrieval(value *genai.Retrieval) safeRetrievalDefinition {
	definition := safeRetrievalDefinition{DisableAttribution: value.DisableAttribution}
	if external := value.ExternalAPI; external != nil {
		definition.ExternalAPI = &safeExternalAPIDefinition{
			APISpec: external.APISpec, Endpoint: safeEndpoint(external.Endpoint),
		}
		if external.SimpleSearchParams != nil {
			definition.ExternalAPI.SimpleSearchParams = &struct{}{}
		}
		if search := external.ElasticSearchParams; search != nil {
			definition.ExternalAPI.ElasticSearchParams = &safeExternalAPISearchDefinition{
				Index: search.Index, NumHits: search.NumHits, SearchTemplate: search.SearchTemplate,
			}
		}
	}
	if search := value.VertexAISearch; search != nil {
		definition.VertexAISearch = &safeVertexAISearchDefinition{
			Datastore: search.Datastore, Engine: search.Engine, Filter: search.Filter, MaxResults: search.MaxResults,
		}
		for _, spec := range search.DataStoreSpecs {
			if spec != nil {
				definition.VertexAISearch.DataStoreSpecs = append(definition.VertexAISearch.DataStoreSpecs,
					safeVertexAISearchDataStoreDefinition{DataStore: spec.DataStore, Filter: spec.Filter})
			}
		}
	}
	if store := value.VertexRAGStore; store != nil {
		definition.VertexRAGStore = safeVertexRAGStore(store)
	}
	return definition
}

func safeVertexRAGStore(store *genai.VertexRAGStore) *safeVertexRAGStoreDefinition {
	definition := &safeVertexRAGStoreDefinition{
		RAGCorpora: append([]string(nil), store.RAGCorpora...), SimilarityTopK: store.SimilarityTopK,
		StoreContext: store.StoreContext, VectorDistanceThreshold: store.VectorDistanceThreshold,
	}
	for _, resource := range store.RAGResources {
		if resource != nil {
			definition.RAGResources = append(definition.RAGResources, safeRAGResourceDefinition{
				RAGCorpus: resource.RAGCorpus, RAGFileIDs: append([]string(nil), resource.RAGFileIDs...),
			})
		}
	}
	if config := store.RAGRetrievalConfig; config != nil {
		definition.RAGRetrievalConfig = &safeRAGRetrievalConfigDefinition{TopK: config.TopK}
		if config.Filter != nil {
			definition.RAGRetrievalConfig.Filter = &safeRAGFilterDefinition{
				MetadataFilter:            config.Filter.MetadataFilter,
				VectorDistanceThreshold:   config.Filter.VectorDistanceThreshold,
				VectorSimilarityThreshold: config.Filter.VectorSimilarityThreshold,
			}
		}
		if config.HybridSearch != nil {
			definition.RAGRetrievalConfig.HybridSearch = &safeRAGHybridSearchDefinition{Alpha: config.HybridSearch.Alpha}
		}
		if config.Ranking != nil {
			ranking := &safeRAGRankingDefinition{}
			if config.Ranking.LlmRanker != nil {
				ranking.LlmRanker = &safeRAGRankerDefinition{ModelName: config.Ranking.LlmRanker.ModelName}
			}
			if config.Ranking.RankService != nil {
				ranking.RankService = &safeRAGRankerDefinition{ModelName: config.Ranking.RankService.ModelName}
			}
			definition.RAGRetrievalConfig.Ranking = ranking
		}
	}
	return definition
}

func safeGoogleSearch(value *genai.GoogleSearch) safeGoogleSearchDefinition {
	definition := safeGoogleSearchDefinition{
		BlockingConfidence: value.BlockingConfidence,
		ExcludeDomains:     append([]string(nil), value.ExcludeDomains...),
	}
	if value.SearchTypes != nil {
		definition.SearchTypes = &safeSearchTypesDefinition{}
		if value.SearchTypes.WebSearch != nil {
			definition.SearchTypes.WebSearch = &struct{}{}
		}
		if value.SearchTypes.ImageSearch != nil {
			definition.SearchTypes.ImageSearch = &struct{}{}
		}
	}
	if interval := value.TimeRangeFilter; interval != nil {
		definition.TimeRangeFilter = &safeIntervalDefinition{}
		if !interval.StartTime.IsZero() {
			definition.TimeRangeFilter.StartTime = interval.StartTime.UTC().Format(time.RFC3339Nano)
		}
		if !interval.EndTime.IsZero() {
			definition.TimeRangeFilter.EndTime = interval.EndTime.UTC().Format(time.RFC3339Nano)
		}
	}
	return definition
}

func safeMCPServers(servers []*genai.MCPServer) []safeMCPServerDefinition {
	definitions := make([]safeMCPServerDefinition, 0, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		definition := safeMCPServerDefinition{Name: server.Name}
		if transport := server.StreamableHTTPTransport; transport != nil {
			definition.StreamableHTTPTransport = &safeStreamableHTTPTransportDefinition{
				URL: safeEndpoint(transport.URL), TerminateOnClose: transport.TerminateOnClose,
			}
			if transport.SseReadTimeout != 0 {
				definition.StreamableHTTPTransport.SSEReadTimeout = transport.SseReadTimeout.String()
			}
			if transport.Timeout != 0 {
				definition.StreamableHTTPTransport.Timeout = transport.Timeout.String()
			}
		}
		definitions = append(definitions, definition)
	}
	return definitions
}

func safeEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
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
	choiceIndex          int
	name                 string
	id                   string
	arguments            map[string]any
	partialContinuations map[string]bool
}

type accumulatedChoice struct {
	role              string
	text              string
	thinking          string
	finish            genai.FinishReason
	toolCalls         map[int]*accumulatedToolCall
	toolPositionsByID map[string]int
	activeIDlessCalls map[int]int
	nextToolPosition  int
	media             map[string]capturedMedia
}

type responseAccumulator struct {
	choices           map[int]*accumulatedChoice
	usage             *genai.GenerateContentResponseUsageMetadata
	responseID        string
	chunkCount        int
	mediaCount        int
	mediaPayloadBytes int
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
				activeIDlessCalls: make(map[int]int),
				media:             make(map[string]capturedMedia),
			}
			a.choices[index] = choice
		}
		if cand.Content != nil {
			if cand.Content.Role != "" && cand.Content.Role != "model" {
				choice.role = cand.Content.Role
			}
			functionPosition := 0
			nextActiveIDlessCalls := make(map[int]int)
			for _, part := range cand.Content.Parts {
				if part == nil {
					continue
				}
				for _, media := range partMedia(part, "output") {
					key := media.key()
					if _, exists := choice.media[key]; exists || a.mediaCount >= maxCapturedMediaReferences {
						continue
					}
					if len(media.payload) > 0 && a.mediaPayloadBytes+len(media.payload) > internalmedia.UploadLimit {
						media.payload = nil
						media.state = "failed"
						media.safePreview = "upload failed: memory_limit"
					}
					a.mediaPayloadBytes += len(media.payload)
					a.mediaCount++
					choice.media[key] = media
				}
				switch {
				case part.Thought && part.Text != "":
					choice.thinking += part.Text
				case part.Text != "":
					choice.text += part.Text
				case part.FunctionCall != nil:
					functionCall := part.FunctionCall
					toolPosition, linked := choice.toolPositionsByID[functionCall.ID]
					if functionCall.ID == "" {
						toolPosition, linked = choice.activeIDlessCalls[functionPosition]
					} else if !linked {
						// Vertex may introduce an ID after an ID-less partial. Link it
						// to the continued call at the same choice-local position, but
						// never merge two different provider-identified calls.
						if activePosition, active := choice.activeIDlessCalls[functionPosition]; active {
							activeCall := choice.toolCalls[activePosition]
							if activeCall != nil && activeCall.id == "" {
								toolPosition, linked = activePosition, true
							}
						}
					}
					if !linked {
						toolPosition = choice.nextToolPosition
						choice.nextToolPosition++
					}
					if functionCall.ID != "" {
						choice.toolPositionsByID[functionCall.ID] = toolPosition
					}
					call := choice.toolCalls[toolPosition]
					if call == nil {
						call = &accumulatedToolCall{
							choiceIndex:          index,
							arguments:            make(map[string]any),
							partialContinuations: make(map[string]bool),
						}
						choice.toolCalls[toolPosition] = call
					}
					if functionCall.Name != "" {
						call.name = functionCall.Name
					}
					if functionCall.ID != "" {
						call.id = functionCall.ID
					}
					for key, value := range functionCall.Args {
						call.arguments[key] = value
					}
					for _, partial := range functionCall.PartialArgs {
						call.applyPartialArg(partial)
					}
					if functionCall.WillContinue != nil && *functionCall.WillContinue && call.id == "" {
						// Provider positions are relative to the calls present in each
						// chunk. Compact the surviving calls so a later B-only chunk
						// still links to B after A completed in the preceding chunk.
						// Calls with provider IDs must only be linked by those IDs.
						nextActiveIDlessCalls[len(nextActiveIDlessCalls)] = toolPosition
					}
					functionPosition++
				}
			}
			if functionPosition > 0 {
				choice.activeIDlessCalls = nextActiveIDlessCalls
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

type jsonPathSegment struct {
	key   string
	index int
	isKey bool
}

const (
	maxJSONPathLength     = 4096
	maxJSONPathSegments   = 64
	maxJSONPathArrayIndex = 1024
)

func (c *accumulatedToolCall) applyPartialArg(partial *genai.PartialArg) {
	if partial == nil {
		return
	}
	segments, ok := parseJSONPath(partial.JsonPath)
	if !ok || len(segments) == 0 || !segments[0].isKey {
		return
	}
	value, isString := partialArgValue(partial)
	if isString && c.partialContinuations[partial.JsonPath] {
		if existing, found := jsonPathValue(c.arguments, segments); found {
			if text, textOK := existing.(string); textOK {
				value = text + value.(string)
			}
		}
	}
	assignJSONPath(c.arguments, segments, value)
	if partial.WillContinue != nil && *partial.WillContinue {
		c.partialContinuations[partial.JsonPath] = true
	} else {
		delete(c.partialContinuations, partial.JsonPath)
	}
}

func partialArgValue(partial *genai.PartialArg) (any, bool) {
	switch {
	case partial.BoolValue != nil:
		return *partial.BoolValue, false
	case partial.NumberValue != nil:
		return *partial.NumberValue, false
	case partial.NULLValue != "":
		return nil, false
	default:
		return partial.StringValue, true
	}
}

// parseJSONPath accepts the provider's child-selector subset of RFC 9535:
// member-name shorthand, quoted names, and non-negative array indexes. Limits
// keep malformed telemetry from causing unbounded parsing or array allocation;
// unsupported or over-limit selectors are rejected atomically.
func parseJSONPath(path string) ([]jsonPathSegment, bool) {
	if path == "" || len(path) > maxJSONPathLength || path[0] != '$' || !utf8.ValidString(path) {
		return nil, false
	}
	segments := make([]jsonPathSegment, 0, 4)
	for index := 1; index < len(path); {
		if len(segments) >= maxJSONPathSegments {
			return nil, false
		}
		switch path[index] {
		case '.':
			index++
			key, next, ok := parseJSONPathMemberName(path, index)
			if !ok {
				return nil, false
			}
			segments = append(segments, jsonPathSegment{key: key, isKey: true})
			index = next
		case '[':
			index++
			index = skipJSONPathWhitespace(path, index)
			if index >= len(path) {
				return nil, false
			}
			if path[index] == '\'' || path[index] == '"' {
				key, next, ok := parseJSONPathQuotedName(path, index)
				if !ok {
					return nil, false
				}
				index = skipJSONPathWhitespace(path, next)
				if index >= len(path) || path[index] != ']' {
					return nil, false
				}
				segments = append(segments, jsonPathSegment{key: key, isKey: true})
				index++
			} else {
				start := index
				for index < len(path) && path[index] >= '0' && path[index] <= '9' {
					index++
				}
				if start == index || (path[start] == '0' && index-start > 1) {
					return nil, false
				}
				position, err := strconv.Atoi(path[start:index])
				if err != nil || position > maxJSONPathArrayIndex {
					return nil, false
				}
				index = skipJSONPathWhitespace(path, index)
				if index >= len(path) || path[index] != ']' {
					return nil, false
				}
				segments = append(segments, jsonPathSegment{index: position})
				index++
			}
		default:
			return nil, false
		}
	}
	return segments, true
}

func parseJSONPathMemberName(path string, start int) (string, int, bool) {
	index := start
	for index < len(path) && path[index] != '.' && path[index] != '[' {
		value, size := utf8.DecodeRuneInString(path[index:])
		if value == utf8.RuneError && size == 1 {
			return "", 0, false
		}
		valid := value == '_' || value >= 0x80 || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
		if index > start {
			valid = valid || value >= '0' && value <= '9'
		}
		if !valid {
			return "", 0, false
		}
		index += size
	}
	if start == index {
		return "", 0, false
	}
	return path[start:index], index, true
}

func skipJSONPathWhitespace(path string, index int) int {
	for index < len(path) {
		switch path[index] {
		case ' ', '\t', '\n', '\r':
			index++
		default:
			return index
		}
	}
	return index
}

// parseJSONPathQuotedName parses the RFC 9535 single- and double-quoted name
// selector forms. It converts the single-quoted form to a JSON string so the
// standard decoder validates escape syntax and decodes Unicode sequences.
func parseJSONPathQuotedName(path string, start int) (string, int, bool) {
	quote := path[start]
	var encoded strings.Builder
	encoded.Grow(16)
	encoded.WriteByte('"')
	for index := start + 1; index < len(path); index++ {
		char := path[index]
		if char == quote {
			encoded.WriteByte('"')
			var key string
			if err := json.Unmarshal([]byte(encoded.String()), &key); err != nil {
				return "", 0, false
			}
			return key, index + 1, true
		}
		if char < 0x20 {
			return "", 0, false
		}
		if char == '"' {
			// A double quote is ordinary content only inside a single-quoted
			// selector; escape it in the JSON representation.
			encoded.WriteString(`\"`)
			continue
		}
		if char != '\\' {
			encoded.WriteByte(char)
			continue
		}
		if index+1 >= len(path) {
			return "", 0, false
		}
		escaped := path[index+1]
		if escaped == quote {
			if quote == '"' {
				encoded.WriteString(`\"`)
			} else {
				encoded.WriteByte('\'')
			}
			index++
			continue
		}
		// Escaping the other quote is not part of the RFC grammar. The
		// unescaped form is already valid in this quoting style.
		if escaped == '\'' || escaped == '"' {
			return "", 0, false
		}
		if escaped == 'u' {
			next, ok := parseJSONPathUnicodeEscape(path, index)
			if !ok {
				return "", 0, false
			}
			encoded.WriteString(path[index:next])
			index = next - 1
			continue
		}
		encoded.WriteByte('\\')
		encoded.WriteByte(escaped)
		index++
	}
	return "", 0, false
}

// parseJSONPathUnicodeEscape validates RFC 9535's hexchar production. Go's
// encoding/json accepts lone or mismatched UTF-16 surrogates by replacing them
// with U+FFFD, while JSONPath requires a high surrogate to be immediately
// followed by a low surrogate and forbids a low surrogate on its own.
func parseJSONPathUnicodeEscape(path string, start int) (int, bool) {
	value, next, ok := parseJSONPathCodeUnit(path, start)
	if !ok {
		return 0, false
	}
	if value >= 0xDC00 && value <= 0xDFFF {
		return 0, false
	}
	if value < 0xD800 || value > 0xDBFF {
		return next, true
	}
	low, end, ok := parseJSONPathCodeUnit(path, next)
	if !ok || low < 0xDC00 || low > 0xDFFF {
		return 0, false
	}
	return end, true
}

func parseJSONPathCodeUnit(path string, start int) (uint16, int, bool) {
	if start+6 > len(path) || path[start] != '\\' || path[start+1] != 'u' {
		return 0, 0, false
	}
	var value uint16
	for _, char := range []byte(path[start+2 : start+6]) {
		value *= 16
		switch {
		case char >= '0' && char <= '9':
			value += uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value += uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value += uint16(char-'A') + 10
		default:
			return 0, 0, false
		}
	}
	return value, start + 6, true
}

func assignJSONPath(root map[string]any, segments []jsonPathSegment, value any) {
	if len(segments) == 0 || !segments[0].isKey {
		return
	}
	root[segments[0].key] = assignJSONPathValue(root[segments[0].key], segments[1:], value)
}

func assignJSONPathValue(current any, segments []jsonPathSegment, value any) any {
	if len(segments) == 0 {
		return value
	}
	segment := segments[0]
	if segment.isKey {
		object, ok := current.(map[string]any)
		if !ok {
			object = make(map[string]any)
		}
		object[segment.key] = assignJSONPathValue(object[segment.key], segments[1:], value)
		return object
	}
	array, ok := current.([]any)
	if !ok {
		array = nil
	}
	for len(array) <= segment.index {
		array = append(array, nil)
	}
	array[segment.index] = assignJSONPathValue(array[segment.index], segments[1:], value)
	return array
}

func jsonPathValue(root map[string]any, segments []jsonPathSegment) (any, bool) {
	var current any = root
	for _, segment := range segments {
		if segment.isKey {
			object, ok := current.(map[string]any)
			if !ok {
				return nil, false
			}
			current, ok = object[segment.key]
			if !ok {
				return nil, false
			}
			continue
		}
		array, ok := current.([]any)
		if !ok || segment.index >= len(array) {
			return nil, false
		}
		current = array[segment.index]
	}
	return current, true
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
	// Provider parts can contain raw inline bytes or signed remote locators.
	// Canonical media attributes carry those values; the generic content field
	// receives only a sanitized structural clone so it cannot duplicate them.
	safeParts := make([]*genai.Part, 0, len(c.Parts))
	for _, part := range c.Parts {
		if part == nil {
			continue
		}
		clone := *part
		if part.InlineData != nil && len(part.InlineData.Data) > internalmedia.InlineLimit {
			clone.InlineData = nil
		}
		if part.FileData != nil {
			file := *part.FileData
			file.FileURI = sanitizedMediaReference(file.FileURI)
			clone.FileData = &file
		}
		safeParts = append(safeParts, &clone)
	}
	return mustJSON(safeParts)
}

type capturedMedia struct {
	id          string
	kind        string
	source      string
	mimeType    string
	byteLength  int
	sha256      string
	reference   string
	purpose     string
	state       string
	safePreview string
	payload     []byte
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

func canonicalMediaMIME(mimeType string) string {
	mimeType = strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	switch mimeType {
	case "image/jpg":
		return "image/jpeg"
	case "audio/mp3":
		return "audio/mpeg"
	default:
		return mimeType
	}
}

func partMedia(part *genai.Part, purpose string) []capturedMedia {
	if part == nil {
		return nil
	}
	media := make([]capturedMedia, 0, 2)
	if part.InlineData != nil {
		digest := sha256.Sum256(part.InlineData.Data)
		mimeType := canonicalMediaMIME(part.InlineData.MIMEType)
		captured := capturedMedia{
			id: fmt.Sprintf("nl_media_%x", digest[:12]), kind: mediaKind(mimeType),
			source: "inline", mimeType: mimeType, byteLength: len(part.InlineData.Data),
			sha256: fmt.Sprintf("%x", digest), purpose: purpose, state: "inline",
		}
		if captured.mimeType == "" {
			captured.mimeType = "application/octet-stream"
		}
		switch {
		case len(part.InlineData.Data) > internalmedia.UploadLimit:
			captured.state = "failed"
			captured.safePreview = "upload failed: payload_too_large"
		case len(part.InlineData.Data) > internalmedia.InlineLimit:
			captured.state = "pending-upload"
			captured.payload = part.InlineData.Data
		}
		media = append(media, captured)
	}
	if part.FileData != nil && part.FileData.FileURI != "" {
		reference := sanitizedMediaReference(part.FileData.FileURI)
		digest := sha256.Sum256([]byte(reference))
		source := "provider"
		mimeType := canonicalMediaMIME(part.FileData.MIMEType)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		if strings.HasPrefix(part.FileData.FileURI, "http://") || strings.HasPrefix(part.FileData.FileURI, "https://") {
			source = "url"
		}
		media = append(media, capturedMedia{
			id: fmt.Sprintf("nl_media_%x", digest[:12]), kind: mediaKind(mimeType),
			source: source, mimeType: mimeType, reference: reference,
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
	payloadBytes := 0
	seen := make(map[string]struct{})
	for _, part := range content.Parts {
		for _, media := range partMedia(part, purpose) {
			if index >= maxCapturedMediaReferences {
				return
			}
			if _, exists := seen[media.key()]; exists {
				continue
			}
			if len(media.payload) > 0 && payloadBytes+len(media.payload) > internalmedia.UploadLimit {
				media.payload = nil
				media.state = "failed"
				media.safePreview = "upload failed: memory_limit"
			}
			payloadBytes += len(media.payload)
			seen[media.key()] = struct{}{}
			setMediaAttributes(span, fmt.Sprintf("%s.media.%d.", prefix, index), media)
			index++
		}
	}
}

func setMediaAttributes(span trace.Span, prefix string, media capturedMedia) {
	values := make([]attribute.KeyValue, 0, 10)
	values = append(values,
		attribute.String(prefix+"id", media.id), attribute.String(prefix+"type", media.kind),
		attribute.String(prefix+"source", media.source), attribute.String(prefix+"mime_type", media.mimeType),
		attribute.String(prefix+"purpose", media.purpose), attribute.String(prefix+"state", media.state),
	)
	if media.byteLength > 0 {
		values = append(values, attribute.Int(prefix+"byte_length", media.byteLength))
	}
	if media.sha256 != "" {
		values = append(values, attribute.String(prefix+"sha256", media.sha256))
	}
	if media.reference != "" {
		values = append(values, attribute.String(prefix+"reference", media.reference))
	}
	if media.safePreview != "" {
		values = append(values, attribute.String(prefix+"safe_preview", media.safePreview))
	}
	if len(media.payload) > 0 {
		// Append private content last. If the OTel attribute limit drops it, the
		// retained pending reference is turned into explicit failure metadata.
		values = append(values, internalmedia.PayloadAttribute(prefix, media.payload))
	}
	span.SetAttributes(values...)
}

func sanitizedMediaReference(reference string) string {
	parsed, err := url.Parse(reference)
	if err != nil {
		safe := strings.SplitN(strings.SplitN(reference, "#", 2)[0], "?", 2)[0]
		if scheme := strings.Index(safe, "://"); scheme >= 0 {
			remainder := safe[scheme+3:]
			if at := strings.LastIndex(remainder, "@"); at >= 0 {
				safe = safe[:scheme+3] + remainder[at+1:]
			}
		}
		return safe
	}
	if parsed.IsAbs() && (parsed.Host != "" || strings.HasPrefix(reference, "//")) {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	return strings.SplitN(strings.SplitN(reference, "#", 2)[0], "?", 2)[0]
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
