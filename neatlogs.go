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
	"crypto/sha256"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/neatlogs/neatlogs-go/internal/attributes"
	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

// defaultEndpoint is the Neatlogs ingestion base URL. Override via
// Config.Endpoint or the NEATLOGS_ENDPOINT environment variable.
const defaultEndpoint = "https://ingest.neatlogs.com"

const (
	defaultMaxQueueSize       = 2048
	defaultMaxExportBatchSize = 100
	defaultBatchTimeout       = 5 * time.Second
)

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

	// SampleRate is the head-sampling probability for a complete trace. Nil
	// means 1.0 (keep every trace); a non-nil value must be within [0, 1]. The
	// root decision is inherited by every descendant and completion marker.
	SampleRate *float64

	// DisableExport drops all spans instead of sending them. Useful in tests.
	DisableExport bool

	// EnableUploads opts this pipeline into the draft authenticated upload
	// authority for large typed media and individually oversized masked OTLP
	// spans. It defaults to false and may also be enabled with
	// NEATLOGS_UPLOADS_ENABLED=true. Uploads use the same API key and endpoint.
	EnableUploads bool

	// Mask transforms a cloned, normalized span on the batch-export worker.
	// Callbacks run serially. Errors and nil results fail closed: the original
	// span is never exported. Because Go function values have no stable identity,
	// any repeated Init while a Mask is configured is treated as conflicting.
	Mask MaskFunc

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
	mu           sync.Mutex
	state        sdkState
	runtime      *sdkRuntime
	lastDelivery DeliveryDiagnosticsSnapshot
	signature    string
}

// Option customizes Init or NewClient. Options are for advanced/testing use;
// the common path needs only Config.
type Option func(*initOptions)

type initOptions struct {
	exporter    sdktrace.SpanExporter
	delivery    *deliveryDiagnostics
	doctorProbe bool
}

// WithExporter overrides the OTLP/HTTP exporter with a custom SpanExporter. The
// SDK still wraps it so attributes are normalized to neatlogs.* before export.
// Useful for tests (in-memory exporter) or alternate sinks (stdout). When set,
// Config.Endpoint/APIKey are ignored for transport, but DisableExport still
// suppresses all export.
func WithExporter(exp sdktrace.SpanExporter) Option {
	return func(o *initOptions) { o.exporter = exp }
}

// WithDoctorProbe marks the isolated pipeline as a versioned Doctor probe.
// It is intended for the Neatlogs Doctor CLI: normal application telemetry
// must not use this option. Authentication and tenant selection still come
// exclusively from Config.APIKey.
func WithDoctorProbe() Option {
	return func(o *initOptions) { o.doctorProbe = true }
}

// Init configures a private OpenTelemetry TracerProvider for Neatlogs and
// returns a ShutdownFunc. It never changes process-global OpenTelemetry state.
// Repeating the same initialization without a Mask is idempotent; conflicting
// configuration, including any repeated Init with a Mask, requires the caller
// to run the returned shutdown first.
func Init(ctx context.Context, cfg Config, opts ...Option) (ShutdownFunc, error) {
	var io initOptions
	for _, opt := range opts {
		opt(&io)
	}

	signature := initializationSignature(cfg, io)

	global.mu.Lock()
	if global.state == stateRunning && global.runtime != nil {
		runtime := global.runtime
		// Function values do not have a stable identity in Go: closures with
		// different captures may share a code pointer. A configured mask therefore
		// makes repeated Init calls explicitly conflicting.
		if global.signature == signature && !hasUnstableFunctionIdentity(cfg, io) {
			global.mu.Unlock()
			return func(ctx context.Context) error {
				return global.shutdown(ctx, runtime, "shutdown")
			}, nil
		}
		global.mu.Unlock()
		return nil, fmt.Errorf("neatlogs: already running with different configuration; call the returned shutdown first")
	}
	if global.state == stateClosing && global.runtime != nil {
		global.mu.Unlock()
		return nil, fmt.Errorf("neatlogs: shutdown is in progress; retry initialization after it completes")
	}
	runtime, base, exportEnabled, err := buildSDKRuntime(ctx, cfg, io)
	if err != nil {
		global.mu.Unlock()
		return nil, err
	}
	global.runtime = runtime
	global.state = stateRunning
	global.signature = signature

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
		g.lastDelivery = runtime.delivery.snapshot()
		g.runtime = nil
		g.state = stateUninitialized
		g.signature = ""
	}
	g.mu.Unlock()
	return err
}

