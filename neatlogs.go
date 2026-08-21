// Package neatlogs is the Go SDK for Neatlogs — OpenTelemetry-based tracing for
// Go LLM agents.
//
// It exports spans over OTLP/HTTP to the Neatlogs ingestion endpoint
// ({endpoint}/v1/traces) and normalizes their attributes into the neatlogs.*
// namespace shared with the Python and TypeScript SDKs.
//
// Spans reach Neatlogs through explicit SDK wrappers and tracing helpers:
//
//   - Active wrapping. Call WrapGenAI on a google.golang.org/genai client to
//     trace each GenerateContent / GenerateContentStream / EmbedContent /
//     CountTokens call with full request/response detail (including message
//     text) on the span.
//
// Init never changes the process-global OpenTelemetry provider or propagator.
// This keeps Neatlogs isolated from Datadog and other co-tenant
// instrumentation. OTel-native frameworks that only use the global provider
// must be integrated through an explicit Neatlogs wrapper.
//
// Typical usage:
//
//	ctx := context.Background()
//	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{
//		APIKey:       os.Getenv("NEATLOGS_API_KEY"),
//		WorkflowName: "my-agent",
//	})
//	if err != nil { log.Fatal(err) }
//	defer shutdown(ctx)
package neatlogs

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/neatlogs/neatlogs-go/internal/attributes"
)

// defaultEndpoint is the Neatlogs ingestion base URL. Override via
// Config.Endpoint or the NEATLOGS_ENDPOINT environment variable.
const defaultEndpoint = "https://ingest.neatlogs.com"

// tracerName is the instrumentation scope used by this SDK's own wrappers.
const tracerName = "neatlogs-go"

// Config controls SDK initialization. All fields are optional except APIKey
// (which may also be supplied via the NEATLOGS_API_KEY environment variable).
type Config struct {
	// APIKey authenticates with Neatlogs. Falls back to NEATLOGS_API_KEY.
	// If empty after that fallback, export is disabled and spans are dropped.
	APIKey string

	// Endpoint is the Neatlogs ingestion base URL (without the /v1/traces
	// path). Falls back to NEATLOGS_ENDPOINT, then defaultEndpoint
	// (https://ingest.neatlogs.com).
	Endpoint string

	// WorkflowName labels this service/run. Defaults to the executable name.
	WorkflowName string

	// Tags are attached to every span as a resource attribute.
	Tags []string

	// Debug enables verbose diagnostics on stderr.
	Debug bool

	// DisableExport drops all spans instead of sending them. Useful in tests.
	DisableExport bool

	// EnableSignalHandlers opts Init into handling SIGINT and SIGTERM. The zero
	// value is false: by default Neatlogs never calls signal.Notify and the host
	// retains complete signal ownership. Clients never install signal handlers.
	EnableSignalHandlers bool

	// DisableSignalHandlers is retained for source compatibility.
	// Deprecated: signal handling is disabled by default. If both fields are
	// true, DisableSignalHandlers wins.
	DisableSignalHandlers bool
}

// ShutdownFunc flushes pending spans and releases SDK resources. Call it (often
// via defer) before the process exits so buffered spans are not lost.
type ShutdownFunc func(context.Context) error

var (
	noopTP = trace.NewNoopTracerProvider()
	global = globalLifecycle{state: stateUninitialized}
)

// globalLifecycle is the process-wide Init gate. It deliberately remains in
// stateClosing until the old provider has fully shut down, preventing Init and
// new spans from overlapping that close.
type globalLifecycle struct {
	mu      sync.Mutex
	state   sdkState
	runtime *sdkRuntime
}

// Option customizes Init or NewClient. Options are for advanced/testing use;
// the common path needs only Config.
type Option func(*initOptions)

type initOptions struct {
	exporter sdktrace.SpanExporter
}

// WithExporter overrides the OTLP/HTTP exporter with a custom SpanExporter. The
// SDK still wraps it so attributes are normalized to neatlogs.* before export.
// Useful for tests (in-memory exporter) or alternate sinks (stdout). When set,
// Config.Endpoint/APIKey are ignored for transport, but DisableExport still
// suppresses all export.
func WithExporter(exp sdktrace.SpanExporter) Option {
	return func(o *initOptions) { o.exporter = exp }
}

