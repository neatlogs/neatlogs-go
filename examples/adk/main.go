// Command adk demonstrates Neatlogs passive passthrough for Google ADK across
// every execution path ADK-Go supports, each exported to the Neatlogs backend
// under its own workflow name so it can be found individually in the UI.
//
// ADK auto-instruments agent runs via the global OpenTelemetry TracerProvider.
// Because neatlogs.Init installs that global provider, ADK's spans
// (invoke_agent → agent, generate_content → llm, execute_tool → tool) flow
// through and are normalized into the neatlogs.* namespace WITHOUT wrapping any
// ADK call. The only Neatlogs-specific lines are Init and the deferred shutdown.
//
// Scenarios (each its own workflow in the UI):
//
//	adk-non-streaming   single agent, StreamingModeNone
//	adk-streaming       single agent, StreamingModeSSE
//	adk-tools           agent + functiontool  → execute_tool span
//	adk-sequential      sequentialagent pipeline (writer → critic)
//	adk-parallel        parallelagent fan-out
//	adk-loop            loopagent (bounded iterations)
//	adk-a2a             remote agent over the A2A protocol
//	adk-concurrent      N agents run concurrently (goroutine safety)
//
// Run:
//
//	export NEATLOGS_API_KEY=...   # spans dropped if unset
//	export GOOGLE_API_KEY=...
//	go run .
//
// Keys may instead be placed in a .env file in this directory. Audio/bidi
// (RunLive) is intentionally out of scope.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/agent/remoteagent/v2"
	"google.golang.org/adk/agent/workflowagents/loopagent"
	"google.golang.org/adk/agent/workflowagents/parallelagent"
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

const adkModel = "gemini-2.5-flash"

func main() {
	loadDotEnv(".env")

	scenarios := map[string]func(context.Context, string){
		"non-streaming": func(ctx context.Context, k string) { runSingle(ctx, k, agent.StreamingModeNone) },
		"streaming":     func(ctx context.Context, k string) { runSingle(ctx, k, agent.StreamingModeSSE) },
		"tools":         runTools,
		"sequential":    runSequential,
		"parallel":      runParallel,
		"loop":          runLoop,
		"a2a":           runA2A,
		"concurrent":    func(ctx context.Context, k string) { runConcurrent(ctx, k, 4) },
	}

	which := flag.String("scenario", "non-streaming", "which scenario to run: "+scenarioList(scenarios)+", or 'all'")
	flag.Parse()

	googleKey := firstEnv("GOOGLE_API_KEY", "GEMINI_API_KEY")
	if googleKey == "" {
		log.Fatal("set GOOGLE_API_KEY (or GEMINI_API_KEY) — these scenarios make real Gemini calls")
	}

	// One scenario per run keeps the UI clean: each runner.Run is one trace, so
	// running everything at once floods the workflow with a dozen traces. Pick a
	// scenario with -scenario, or pass -scenario=all to run them all.
	//
	// Neatlogs is initialized ONCE here. ADK (like all OTel libraries) binds its
	// tracer to the first global TracerProvider installed, and that binding is
	// one-shot — re-initializing per scenario would leave later scenarios with no
	// spans. A real app likewise calls Init once at startup.
	ctx := context.Background()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{
		APIKey:       os.Getenv("NEATLOGS_API_KEY"),
		WorkflowName: "adk-example",
	})
	if err != nil {
		log.Fatalf("neatlogs init: %v", err)
	}
	defer shutdown(ctx)

	runOne := func(name string) {
		fn, ok := scenarios[name]
		if !ok {
			log.Fatalf("unknown scenario %q; choose one of %s, or 'all'", name, scenarioList(scenarios))
		}
		fmt.Printf("▶ %s\n", name)
		fn(ctx, googleKey)
	}

	if *which == "all" {
		// Deterministic order for readability.
		for _, name := range []string{"non-streaming", "streaming", "tools", "sequential", "parallel", "loop", "a2a", "concurrent"} {
			runOne(name)
		}
	} else {
		runOne(*which)
	}

	if err := neatlogs.Flush(ctx); err != nil {
		log.Printf("flush: %v", err)
	}
	fmt.Println("\nExported under the adk-example workflow.")
}

func scenarioList(m map[string]func(context.Context, string)) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func newModel(ctx context.Context, key string) model.LLM {
	m, err := gemini.NewModel(ctx, adkModel, &genai.ClientConfig{APIKey: key})
	if err != nil {
		log.Fatalf("create model: %v", err)
	}
	// Wrap so request/response messages land on the generate_content span.
	// ADK itself only emits message text on the logs signal; this brings I/O
	// onto the trace, where Neatlogs reads semantic data.
	return nladk.WrapModel(m)
}