func initializationSignature(cfg Config, options initOptions) string {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("NEATLOGS_API_KEY"))
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("NEATLOGS_ENDPOINT"))
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	rate := 1.0
	if cfg.SampleRate != nil {
		rate = *cfg.SampleRate
	}
	keyDigest := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf(
		"key=%x|endpoint=%s|workflow=%s|tags=%q|debug=%t|rate=%g|disable=%t|uploads=%s|mask=%s|exporter=%s|signals=%t/%t|doctor=%t",
		keyDigest,
		endpoint,
		resolvedWorkflowNameFrom(cfg),
		strings.Join(cfg.Tags, "\x00"),
		cfg.Debug,
		rate,
		cfg.DisableExport,
		uploadsSignature(cfg),
		maskSignature(cfg.Mask),
		identityOf(options.exporter),
		cfg.EnableSignalHandlers,
		cfg.DisableSignalHandlers,
		options.doctorProbe,
	)
}

func maskSignature(mask MaskFunc) string {
	if mask == nil {
		return "nil"
	}
	return "configured"
}

func hasUnstableFunctionIdentity(cfg Config, options initOptions) bool {
	if cfg.Mask != nil {
		return true
	}
	return options.exporter != nil && reflect.ValueOf(options.exporter).Kind() == reflect.Func
}

func identityOf(value any) string {
	if value == nil {
		return "nil"
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Func:
		return fmt.Sprintf("%T:function", value)
	case reflect.Chan, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return fmt.Sprintf("%T:%x", value, reflected.Pointer())
	default:
		return fmt.Sprintf("%T:%#v", value, value)
	}
}

