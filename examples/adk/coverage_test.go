package main

// Comprehensive ADK coverage against the REAL Gemini API. These tests confirm
// that Neatlogs passive passthrough captures and normalizes ADK spans across
// every execution path ADK-Go supports via runner.Run:
//
//   - non-streaming (StreamingModeNone)
//   - SSE streaming (StreamingModeSSE)
//   - tool calls (functiontool)            → execute_tool spans
//   - sequential workflow agents           → multiple invoke_agent spans
//   - A2A (remote agent over HTTP)          → remote agent invocation
//   - concurrent runs (goroutine safety)
//
// They require GOOGLE_API_KEY (real Gemini calls) and are skipped without it.
// Audio/bidi (RunLive) is intentionally out of scope.
//
// Each test asserts on the neatlogs.* attributes our exporter produces, since
// that is the contract the backend consumes. Span kinds present depend on what
// the model chooses to do (e.g. whether it calls a tool), so assertions on
// model-driven spans are tolerant; the always-present spans are asserted hard.

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/remoteagent/v2"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/model/gemini"
	"google.golang.org/adk/runner"
	adka2a "google.golang.org/adk/server/adka2a/v2"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	neatlogs "go.neatlogs.com"
	nladk "go.neatlogs.com/contrib/adk"
)

const testModel = "gemini-2.5-flash"

// swapExporter is a SpanExporter whose downstream in-memory sink can be swapped
// between subtests. This is essential because ADK (like all OTel libraries)
// binds its tracer to the FIRST TracerProvider installed via the global
// delegate — that binding is one-shot, so we must Init exactly ONCE for the
// whole package and route spans to a per-subtest sink, rather than re-init.
type swapExporter struct {
	mu   sync.Mutex
	sink *tracetest.InMemoryExporter
}

func (e *swapExporter) use(sink *tracetest.InMemoryExporter) {
	e.mu.Lock()
	e.sink = sink
	e.mu.Unlock()
}

func (e *swapExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	if sink == nil {
		return nil
	}
	return sink.ExportSpans(ctx, spans)
}

func (e *swapExporter) Shutdown(ctx context.Context) error { return nil }

var (
	sharedExporter = &swapExporter{}
	sharedInitOnce sync.Once
	sharedShutdown neatlogs.ShutdownFunc
)

// initShared installs the Neatlogs global provider exactly once for the package,
// routing through sharedExporter, and returns the Google API key. Skips when no
// key is set.
func initShared(t *testing.T) string {
	t.Helper()
	key := googleKey(t) // skips if absent
	sharedInitOnce.Do(func() {
		sd, err := neatlogs.Init(context.Background(),
			neatlogs.Config{WorkflowName: "adk-coverage"},
			neatlogs.WithExporter(sharedExporter),
		)
		if err != nil {
			t.Fatalf("shared Init: %v", err)
		}
		sharedShutdown = sd
	})
	return key
}

// TestMain flushes and shuts down the shared provider after all subtests.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedShutdown != nil {
		_ = sharedShutdown(context.Background())
	}
	os.Exit(code)
}

// collect routes spans to a fresh sink, runs fn, flushes, and summarizes. One
// shared provider stays installed the whole time so ADK's bound tracer keeps
// working across all subtests.
func collect(t *testing.T, fn func()) captured {
	t.Helper()
	sink := tracetest.NewInMemoryExporter()
	sharedExporter.use(sink)
	defer sharedExporter.use(nil)
	fn()
	if err := neatlogs.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return summarize(sink.GetSpans())
}

func googleKey(t *testing.T) string {
	loadDotEnv(".env")
	k := os.Getenv("GOOGLE_API_KEY")
	if k == "" {
		k = os.Getenv("GEMINI_API_KEY")
	}
	if k == "" {
		t.Skip("GOOGLE_API_KEY not set; skipping real-Gemini ADK coverage")
	}
	return k
}

