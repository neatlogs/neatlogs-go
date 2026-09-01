package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	neatlogs "github.com/neatlogs/neatlogs-go"
	nlgenai "github.com/neatlogs/neatlogs-go/contrib/genai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/genai"
)

type scenario func(context.Context, *nlgenai.GenAIModels) (map[string]any, error)

var emailPattern = regexp.MustCompile(`(?i)[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}`)

func safeMask(_ context.Context, snapshot neatlogs.SpanData) (*neatlogs.SpanData, error) {
	credentialKeys := map[string]bool{
		"api_key": true, "apikey": true, "authorization": true,
		"access_token": true, "refresh_token": true, "password": true, "secret": true,
	}
	for i, kv := range snapshot.Attributes {
		key := strings.ToLower(string(kv.Key))
		if credentialKeys[key] {
			snapshot.Attributes[i] = attribute.String(string(kv.Key), "[REDACTED]")
			continue
		}
		if kv.Value.Type() == attribute.STRING {
			value := emailPattern.ReplaceAllString(kv.Value.AsString(), "[REDACTED_EMAIL]")
			var structured any
			if json.Unmarshal([]byte(value), &structured) == nil {
				if encoded, err := json.Marshal(redactJSON(structured)); err == nil {
					value = string(encoded)
				}
			}
			snapshot.Attributes[i] = attribute.String(string(kv.Key), value)
		}
	}
	return &snapshot, nil
}

func redactJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "api_key", "apikey", "authorization", "access_token", "refresh_token", "password", "secret":
				out[key] = "[REDACTED]"
			default:
				out[key] = redactJSON(child)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redactJSON(child)
		}
		return out
	case string:
		return emailPattern.ReplaceAllString(typed, "[REDACTED_EMAIL]")
	default:
		return value
	}
}

func generatorChannel(ctx context.Context, _ *nlgenai.GenAIModels) (map[string]any, error) {
	rootCtx, root, endRoot := neatlogs.Trace(ctx, "go-generator-channel")
	defer endRoot()

	type generated struct{ Index int }
	items := make(chan generated)
	go func() {
		defer close(items)
		agentCtx, _, endAgent := neatlogs.StartSpan(rootCtx, "generator-agent", "agent")
		defer endAgent()
		for index := 1; index <= 5; index++ {
			_, toolSpan, endTool := neatlogs.StartSpan(
				agentCtx,
				fmt.Sprintf("generate-item-%d", index),
				"tool",
				attribute.Int("generator.index", index),
			)
			toolSpan.SetAttributes(attribute.String("neatlogs.output.value", fmt.Sprintf(`{"index":%d}`, index)))
			endTool()
			items <- generated{Index: index}
		}
	}()

	sum := 0
	for item := range items {
		sum += item.Index
	}
	if err := neatlogs.SetTraceOutput(root, map[string]any{"items": 5, "sum": sum}); err != nil {
		return nil, err
	}
	return map[string]any{"expected_spans": 7, "items": 5, "sum": sum}, nil
}

func streamingComplete(ctx context.Context, models *nlgenai.GenAIModels) (map[string]any, error) {
	rootCtx, root, endRoot := neatlogs.Trace(ctx, "go-streaming-complete")
	defer endRoot()
	chunks := 0
	var text strings.Builder
	for response, err := range models.GenerateContentStream(
		rootCtx,
		modelName(),
		[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "Return exactly three short words about observability."}}}},
		&genai.GenerateContentConfig{MaxOutputTokens: 64},
	) {
		if err != nil {
			return nil, err
		}
		chunks++
		for _, candidate := range response.Candidates {
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
				text.WriteString(part.Text)
			}
		}
	}
	if chunks == 0 {
		return nil, errors.New("Gemini stream returned zero chunks")
	}
	if err := neatlogs.SetTraceOutput(root, map[string]any{"chunks": chunks, "text": text.String()}); err != nil {
		return nil, err
	}
	return map[string]any{"expected_spans": 2, "chunks": chunks}, nil
}

func streamingEarlyStop(ctx context.Context, models *nlgenai.GenAIModels) (map[string]any, error) {
	rootCtx, root, endRoot := neatlogs.Trace(ctx, "go-streaming-early-stop")
	defer endRoot()
	chunks := 0
	for _, err := range models.GenerateContentStream(
		rootCtx,
		modelName(),
		[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "Count slowly from one to ten, one number per chunk."}}}},
		&genai.GenerateContentConfig{MaxOutputTokens: 128},
	) {
		if err != nil {
			return nil, err
		}
		chunks++
		break
	}
	if chunks != 1 {
		return nil, fmt.Errorf("early-stop chunks = %d, want 1", chunks)
	}
	if err := neatlogs.SetTraceOutput(root, map[string]any{"consumer_stopped_after_chunks": chunks}); err != nil {
		return nil, err
	}
	return map[string]any{"expected_spans": 2, "expected_stream_state": "consumer_cancelled", "chunks": chunks}, nil
}