// Init configures a private OpenTelemetry TracerProvider for Neatlogs and
// returns a ShutdownFunc. It never changes process-global OpenTelemetry state.
// It is safe to call once; a second call without an intervening shutdown
// returns an error.
func Init(ctx context.Context, cfg Config, opts ...Option) (ShutdownFunc, error) {
	var io initOptions
	for _, opt := range opts {
		opt(&io)
	}

	global.mu.Lock()
	if global.state != stateUninitialized {
		global.mu.Unlock()
		return nil, fmt.Errorf("neatlogs: already initialized; call the returned shutdown first")
	}
	runtime, base, exportEnabled, err := buildSDKRuntime(ctx, cfg, io)
	if err != nil {
		global.mu.Unlock()
		return nil, err
	}
	global.runtime = runtime
	global.state = stateRunning

	if cfg.EnableSignalHandlers && !cfg.DisableSignalHandlers {
		signalController := newShutdownSignalController()
		runtime.setSignalController(signalController)
		signalController.Start(func(sig os.Signal) {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulSignalTimeout)
			defer cancel()
			_ = global.shutdown(shutdownCtx, runtime, signalTerminationReason(sig))
		})
	}
	global.mu.Unlock()

	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "neatlogs: initialized (workflow=%q, endpoint=%s, export=%v)\n", runtime.workflowName, base.String(), exportEnabled)
	}

	return func(ctx context.Context) error {
		return global.shutdown(ctx, runtime, "shutdown")
	}, nil
}

// Flush forces a synchronous export of all buffered spans. Safe to call even
// when the SDK is not initialized (it is a no-op then).
func Flush(ctx context.Context) error {
	if client, ok := ClientFromContext(ctx); ok {
		return client.Flush(ctx)
	}
	return global.forceFlush(ctx)
}

func (g *globalLifecycle) startSpan(
	ctx context.Context,
	name string,
	options ...trace.SpanStartOption,
) (context.Context, trace.Span, func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != stateRunning || g.runtime == nil {
		return startNoopSpan(ctx, name, options...)
	}
	return g.runtime.startSpan(ctx, name, options...)
}

func (g *globalLifecycle) startProviderSpan(ctx context.Context, name, kind string) (context.Context, trace.Span, func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != stateRunning || g.runtime == nil {
		return startNoopSpan(ctx, name)
	}
	return g.runtime.startProviderSpan(ctx, name, kind)
}

func startSpanForContext(
	ctx context.Context,
	name string,
	options ...trace.SpanStartOption,
) (context.Context, trace.Span, func()) {
	if client, ok := ClientFromContext(ctx); ok {
		if client.runtime == nil {
			return startNoopSpan(ctx, name, options...)
		}
		return client.runtime.startSpan(ctx, name, options...)
	}
	return global.startSpan(ctx, name, options...)
}

func startProviderSpanForContext(ctx context.Context, name, kind string) (context.Context, trace.Span, func()) {
	if client, ok := ClientFromContext(ctx); ok {
		if client.runtime == nil {
			return startNoopSpan(ctx, name)
		}
		return client.runtime.startProviderSpan(ctx, name, kind)
	}
	return global.startProviderSpan(ctx, name, kind)
}

func (g *globalLifecycle) forceFlush(ctx context.Context) error {
	g.mu.Lock()
	runtime := g.runtime
	state := g.state
	g.mu.Unlock()
	if runtime == nil || state == stateUninitialized {
		return nil
	}
	return runtime.forceFlush(ctx)
}

func (g *globalLifecycle) shutdown(ctx context.Context, runtime *sdkRuntime, reason string) error {
	g.mu.Lock()
	if g.runtime != runtime {
		g.mu.Unlock()
		return runtime.wait(ctx)
	}
	switch g.state {
	case stateRunning:
		g.state = stateClosing
	case stateClosing:
		g.mu.Unlock()
		return runtime.wait(ctx)
	case stateUninitialized:
		g.mu.Unlock()
		return runtime.wait(ctx)
	}
	g.mu.Unlock()

	err := runtime.shutdown(ctx, reason)

	g.mu.Lock()
	if g.runtime == runtime {
		g.runtime = nil
		g.state = stateUninitialized
	}
	g.mu.Unlock()
	return err
}

// buildSDKRuntime constructs one private provider. Supplying a custom exporter
// transfers its shutdown ownership to the returned runtime.
func buildSDKRuntime(ctx context.Context, cfg Config, io initOptions) (*sdkRuntime, *url.URL, bool, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("NEATLOGS_API_KEY"))
	}

	disable := cfg.DisableExport
	// A custom exporter supplies its own transport, so the missing-API-key rule
	// (which only governs the built-in OTLP exporter) does not apply to it.
	if apiKey == "" && !disable && io.exporter == nil {
		disable = true
		if cfg.Debug {
			fmt.Fprintln(os.Stderr, "neatlogs: no API key set; export disabled (set NEATLOGS_API_KEY or Config.APIKey)")
		}
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("NEATLOGS_ENDPOINT"))
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, false, fmt.Errorf("neatlogs: invalid endpoint %q: %w", endpoint, err)
	}

	var tpOpts []sdktrace.TracerProviderOption
	tpOpts = append(tpOpts, sdktrace.WithResource(buildResource(ctx, cfg)))

	if !disable {
		exp := io.exporter
		if exp == nil {
			exp, err = newOTLPExporter(ctx, base, apiKey)
			if err != nil {
				return nil, nil, false, fmt.Errorf("neatlogs: create exporter: %w", err)
			}
		}
		// The batch processor is installed during provider construction. The
		// completion processor is registered below, after this exporter/root
		// path, so an ending root is queued before its completion marker.
		tpOpts = append(tpOpts, sdktrace.WithBatcher(&normalizingExporter{next: exp, mapper: attributes.Default()}))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	lifecycle := newActiveSpanRegistry()
	tp.RegisterSpanProcessor(lifecycle)
	tp.RegisterSpanProcessor(&identityProcessor{})
	if !disable {
		tp.RegisterSpanProcessor(&completionProcessor{tracer: tp.Tracer(tracerName, trace.WithInstrumentationVersion(Version))})
	}
	return newSDKRuntime(tp, lifecycle, resolvedWorkflowNameFrom(cfg)), base, !disable, nil
}