func tNewModel(t *testing.T, ctx context.Context, key string) model.LLM {
	t.Helper()
	m, err := gemini.NewModel(ctx, testModel, &genai.ClientConfig{APIKey: key})
	if err != nil {
		t.Fatalf("gemini.NewModel: %v", err)
	}
	// Wrap so request/response messages are captured onto the generate_content
	// span (ADK itself emits message text only on the logs signal).
	return nladk.WrapModel(m)
}

// captured holds the neatlogs.* view of one run's spans.
type captured struct {
	spans      tracetest.SpanStubs
	kinds      map[string]int // neatlogs.span.kind -> count
	spanNames  map[string]int
	neatlogsKV map[string]bool
}

func (c captured) hasKind(k string) bool { return c.kinds[k] > 0 }

// tRunAgent runs an agent through runner.Run with the given streaming mode and
// returns the captured spans, using the shared package-wide provider.
func tRunAgent(t *testing.T, ctx context.Context, a agent.Agent, prompt string, mode agent.StreamingMode) captured {
	t.Helper()
	return collect(t, func() {
		const appName, userID, sessionID = "adk-coverage", "user-1", "session-1"
		sessions := session.InMemoryService()
		if _, err := sessions.Create(ctx, &session.CreateRequest{
			AppName: appName, UserID: userID, SessionID: sessionID,
		}); err != nil {
			t.Fatalf("session create: %v", err)
		}

		r, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: sessions})
		if err != nil {
			t.Fatalf("runner.New: %v", err)
		}

		msg := genai.NewContentFromText(prompt, genai.RoleUser)
		for _, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{StreamingMode: mode}) {
			if err != nil {
				t.Fatalf("run: %v", err)
			}
		}
	})
}

func summarize(spans tracetest.SpanStubs) captured {
	c := captured{
		spans:      spans,
		kinds:      map[string]int{},
		spanNames:  map[string]int{},
		neatlogsKV: map[string]bool{},
	}
	for _, s := range spans {
		c.spanNames[s.Name]++
		for _, kv := range s.Attributes {
			key := string(kv.Key)
			if len(key) >= 9 && key[:9] == "neatlogs." {
				c.neatlogsKV[key] = true
			}
			if key == "neatlogs.span.kind" {
				c.kinds[kv.Value.AsString()]++
			}
		}
	}
	return c
}

// assertLLMTrace asserts the invariants every ADK LLM run must satisfy: at least
// one llm span, normalized model + token attributes, and a usable trace shape.
func assertLLMTrace(t *testing.T, c captured) {
	t.Helper()
	if len(c.spans) == 0 {
		t.Fatal("no spans captured — ADK did not flow through the global provider")
	}
	if !c.hasKind("llm") {
		t.Errorf("expected an llm span; kinds=%v names=%v", c.kinds, c.spanNames)
	}
	for _, want := range []string{"neatlogs.llm.model_name", "neatlogs.llm.token_count.prompt"} {
		if !c.neatlogsKV[want] {
			t.Errorf("missing %s; neatlogs keys=%v", want, keysOf(c.neatlogsKV))
		}
	}
	// I/O must be on the trace (WrapModel), not just in logs. Input always
	// present; output present whenever the model produced text (vs. only a tool
	// call), which the haiku/sentence prompts always do.
	if !c.neatlogsKV["neatlogs.llm.input_messages.0.role"] {
		t.Errorf("missing input messages on span; neatlogs keys=%v", keysOf(c.neatlogsKV))
	}
}