func asyncFanout(ctx context.Context, _ *nlgenai.GenAIModels) (map[string]any, error) {
	rootCtx, root, endRoot := neatlogs.Trace(ctx, "go-async-goroutine-fanout")
	defer endRoot()

	const workers = 6
	var wait sync.WaitGroup
	errorsCh := make(chan error, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			agentCtx, _, endAgent := neatlogs.StartSpan(rootCtx, fmt.Sprintf("worker-%d", worker), "agent")
			defer endAgent()
			_, llm := neatlogs.StartLLMSpan(agentCtx, neatlogs.LLMCallOptions{
				Provider: "phase5_fake", Model: "deterministic-go-model",
				Messages: []neatlogs.LLMMessage{{Role: "user", Content: fmt.Sprintf("worker %d", worker)}},
			})
			llm.SetOutputMessage("assistant", fmt.Sprintf("done %d", worker))
			llm.SetUsage(10+worker, 5, 15+worker)
			llm.SetFinishReason("stop")
			llm.End()
		}(index)
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			return nil, err
		}
	}
	if err := neatlogs.SetTraceOutput(root, map[string]any{"workers": workers}); err != nil {
		return nil, err
	}
	return map[string]any{"expected_spans": 13, "agents": workers, "llm_calls": workers}, nil
}

func retryThenSuccess(ctx context.Context, _ *nlgenai.GenAIModels) (map[string]any, error) {
	rootCtx, root, endRoot := neatlogs.Trace(ctx, "go-retry-then-success")
	defer endRoot()

	for attempt := 1; attempt <= 3; attempt++ {
		attemptCtx, attemptSpan, endAttempt := neatlogs.StartSpan(
			rootCtx,
			fmt.Sprintf("retry-attempt-%d", attempt),
			"agent",
			attribute.Int("retry.attempt", attempt),
			attribute.String("retry.idempotency_key", "phase5-stable-operation"),
		)
		_, llm := neatlogs.StartLLMSpan(attemptCtx, neatlogs.LLMCallOptions{
			Provider: "phase5_fake", Model: "retry-model",
			Messages: []neatlogs.LLMMessage{{Role: "user", Content: "retry safely"}},
		})
		if attempt < 3 {
			err := fmt.Errorf("transient provider failure attempt %d", attempt)
			llm.SetError(err)
			attemptSpan.RecordError(err)
			attemptSpan.SetStatus(codes.Error, err.Error())
		} else {
			llm.SetOutputMessage("assistant", "retry succeeded")
			llm.SetUsage(30, 10, 40)
			llm.SetFinishReason("stop")
		}
		llm.End()
		endAttempt()
		if attempt < 3 {
			time.Sleep(time.Duration(attempt*50) * time.Millisecond)
		}
	}
	if err := neatlogs.SetTraceOutput(root, map[string]any{"attempts": 3, "result": "success"}); err != nil {
		return nil, err
	}
	return map[string]any{"expected_spans": 7, "attempts": 3, "expected_error_spans": 4}, nil
}

func numericMasking(ctx context.Context, _ *nlgenai.GenAIModels) (map[string]any, error) {
	rootCtx, root, endRoot := neatlogs.Trace(ctx, "go-numeric-token-and-pii")
	defer endRoot()
	llmCtx, llm := neatlogs.StartLLMSpan(rootCtx, neatlogs.LLMCallOptions{
		Provider: "phase5_fake", Model: "numeric-contract-model",
		Messages: []neatlogs.LLMMessage{{Role: "user", Content: "Email go.qa@example.com and keep token counts numeric"}},
	})
	_, tool, endTool := neatlogs.StartSpan(
		llmCtx,
		"credential-bearing-tool",
		"tool",
		attribute.String("access_token", "must-not-leave-process"),
		attribute.String("neatlogs.output.value", `{"email":"go.qa@example.com","prompt_tokens":120,"completion_tokens":50,"total_tokens":170}`),
	)
	endTool()
	_ = tool
	llm.SetOutputMessage("assistant", "Email go.qa@example.com was handled")
	llm.SetUsage(120, 50, 170)
	llm.SetFinishReason("stop")
	llm.End()
	if err := neatlogs.SetTraceOutput(root, map[string]any{"prompt_tokens": 120, "completion_tokens": 50, "total_tokens": 170}); err != nil {
		return nil, err
	}
	return map[string]any{"expected_spans": 3, "expected_tokens": []int{120, 50, 170}}, nil
}