// runAgent drives one agent to completion through the runner, printing any text.
func runAgent(ctx context.Context, a agent.Agent, appName, prompt string, mode agent.StreamingMode) {
	const userID, sessionID = "user-1", "session-1"
	sessions := session.InMemoryService()
	if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID}); err != nil {
		log.Fatalf("[%s] session: %v", appName, err)
	}
	r, err := runner.New(runner.Config{AppName: appName, Agent: a, SessionService: sessions})
	if err != nil {
		log.Fatalf("[%s] runner: %v", appName, err)
	}
	msg := genai.NewContentFromText(prompt, genai.RoleUser)
	for ev, err := range r.Run(ctx, userID, sessionID, msg, agent.RunConfig{StreamingMode: mode}) {
		if err != nil {
			log.Fatalf("[%s] run: %v", appName, err)
		}
		printEvent(ev)
	}
}

func printEvent(ev *session.Event) {
	if ev == nil || ev.Content == nil {
		return
	}
	for _, part := range ev.Content.Parts {
		if part.Text != "" {
			fmt.Printf("  %s\n", strings.TrimSpace(part.Text))
		}
	}
}

// ── single agent (non-streaming / streaming) ────────────────────────────────

func runSingle(ctx context.Context, key string, mode agent.StreamingMode) {
	a, err := llmagent.New(llmagent.Config{
		Name: "haiku_agent", Model: newModel(ctx, key),
		Instruction: "Write a single three-line haiku about the topic. Nothing else.",
	})
	if err != nil {
		log.Fatalf("agent: %v", err)
	}
	runAgent(ctx, a, "adk-single", "the sea at dawn", mode)
}

// ── tool call ───────────────────────────────────────────────────────────────

type weatherArgs struct {
	City string `json:"city" jsonschema:"the city to get the weather for"`
}
type weatherResult struct {
	Report string `json:"report"`
}

func runTools(ctx context.Context, key string) {
	weatherTool, err := functiontool.New(
		functiontool.Config{Name: "get_weather", Description: "Get the current weather for a city."},
		func(_ tool.Context, args weatherArgs) (weatherResult, error) {
			return weatherResult{Report: "Sunny, 24°C in " + args.City}, nil
		},
	)
	if err != nil {
		log.Fatalf("tool: %v", err)
	}
	a, err := llmagent.New(llmagent.Config{
		Name: "weather_agent", Model: newModel(ctx, key),
		Instruction: "Use the get_weather tool to answer, then report the result in one sentence.",
		Tools:       []tool.Tool{weatherTool},
	})
	if err != nil {
		log.Fatalf("agent: %v", err)
	}
	runAgent(ctx, a, "adk-tools", "What's the weather in Tokyo?", agent.StreamingModeNone)
}

// ── workflow agents: sequential / parallel / loop ───────────────────────────

func runSequential(ctx context.Context, key string) {
	writer := mustAgent(ctx, key, "writer", "Write one sentence about the topic.")
	critic := mustAgent(ctx, key, "critic", "Improve the previous sentence in one sentence.")
	seq, err := sequentialagent.New(sequentialagent.Config{
		AgentConfig: agent.Config{Name: "pipeline", Description: "Writes then critiques.", SubAgents: []agent.Agent{writer, critic}},
	})
	if err != nil {
		log.Fatalf("sequential: %v", err)
	}
	runAgent(ctx, seq, "adk-sequential", "the ocean", agent.StreamingModeNone)
}

func runParallel(ctx context.Context, key string) {
	haiku := mustAgent(ctx, key, "haiku_writer", "Write a haiku about the topic.")
	limerick := mustAgent(ctx, key, "limerick_writer", "Write a one-line limerick about the topic.")
	par, err := parallelagent.New(parallelagent.Config{
		AgentConfig: agent.Config{Name: "fanout", Description: "Writes two poems in parallel.", SubAgents: []agent.Agent{haiku, limerick}},
	})
	if err != nil {
		log.Fatalf("parallel: %v", err)
	}
	runAgent(ctx, par, "adk-parallel", "autumn leaves", agent.StreamingModeNone)
}

