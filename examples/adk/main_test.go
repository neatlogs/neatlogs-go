package main

import (
	"context"
	"iter"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
)

// fakeModel implements model.LLM with a canned response, so the test runs a
// real ADK agent end-to-end without any network or API key.
type fakeModel struct{ name string }

func (m fakeModel) Name() string { return m.name }

func (m fakeModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText("sea breathes at first light", genai.RoleModel),
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     11,
				CandidatesTokenCount: 7,
				TotalTokenCount:      18,
			},
		}, nil)
	}
}

// TestADKPassthrough proves the core passthrough claim: ADK auto-instruments
// via the GLOBAL OpenTelemetry TracerProvider, ADK binds its tracer at package
// init (before Init runs), yet because OTel's global provider delegates to the
// provider Init installs later, ADK's spans still reach our exporter — already
// normalized into the neatlogs.* namespace. No ADK call is wrapped.
func TestADKPassthrough(t *testing.T) {
	ctx := context.Background()

	// In-memory exporter stands in for the OTLP/HTTP transport.
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx,
		neatlogs.Config{WorkflowName: "adk-test"},
		neatlogs.WithExporter(sink),
	)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer shutdown(ctx)

	a, err := llmagent.New(llmagent.Config{
		Name:        "haiku_agent",
		Model:       fakeModel{name: "gemini-2.5-flash"},
		Description: "Writes short haikus.",
		Instruction: "Write a haiku.",
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	const appName, userID, sessionID = "adk-test", "user-1", "session-1"
	sessions := session.InMemoryService()
	if _, err := sessions.Create(ctx, &session.CreateRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	}); err != nil {
		t.Fatalf("session: %v", err)
	}

	r, err := runner.New(runner.Config{
		AppName:        appName,
		Agent:          a,
		SessionService: sessions,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}

	msg := genai.NewContentFromText("the sea at dawn", genai.RoleUser)
	for _, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	spans := sink.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans captured — ADK did not flow through the global provider")
	}

	// Index spans by name and collect every neatlogs.* attribute key seen.
	names := map[string]bool{}
	neatlogsKeys := map[string]bool{}
	var sawLLMKind, sawModelName, sawInputTokens bool
	for _, s := range spans {
		names[s.Name] = true
		for _, kv := range s.Attributes {
			key := string(kv.Key)
			if len(key) >= 9 && key[:9] == "neatlogs." {
				neatlogsKeys[key] = true
			}
			switch key {
			case "neatlogs.span.kind":
				if kv.Value.AsString() == "llm" {
					sawLLMKind = true
				}
			case "neatlogs.llm.model_name":
				sawModelName = true
			case "neatlogs.llm.token_count.prompt":
				sawInputTokens = true
			}
		}
	}

	// ADK emits a generate_content span; its gen_ai.* attributes must have been
	// normalized to neatlogs.* by our exporter.
	if !hasPrefix(names, "generate_content") {
		t.Errorf("expected a generate_content span; got span names %v", keys(names))
	}
	if len(neatlogsKeys) == 0 {
		t.Fatalf("no neatlogs.* attributes — normalization did not run; keys present: %v", names)
	}
	if !sawLLMKind {
		t.Errorf("expected neatlogs.span.kind=llm on the LLM span; neatlogs keys seen: %v", keys(neatlogsKeys))
	}
	if !sawModelName {
		t.Errorf("expected neatlogs.llm.model_name; neatlogs keys seen: %v", keys(neatlogsKeys))
	}
	if !sawInputTokens {
		t.Errorf("expected neatlogs.llm.token_count.prompt; neatlogs keys seen: %v", keys(neatlogsKeys))
	}

	t.Logf("captured %d spans; neatlogs.* attribute keys: %v", len(spans), keys(neatlogsKeys))
}

func hasPrefix(set map[string]bool, prefix string) bool {
	for k := range set {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