func multiBatchFlush(ctx context.Context, _ *nlgenai.GenAIModels) (map[string]any, error) {
	rootCtx, root, endRoot := neatlogs.Trace(ctx, "go-multi-batch-flush")
	defer endRoot()
	for group := 0; group < 3; group++ {
		for child := 0; child < 8; child++ {
			_, _, end := neatlogs.StartSpan(rootCtx, fmt.Sprintf("group-%d-child-%d", group, child), "tool")
			end()
		}
		if err := neatlogs.Flush(rootCtx); err != nil {
			return nil, err
		}
	}
	if err := neatlogs.SetTraceOutput(root, map[string]any{"groups": 3, "children": 24}); err != nil {
		return nil, err
	}
	return map[string]any{"expected_spans": 25, "flush_boundaries": 3}, nil
}

var scenarios = map[string]scenario{
	"generator-channel":    generatorChannel,
	"streaming-complete":   streamingComplete,
	"streaming-early-stop": streamingEarlyStop,
	"async-fanout":         asyncFanout,
	"retry":                retryThenSuccess,
	"numeric-pii":          numericMasking,
	"multi-batch":          multiBatchFlush,
}

func modelName() string {
	if value := strings.TrimSpace(os.Getenv("GEMINI_MODEL")); value != "" {
		return value
	}
	return "gemini-2.5-flash"
}

func initModels(ctx context.Context) (*nlgenai.GenAIModels, error) {
	key := strings.TrimSpace(os.Getenv("GOOGLE_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI})
	if err != nil {
		return nil, err
	}
	return nlgenai.WrapGenAI(client), nil
}

func runOne(ctx context.Context, name string, fn scenario, models *nlgenai.GenAIModels, runID string) error {
	disableExport, _ := strconv.ParseBool(os.Getenv("NEATLOGS_LOCAL_ONLY"))
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{
		APIKey:        strings.TrimSpace(os.Getenv("NEATLOGS_API_KEY")),
		Endpoint:      strings.TrimSpace(os.Getenv("NEATLOGS_ENDPOINT")),
		WorkflowName:  fmt.Sprintf("phase5-%s-%s", name, runID),
		Debug:         os.Getenv("NEATLOGS_DEBUG") == "true",
		DisableExport: disableExport,
		Mask:          safeMask,
	})
	if err != nil {
		return err
	}
	result, scenarioErr := fn(ctx, models)
	flushCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	flushErr := neatlogs.Flush(flushCtx)
	shutdownErr := shutdown(flushCtx)
	fmt.Printf("%s\n", mustJSON(map[string]any{
		"scenario": name, "run_id": runID, "workflow": fmt.Sprintf("phase5-%s-%s", name, runID),
		"result": result, "scenario_error": errorString(scenarioErr), "flush_error": errorString(flushErr), "shutdown_error": errorString(shutdownErr),
		"export_health": neatlogs.GetDeliveryDiagnostics(ctx),
	}))
	return errors.Join(scenarioErr, flushErr, shutdownErr)
}

func mustJSON(value any) string {
	bytes, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf(`{"marshal_error":%q}`, err.Error())
	}
	return string(bytes)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func main() {
	ctx := context.Background()
	runID := strings.TrimSpace(os.Getenv("PHASE5_RUN_ID"))
	if runID == "" {
		runID = fmt.Sprintf("%d", time.Now().UnixMilli())
	}
	selected := "all"
	if len(os.Args) > 1 {
		selected = os.Args[1]
	}
	var models *nlgenai.GenAIModels
	if selected == "all" || selected == "streaming-complete" || selected == "streaming-early-stop" {
		var err error
		models, err = initModels(ctx)
		if err != nil {
			log.Fatalf("Gemini client: %v", err)
		}
	}
	if selected != "all" {
		fn, ok := scenarios[selected]
		if !ok {
			log.Fatalf("unknown scenario %q", selected)
		}
		if err := runOne(ctx, selected, fn, models, runID); err != nil {
			log.Fatal(err)
		}
		return
	}
	order := []string{"generator-channel", "streaming-complete", "streaming-early-stop", "async-fanout", "retry", "numeric-pii", "multi-batch"}
	for _, name := range order {
		if err := runOne(ctx, name, scenarios[name], models, runID); err != nil {
			log.Fatal(err)
		}
	}
}