// newOTLPExporter builds an OTLP/HTTP span exporter targeting {base}/v1/traces
// with the x-api-key auth header Neatlogs ingestion expects.
func newOTLPExporter(ctx context.Context, base *url.URL, apiKey string) (sdktrace.SpanExporter, error) {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(base.Host),
		otlptracehttp.WithURLPath("/v1/traces"),
		otlptracehttp.WithHeaders(map[string]string{"x-api-key": apiKey}),
	}
	if base.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, opts...)
}

// buildResource assembles the resource attributes attached to every span.
// resolvedWorkflowNameFrom returns the configured workflow name, falling back
// to the executable name. Shared by buildResource and auto-root naming.
func resolvedWorkflowNameFrom(cfg Config) string {
	if w := strings.TrimSpace(cfg.WorkflowName); w != "" {
		return w
	}
	return defaultWorkflowName()
}

func buildResource(ctx context.Context, cfg Config) *resource.Resource {
	workflow := resolvedWorkflowNameFrom(cfg)

	attrs := []attribute.KeyValue{
		semconv.ServiceName(workflow),
		semconv.ServiceVersion(Version),
		attribute.String(attributes.WorkflowName, workflow),
	}
	if len(cfg.Tags) > 0 {
		attrs = append(attrs, attribute.String(attributes.Tags, strings.Join(cfg.Tags, ",")))
	}

	// Explicitly re-read OTEL_RESOURCE_ATTRIBUTES for each global runtime or
	// Client. resource.Default may have been initialized before a process-scoped
	// verification marker was installed; EnvironmentWithContext uses OTel's
	// canonical escaping/parser and safely ignores malformed entries.
	base := resource.DefaultWithContext(ctx)
	if withEnv, err := resource.Merge(base, resource.EnvironmentWithContext(ctx)); err == nil {
		base = withEnv
	}

	// SDK-owned attributes override environment values for their canonical keys;
	// unrelated resource values such as neatlogs.verification.marker survive.
	merged, err := resource.Merge(base, resource.NewSchemaless(attrs...))
	if err != nil {
		return resource.NewSchemaless(attrs...)
	}
	return merged
}

// defaultWorkflowName derives a workflow name from the source file that called
// Init — e.g. "genai/main.go" — so traces are grouped by where the SDK is used.
// It returns the last two path segments (parent dir + file), never an absolute
// path, and falls back to "neatlogs-app" if the caller can't be determined.
func defaultWorkflowName() string {
	if file, ok := callerSourceFile(); ok {
		return shortSourcePath(file)
	}
	return "neatlogs-app"
}

// sdkSourceDir is the directory containing this SDK's own source files, used to
// skip our own frames when finding the user's calling file.
var sdkSourceDir = func() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Dir(file)
}()

// callerSourceFile walks the stack outward past this SDK's own frames (matched
// by source directory, so it works regardless of the caller's package name) and
// returns the first source file belonging to the user's code.
func callerSourceFile() (string, bool) {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(2, pcs) // skip runtime.Callers + this function
	if n == 0 {
		return "", false
	}
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if frame.File != "" && (sdkSourceDir == "" || filepath.Dir(frame.File) != sdkSourceDir) {
			return frame.File, true
		}
		if !more {
			break
		}
	}
	return "", false
}

// shortSourcePath reduces an absolute source path to "<parent>.<file>", or just
// the file name when there is no parent segment. Slashes become dots so the
// workflow name reads as a single token (e.g. "genai.main.go").
func shortSourcePath(file string) string {
	file = filepath.ToSlash(file)
	parts := strings.Split(file, "/")
	switch len(parts) {
	case 0:
		return "neatlogs-app"
	case 1:
		return parts[0]
	default:
		return parts[len(parts)-2] + "." + parts[len(parts)-1]
	}
}