// buildSDKRuntime constructs one private provider. Supplying a custom exporter
// transfers its shutdown ownership to the returned runtime.
func buildSDKRuntime(ctx context.Context, cfg Config, io initOptions) (*sdkRuntime, *url.URL, bool, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("NEATLOGS_API_KEY"))
	}

	uploadsEnabled, uploadsErr := resolveUploadsEnabled(cfg)
	if uploadsErr != nil {
		return nil, nil, false, uploadsErr
	}
	if uploadsEnabled && apiKey == "" && !cfg.DisableExport {
		return nil, nil, false, fmt.Errorf("neatlogs: uploads require NEATLOGS_API_KEY or Config.APIKey")
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

	sampleRate := 1.0
	if cfg.SampleRate != nil {
		sampleRate = *cfg.SampleRate
		if math.IsNaN(sampleRate) || math.IsInf(sampleRate, 0) || sampleRate < 0 || sampleRate > 1 {
			return nil, nil, false, fmt.Errorf("neatlogs: sample rate must be between 0 and 1, got %v", sampleRate)
		}
	}
	var tpOpts []sdktrace.TracerProviderOption
	tpOpts = append(
		tpOpts,
		sdktrace.WithResource(buildResource(ctx, cfg, io.doctorProbe)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRate))),
	)

	var mediaStore *internalmedia.Store
	var captures *doctorCaptureStore
	if io.doctorProbe {
		captures = newDoctorCaptureStore(16)
	}
	if !disable {
		exp := io.exporter
		if exp == nil {
			exp, err = newOTLPExporter(ctx, base, apiKey, io.doctorProbe)
			if err != nil {
				return nil, nil, false, fmt.Errorf("neatlogs: create exporter: %w", err)
			}
		}
		// The batch processor is installed during provider construction. The
		// completion processor is registered below, after this exporter/root
		// path, so an ending root is queued before its completion marker.
		delivery := &deliveryDiagnostics{}
		var uploads uploadAuthority
		if uploadsEnabled {
			uploads = newHTTPUploadAuthority(base, apiKey)
			mediaStore = internalmedia.NewStore(internalmedia.UploadLimit, internalmedia.MaxPendingItems)
			delivery.uploadAuthorityAvailable.Store(true)
		}
		byteLimited, byteErr := newByteLimitedExporter(exp, defaultMaxExportBytes, delivery)
		if byteErr != nil {
			return nil, nil, false, byteErr
		}
		queue := newDeliveryQueue(defaultMaxQueueSize, delivery)
		batchProcessor := sdktrace.NewBatchSpanProcessor(
			&normalizingExporter{
				next: byteLimited, mapper: attributes.Default(), mask: cfg.Mask,
				delivery: delivery, uploads: uploads, captures: captures, release: queue.release,
			},
			sdktrace.WithMaxQueueSize(defaultMaxQueueSize),
			sdktrace.WithMaxExportBatchSize(defaultMaxExportBatchSize),
			sdktrace.WithBatchTimeout(defaultBatchTimeout),
		)
		byteLimited.uploads = uploads
		tpOpts = append(tpOpts, sdktrace.WithSpanProcessor(&boundedSpanProcessor{
			next: batchProcessor, queue: queue, media: mediaStore,
		}))
		io.delivery = delivery
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	lifecycle := newActiveSpanRegistry()
	tp.RegisterSpanProcessor(lifecycle)
	tp.RegisterSpanProcessor(&identityProcessor{})
	if !disable {
		tp.RegisterSpanProcessor(&completionProcessor{tracer: tp.Tracer(tracerName, trace.WithInstrumentationVersion(Version))})
	}
	return newSDKRuntime(tp, lifecycle, resolvedWorkflowNameFrom(cfg), io.delivery, mediaStore, captures), base, !disable, nil
}

func resolveUploadsEnabled(cfg Config) (bool, error) {
	if cfg.EnableUploads {
		return true, nil
	}
	raw := strings.TrimSpace(os.Getenv("NEATLOGS_UPLOADS_ENABLED"))
	if raw == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("neatlogs: NEATLOGS_UPLOADS_ENABLED must be true or false")
	}
	return enabled, nil
}

func uploadsSignature(cfg Config) string {
	if cfg.EnableUploads {
		return "true"
	}
	raw := strings.TrimSpace(os.Getenv("NEATLOGS_UPLOADS_ENABLED"))
	if raw == "" {
		return "false"
	}
	return raw
}

// newOTLPExporter builds an OTLP/HTTP span exporter targeting {base}/v1/traces
// with the x-api-key auth header Neatlogs ingestion expects.
func newOTLPExporter(ctx context.Context, base *url.URL, apiKey string, doctorProbe bool) (sdktrace.SpanExporter, error) {
	headers := map[string]string{"x-api-key": apiKey}
	if doctorProbe {
		headers["x-neatlogs-doctor"] = "v1"
	}
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(base.Host),
		otlptracehttp.WithURLPath("/v1/traces"),
		otlptracehttp.WithHeaders(headers),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
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

func buildResource(ctx context.Context, cfg Config, doctorProbe bool) *resource.Resource {
	workflow := resolvedWorkflowNameFrom(cfg)

	attrs := []attribute.KeyValue{
		semconv.ServiceName(workflow),
		semconv.ServiceVersion(Version),
		attribute.String(attributes.WorkflowName, workflow),
	}
	if doctorProbe {
		attrs = append(attrs,
			attribute.Bool("neatlogs.doctor", true),
			attribute.String("neatlogs.doctor.version", "v1"),
			attribute.String("telemetry.sdk.language", "go"),
			attribute.String("telemetry.sdk.version", Version),
		)
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