// assertHasOutput requires captured assistant output text on some span. Used by
// text-producing scenarios (not tool-only turns).
func assertHasOutput(t *testing.T, c captured) {
	t.Helper()
	if !c.neatlogsKV["neatlogs.llm.output_messages.0.content"] {
		t.Errorf("missing output_messages on span (WrapModel output capture); neatlogs keys=%v", keysOf(c.neatlogsKV))
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── 1. Non-streaming ───────────────────────────────────────────────────────

func TestADK_NonStreaming(t *testing.T) {
	key := initShared(t)
	ctx := context.Background()
	a, err := llmagent.New(llmagent.Config{
		Name: "haiku_agent", Model: tNewModel(t, ctx, key),
		Instruction: "Write a single three-line haiku about the topic. Nothing else.",
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	c := tRunAgent(t, ctx, a, "the sea at dawn", agent.StreamingModeNone)
	assertLLMTrace(t, c)
	assertHasOutput(t, c)
	t.Logf("non-streaming: kinds=%v spans=%d", c.kinds, len(c.spans))
}

// ── 2. SSE streaming ────────────────────────────────────────────────────────

func TestADK_Streaming(t *testing.T) {
	key := initShared(t)
	ctx := context.Background()
	a, err := llmagent.New(llmagent.Config{
		Name: "haiku_agent", Model: tNewModel(t, ctx, key),
		Instruction: "Write a single three-line haiku about the topic. Nothing else.",
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	c := tRunAgent(t, ctx, a, "a mountain in winter", agent.StreamingModeSSE)
	assertLLMTrace(t, c)
	assertHasOutput(t, c)
	t.Logf("streaming: kinds=%v spans=%d", c.kinds, len(c.spans))
}

// ── 3. Tool calls → execute_tool spans ──────────────────────────────────────

func TestADK_ToolCalls(t *testing.T) {
	key := initShared(t)
	ctx := context.Background()

	weatherTool, err := functiontool.New(
		functiontool.Config{Name: "get_weather", Description: "Get the current weather for a city."},
		func(_ tool.Context, args weatherArgs) (weatherResult, error) {
			return weatherResult{Report: "Sunny, 24°C in " + args.City}, nil
		},
	)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name: "weather_agent", Model: tNewModel(t, ctx, key),
		Instruction: "Use the get_weather tool to answer weather questions, then report the result.",
		Tools:       []tool.Tool{weatherTool},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}

	c := tRunAgent(t, ctx, a, "What's the weather in Tokyo?", agent.StreamingModeNone)
	assertLLMTrace(t, c)
	// The model decides whether to call the tool. When it does, we must capture
	// the execute_tool span as a normalized tool-kind span.
	if c.spanNames["execute_tool get_weather"] > 0 || c.hasKind("tool") {
		t.Logf("tool call captured: tool spans=%d", c.kinds["tool"])
	} else {
		t.Logf("model did not invoke the tool this run (kinds=%v) — llm trace still valid", c.kinds)
	}
}

// ── 4. Sequential workflow agents → multiple invoke_agent ───────────────────

func TestADK_SequentialWorkflow(t *testing.T) {
	key := initShared(t)
	ctx := context.Background()

	writer, err := llmagent.New(llmagent.Config{
		Name: "writer", Model: tNewModel(t, ctx, key),
		Instruction: "Write one sentence about the topic.",
	})
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	critic, err := llmagent.New(llmagent.Config{
		Name: "critic", Model: tNewModel(t, ctx, key),
		Instruction: "Improve the previous sentence in one sentence.",
	})
	if err != nil {
		t.Fatalf("critic: %v", err)
	}

	seq, err := sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{
			Name:        "pipeline",
			Description: "Writes then critiques.",
			SubAgents:   []agent.Agent{writer, critic},
		},
	})
	if err != nil {
		t.Fatalf("sequential: %v", err)
	}

	c := tRunAgent(t, ctx, seq, "the ocean", agent.StreamingModeNone)
	assertLLMTrace(t, c)
	// A sequential pipeline of two LLM agents should yield ≥2 llm spans.
	if c.kinds["llm"] < 2 {
		t.Errorf("expected ≥2 llm spans from a 2-agent pipeline; got %d (kinds=%v)", c.kinds["llm"], c.kinds)
	}
	t.Logf("sequential: kinds=%v spans=%d", c.kinds, len(c.spans))
}

// ── 5. A2A: remote agent over HTTP ──────────────────────────────────────────

func TestADK_A2A(t *testing.T) {
	key := initShared(t)
	ctx := context.Background()

	addr := tStartA2AServer(t, ctx, key)

	remote, err := remoteagent.NewA2A(remoteagent.A2AConfig{
		Name:              "remote_weather",
		AgentCardProvider: remoteagent.NewAgentCardProvider(addr),
	})
	if err != nil {
		t.Fatalf("remote agent: %v", err)
	}

	c := tRunAgent(t, ctx, remote, "What is the weather in Paris?", agent.StreamingModeNone)
	if len(c.spans) == 0 {
		t.Fatal("no spans captured for A2A run")
	}
	// The A2A client run emits at least an invoke_agent span on this side; the
	// remote server's LLM spans are emitted in its own runner.
	t.Logf("a2a: kinds=%v names=%v spans=%d", c.kinds, c.spanNames, len(c.spans))
}

// tStartA2AServer stands up a local ADK A2A server exposing a weather agent and
// returns its base URL. Modeled on the ADK a2a example.
func tStartA2AServer(t *testing.T, ctx context.Context, key string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	base := &url.URL{Scheme: "http", Host: listener.Addr().String()}

	a, err := llmagent.New(llmagent.Config{
		Name: "weather_time_agent", Model: tNewModel(t, ctx, key),
		Description: "Answers weather questions.",
		Instruction: "Answer the user's weather question in one sentence.",
	})
	if err != nil {
		t.Fatalf("a2a agent: %v", err)
	}

	const agentPath = "/invoke"
	card := &a2a.AgentCard{
		Name:        a.Name(),
		Description: a.Description(),
		SupportedInterfaces: []*a2a.AgentInterface{{
			URL:             base.JoinPath(agentPath).String(),
			ProtocolBinding: a2a.TransportProtocolJSONRPC,
			ProtocolVersion: a2a.Version,
		}},
		Version:            "1.0.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             adka2a.BuildAgentSkills(a),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}

	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{AppName: a.Name(), Agent: a, SessionService: session.InMemoryService()},
	})
	mux.Handle(agentPath, a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)))

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener)
	t.Cleanup(func() { srv.Close() })

	// Give the server a moment to start serving the agent card.
	time.Sleep(200 * time.Millisecond)
	return base.String()
}

