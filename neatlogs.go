// Package neatlogs is the Go SDK for Neatlogs — OpenTelemetry-based tracing for
// Go LLM agents.
//
// It exports spans over OTLP/HTTP to the Neatlogs ingestion endpoint
// ({endpoint}/v1/traces) and normalizes their attributes into the neatlogs.*
// namespace shared with the Python and TypeScript SDKs.
//
// There are two ways spans reach Neatlogs:
//
//   - Active wrapping. Call WrapGenAI on a google.golang.org/genai client to
//     trace each GenerateContent / GenerateContentStream / EmbedContent /
//     CountTokens call with full request/response detail (including message
//     text) on the span.
//
//   - Passive passthrough. Init registers a *global* OpenTelemetry
//     TracerProvider, so any OTel-native framework — notably Google ADK —
//     emits spans that flow through Neatlogs automatically. ADK uses the OTel
//     GenAI semantic conventions, which the SDK maps onto neatlogs.* keys. Note
//     that ADK records prompt/completion text on the OTel logs signal rather
//     than on spans, so the ADK passthrough captures model, token usage, tool
//     calls and finish reasons, but not message text.
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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/neatlogs/neatlogs-go/internal/attributes"
)

// defaultEndpoint is the Neatlogs staging ingestion base URL. Override via
// Config.Endpoint or the NEATLOGS_ENDPOINT environment variable.
const defaultEndpoint = "https://staging-cloud.neatlogs.com"

// tracerName is the instrumentation scope used by this SDK's own wrappers.
const tracerName = "neatlogs-go"

// Config controls SDK initialization. All fields are optional except APIKey
// (which may also be supplied via the NEATLOGS_API_KEY environment variable).
type Config struct {
	// APIKey authenticates with Neatlogs. Falls back to NEATLOGS_API_KEY.
	// If empty after that fallback, export is disabled and spans are dropped.
	APIKey string

	// Endpoint is the Neatlogs ingestion base URL (without the /v1/traces
	// path). Falls back to NEATLOGS_ENDPOINT, then the staging default.
	Endpoint string

	// WorkflowName labels this service/run. Defaults to the executable name.
	WorkflowName string

	// Tags are attached to every span as a resource attribute.
	Tags []string

	// Debug enables verbose diagnostics on stderr.
	Debug bool

	// DisableExport drops all spans instead of sending them. Useful in tests.
	DisableExport bool
}

// ShutdownFunc flushes pending spans and releases SDK resources. Call it (often
// via defer) before the process exits so buffered spans are not lost.
type ShutdownFunc func(context.Context) error

var (
	mu       sync.Mutex
	provider *sdktrace.TracerProvider
)

// Option customizes Init. Options are for advanced/testing use; the common path
// needs only Config.
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

// Init configures the global OpenTelemetry TracerProvider for Neatlogs and
// returns a ShutdownFunc. It is safe to call once; a second call without an
// intervening shutdown returns an error.
func Init(ctx context.Context, cfg Config, opts ...Option) (ShutdownFunc, error) {
	var io initOptions
	for _, opt := range opts {
		opt(&io)
	}

	mu.Lock()
	defer mu.Unlock()
	if provider != nil {
		return nil, fmt.Errorf("neatlogs: already initialized; call the returned shutdown first")
	}

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

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = os.Getenv("NEATLOGS_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("neatlogs: invalid endpoint %q: %w", endpoint, err)
	}

	res := buildResource(cfg)
	setWorkflowName(resolvedWorkflowNameFrom(cfg))

	var tpOpts []sdktrace.TracerProviderOption
	tpOpts = append(tpOpts, sdktrace.WithResource(res))

	if !disable {
		exp := io.exporter
		if exp == nil {
			exp, err = newOTLPExporter(ctx, base, apiKey)
			if err != nil {
				return nil, fmt.Errorf("neatlogs: create exporter: %w", err)
			}
		}
		// Wrap so attributes are normalized to neatlogs.* before export.
		tpOpts = append(tpOpts, sdktrace.WithBatcher(&normalizingExporter{next: exp, mapper: attributes.Default()}))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	// Stamp session/end-user identity (bound via Identify) onto every root span
	// at start — including spans the SDK doesn't create itself (e.g. ADK
	// passthrough), which Trace()/the auto-root never see.
	tp.RegisterSpanProcessor(&identityProcessor{})
	// Emit a trace-completion marker when each root span ends, so the backend
	// finalizes and surfaces the trace. Registered after construction so it can
	// use the provider's own tracer.
	if !disable {
		tp.RegisterSpanProcessor(&completionProcessor{tracer: tp.Tracer(tracerName)})
	}
	otel.SetTracerProvider(tp)
	// Install W3C trace-context + baggage propagators so spans can cross process
	// boundaries (e.g. an HTTP call to another service) and stay in one trace,
	// when callers instrument their transport. Standard OTel default; harmless
	// when unused.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	provider = tp

	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "neatlogs: initialized (workflow=%q, endpoint=%s, export=%v)\n", resolvedWorkflowNameFrom(cfg), base.String(), !disable)
	}

	return func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if provider == nil {
			return nil
		}
		err := provider.Shutdown(ctx)
		provider = nil
		return err
	}, nil
}

// Flush forces a synchronous export of all buffered spans. Safe to call even
// when the SDK is not initialized (it is a no-op then).
func Flush(ctx context.Context) error {
	mu.Lock()
	tp := provider
	mu.Unlock()
	if tp == nil {
		return nil
	}
	return tp.ForceFlush(ctx)
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

func buildResource(cfg Config) *resource.Resource {
	workflow := resolvedWorkflowNameFrom(cfg)

	attrs := []attribute.KeyValue{
		semconv.ServiceName(workflow),
		attribute.String(attributes.WorkflowName, workflow),
	}
	if len(cfg.Tags) > 0 {
		attrs = append(attrs, attribute.String(attributes.Tags, strings.Join(cfg.Tags, ",")))
	}

	// resource.Default carries SDK/runtime info; merge our attrs on top.
	merged, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
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

// tracer returns the SDK's tracer from the global provider.
func tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}