func runLoop(ctx context.Context, key string) {
	refiner := mustAgent(ctx, key, "refiner", "Refine the idea by one increment. Keep it to one sentence.")
	loop, err := loopagent.New(loopagent.Config{
		AgentConfig:   agent.Config{Name: "refine_loop", Description: "Iteratively refines an idea.", SubAgents: []agent.Agent{refiner}},
		MaxIterations: 3, // bounded so the example terminates
	})
	if err != nil {
		log.Fatalf("loop: %v", err)
	}
	runAgent(ctx, loop, "adk-loop", "a product idea for developers", agent.StreamingModeNone)
}

func mustAgent(ctx context.Context, key, name, instruction string) agent.Agent {
	a, err := llmagent.New(llmagent.Config{Name: name, Model: newModel(ctx, key), Instruction: instruction})
	if err != nil {
		log.Fatalf("agent %s: %v", name, err)
	}
	return a
}

// ── A2A: remote agent over HTTP ─────────────────────────────────────────────

// runA2A instruments only the CLIENT side — the realistic integration shape,
// since the A2A server is typically a separate, already-running service you do
// not own. The only Neatlogs-specific addition is the traceparent-injecting HTTP
// client (nladk.A2AHTTPClient): it carries the trace context outbound so that,
// IF the remote server also propagates context, the call stays in one trace.
// The local server below stands in for that remote party and is intentionally
// left uninstrumented.
func runA2A(ctx context.Context, key string) {
	addr := startA2AServer(ctx, key)

	factory := a2aclient.NewFactory(a2aclient.WithJSONRPCTransport(nladk.A2AHTTPClient()))

	remote, err := remoteagent.NewA2A(remoteagent.A2AConfig{
		Name:              "remote_weather",
		AgentCardProvider: remoteagent.NewAgentCardProvider(addr),
		ClientProvider:    remoteagent.NewA2AClientProvider(factory),
		// Capture the request/response on the client's invoke_agent span — the
		// client delegates over HTTP and has no local LLM, so without these the
		// span would carry no I/O.
		BeforeRequestCallbacks: []remoteagent.BeforeA2ARequestCallback{nladk.A2ABeforeRequest},
		AfterRequestCallbacks:  []remoteagent.AfterA2ARequestCallback{nladk.A2AAfterRequest},
	})
	if err != nil {
		log.Fatalf("remote agent: %v", err)
	}
	runAgent(ctx, remote, "adk-a2a", "What is the weather in Paris?", agent.StreamingModeNone)
}

func startA2AServer(ctx context.Context, key string) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	base := &url.URL{Scheme: "http", Host: listener.Addr().String()}

	a, err := llmagent.New(llmagent.Config{
		Name: "weather_time_agent", Model: newModel(ctx, key),
		Description: "Answers weather questions.",
		Instruction: "Answer the user's weather question in one sentence.",
	})
	if err != nil {
		log.Fatalf("a2a agent: %v", err)
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

	// Instrument the server side too: A2AHandler extracts the incoming traceparent
	// so the server's agent/LLM spans nest under the client's trace — one linked
	// trace end to end. (The server's model is wrapped via newModel → WrapModel,
	// so its LLM span carries input/output.)
	srv := &http.Server{Handler: nladk.A2AHandler(mux)}
	go srv.Serve(listener)
	time.Sleep(200 * time.Millisecond) // let the card endpoint come up
	return base.String()
}

// ── concurrent runs ─────────────────────────────────────────────────────────

func runConcurrent(ctx context.Context, key string, n int) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			a, err := llmagent.New(llmagent.Config{
				Name: "haiku_agent", Model: newModel(ctx, key),
				Instruction: "Write a single three-line haiku. Nothing else.",
			})
			if err != nil {
				log.Printf("agent %d: %v", i, err)
				return
			}
			sid := fmt.Sprintf("session-%d", i)
			sessions := session.InMemoryService()
			if _, err := sessions.Create(ctx, &session.CreateRequest{AppName: "adk-concurrent", UserID: "u", SessionID: sid}); err != nil {
				log.Printf("session %d: %v", i, err)
				return
			}
			r, err := runner.New(runner.Config{AppName: "adk-concurrent", Agent: a, SessionService: sessions})
			if err != nil {
				log.Printf("runner %d: %v", i, err)
				return
			}
			for _, err := range r.Run(ctx, "u", sid, genai.NewContentFromText("a river", genai.RoleUser), agent.RunConfig{}) {
				if err != nil {
					log.Printf("run %d: %v", i, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// ── helpers ─────────────────────────────────────────────────────────────────

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// loadDotEnv reads simple KEY=VALUE lines from path and sets any not already in
// the environment. Missing file is not an error.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}