// ── 6. Concurrent runs: goroutine safety of context propagation ─────────────

func TestADK_ConcurrentRuns(t *testing.T) {
	key := initShared(t)
	ctx := context.Background()

	const n = 4
	c := collect(t, func() {
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				a, err := llmagent.New(llmagent.Config{
					Name: "haiku_agent", Model: tNewModel(t, ctx, key),
					Instruction: "Write a single three-line haiku. Nothing else.",
				})
				if err != nil {
					t.Errorf("agent %d: %v", i, err)
					return
				}
				appName := "concurrent"
				userID := "u"
				sessionID := "s-" + string(rune('a'+i))
				sessions := session.InMemoryService()
				if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID}); err != nil {
					t.Errorf("session %d: %v", i, err)
					return
				}
				r, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: sessions})
				if err != nil {
					t.Errorf("runner %d: %v", i, err)
					return
				}
				msg := genai.NewContentFromText("a river", genai.RoleUser)
				for _, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{}) {
					if err != nil {
						t.Errorf("run %d: %v", i, err)
						return
					}
				}
			}(i)
		}
		wg.Wait()
	})

	if c.kinds["llm"] < n {
		t.Errorf("expected ≥%d llm spans across concurrent runs; got %d", n, c.kinds["llm"])
	}
	t.Logf("concurrent: llm spans=%d total=%d", c.kinds["llm"], len(c.spans))
}
