# neatlogs-go

The Go SDK for [Neatlogs](https://neatlogs.com) — OpenTelemetry-based tracing
for Go LLM agents. The initial release line focuses on **Google Gemini** support
and explicit instrumentation helpers.

Spans are exported over OTLP/HTTP to the Neatlogs ingestion endpoint and keyed
in the shared `neatlogs.*` attribute namespace used by the Python and TypeScript
SDKs.

## Install

```bash
go get github.com/neatlogs/neatlogs-go
go get github.com/neatlogs/neatlogs-go/contrib/genai
```

## Quick start

```go
ctx := context.Background()

shutdown, err := neatlogs.Init(ctx, neatlogs.Config{
    APIKey:       os.Getenv("NEATLOGS_API_KEY"),
    WorkflowName: "my-agent",
})
if err != nil {
    log.Fatal(err)
}
defer shutdown(ctx)

client, _ := genai.NewClient(ctx, &genai.ClientConfig{APIKey: os.Getenv("GEMINI_API_KEY")})
gc := nlgenai.WrapGenAI(client) // the one added line

resp, _ := gc.GenerateContent(ctx, "gemini-2.5-flash", contents, config)
```

`gc` has the same method signatures as `client.Models`, so wrapping is a
one-line change. See [examples/genai](examples/genai/main.go).

### Independent context-scoped clients

Use `NewClient` when one process must send different requests to different
Neatlogs projects or own their lifecycles independently:

```go
project, err := neatlogs.NewClient(ctx, neatlogs.Config{
    APIKey:       projectKey,
    WorkflowName: "tenant-agent",
})
if err != nil {
    log.Fatal(err)
}
defer project.Shutdown(context.Background())

ctx = neatlogs.WithClient(ctx, project)
resp, err := nlgenai.WrapGenAI(client).GenerateContent(ctx, model, contents, config)
```

The bound context routes `WrapGenAI`, `Trace`, `StartSpan`,
`StartProviderSpan`, and the LLM/retriever/tool helpers through that Client.
Each Client owns its provider, exporter, workflow resource, active spans,
flush, and shutdown gate. Closing one Client does not affect global `Init` or
another Client, and a context bound to a closed Client produces no-op spans
rather than falling back to the global pipeline.

## Instrumentation and isolation

### 1. Active wrapping — `WrapGenAI`

Wrap a `google.golang.org/genai` client to trace each call with full detail:
input/output messages, tool definitions and calls, invocation parameters, token
usage, and finish reason. `GenerateContent`, `GenerateContentStream`,
`EmbedContent`, and `CountTokens` are traced. Any untraced method is reachable
via `gc.Raw()`.

### 2. Isolation from other tracing SDKs

`Init` creates a private OpenTelemetry `TracerProvider`. It never replaces the
process-global provider or propagator, so Datadog and other instrumentation
cannot export, parent, or be parented by Neatlogs spans.

Attribute normalization is driven by the canonical `attribute-mapping.json`
shared verbatim with the Python and TypeScript SDKs, so every span kind
(`llm`, `tool`, `agent`, `retriever`, `embedding`, `reranker`, `guardrail`,
`mcp_tool`, `vector_store`, and more) and every recognized source vocabulary
(OpenTelemetry GenAI semconv, OpenInference, Traceloop) is translated into the
`neatlogs.*` namespace by one shared contract. The mapper only renames keys; it
performs no value rewriting or derived computation — the backend owns that.

Frameworks that resolve tracers only from the global OTel API are not
auto-instrumented. They require an explicit wrapper or an injected private
tracer/provider integration. Google ADK support is therefore deferred until
that integration can be isolated bidirectionally.

### Cross-process propagation

Use `InjectTraceContext` and `ExtractTraceContext` at explicit HTTP/RPC
boundaries. They use a private W3C trace-context propagator and do not touch the
global OTel propagator.

For direct OpenAI/Anthropic calls and service boundaries, use
`StartLLMSpan`, `StartRetrieverSpan`, `StartToolSpanFromHeaders`, or the
lower-level `Trace`/`StartSpan` helpers. These all share the same private
provider and automatic workflow-root behavior.

For an explicit workflow root, record the final application result before
ending the span:

```go
ctx, root, end := neatlogs.Trace(ctx, "chat_turn")
defer end()

if err := neatlogs.SetTraceInput(root, map[string]any{"question": "weather"}); err != nil {
    return err
}
result := map[string]any{"answer": "sunny"}
if err := neatlogs.SetTraceOutput(root, result); err != nil {
    return err
}
```

### Examples

- [examples/genai](examples/genai/main.go) — the `WrapGenAI` path.

Export runs on a background batch processor, so instrumentation never blocks or
delays your agent code.

## Configuration

| `Config` field | Env fallback       | Notes |
|----------------|--------------------|-------|
| `APIKey`       | `NEATLOGS_API_KEY` | Your Neatlogs project key. Required to export; if empty, spans are dropped. |
| `WorkflowName` | —                  | Service/run label grouping your traces. Defaults to the caller source file (e.g. `main.go`). |
| `Tags`         | —                  | Attached to every span (optional). |
| `SampleRate`   | —                  | Optional pointer to a trace sampling rate in `[0, 1]`; nil keeps every trace. The root decision is inherited by all descendants. |
| `Mask`         | —                  | Context-aware local transform applied to a cloned, normalized span on the export worker. An error or nil result drops that span; unmasked content is never sent. |
| `EnableSignalHandlers` | —           | Opt in to SDK-owned SIGINT/SIGTERM shutdown. Defaults to `false`; Neatlogs does not call `signal.Notify` unless enabled. Applies to `Init`, not `NewClient`. |

`DisableSignalHandlers` remains as a deprecated source-compatibility field.
Signal handling is now disabled by default; if both signal fields are true,
`DisableSignalHandlers` wins. Hosts that own signals should leave
`EnableSignalHandlers` false and call the returned shutdown function during
their existing shutdown sequence.

To correlate a one-off live verification run, append
`neatlogs.verification.marker=<UUID>` to the launched process's existing
`OTEL_RESOURCE_ATTRIBUTES`. The SDK uses OpenTelemetry's resource-environment
parser for every global runtime and Client, so the marker and existing resource
attributes reach every emitted span without a source or persistent-config edit.

## Transport

Standard OTLP/HTTP via
`go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`, targeting
`{endpoint}/v1/traces` with an `x-api-key` header. Attribute normalization to
the `neatlogs.*` namespace happens at the exporter boundary, so spans created
through Neatlogs wrappers or an injected private tracer are translated before
they leave the process.

## Status

v0.1 — Google Gemini (`google.golang.org/genai`) active wrapping, direct
LLM/retriever/tool helpers, and an isolated private provider. More providers and
explicit framework wrappers will follow.
