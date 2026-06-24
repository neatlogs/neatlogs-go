# neatlogs-go

The Go SDK for [Neatlogs](https://neatlogs.com) — OpenTelemetry-based tracing
for Go LLM agents. v1 focuses on **Google Gemini** support.

Spans are exported over OTLP/HTTP to the Neatlogs ingestion endpoint and keyed
in the shared `neatlogs.*` attribute namespace used by the Python and TypeScript
SDKs.

## Install

```bash
go get go.neatlogs.com
```

The module is published under the vanity path `go.neatlogs.com` (hosted on GitHub
at [`neatlogs/neatlogs-go`](https://github.com/neatlogs/neatlogs-go); see
[vanity/](vanity/) for the import-redirect setup).

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
gc := neatlogs.WrapGenAI(client) // the one added line

resp, _ := gc.GenerateContent(ctx, "gemini-2.5-flash", contents, config)
```

`gc` has the same method signatures as `client.Models`, so wrapping is a
one-line change. See [examples/genai](examples/genai/main.go).

## Two ways spans reach Neatlogs

### 1. Active wrapping — `WrapGenAI`

Wrap a `google.golang.org/genai` client to trace each call with full detail:
input/output messages, tool definitions and calls, invocation parameters, token
usage, and finish reason. `GenerateContent`, `GenerateContentStream`,
`EmbedContent`, and `CountTokens` are traced. Any untraced method is reachable
via `gc.Raw()`.

### 2. Passive passthrough — Google ADK and other OTel-native frameworks

`Init` registers the **global** OpenTelemetry `TracerProvider`. Frameworks that
emit OpenTelemetry GenAI semantic-convention spans — notably
[Google ADK](https://github.com/google/adk-go) — flow through Neatlogs
automatically, with no per-call wrapping.

Attribute normalization is driven by the canonical `attribute-mapping.json`
shared verbatim with the Python and TypeScript SDKs, so every span kind
(`llm`, `tool`, `agent`, `retriever`, `embedding`, `reranker`, `guardrail`,
`mcp_tool`, `vector_store`, and more) and every recognized source vocabulary
(OpenTelemetry GenAI semconv, OpenInference, Traceloop) is translated into the
`neatlogs.*` namespace by one shared contract. The mapper only renames keys; it
performs no value rewriting or derived computation — the backend owns that.

> **ADK note:** ADK-Go records prompt/completion **text** on the OpenTelemetry
> *logs* signal, not on spans, so plain passthrough captures model, token usage,
> tool calls, and finish reasons — but not message text. To put the request and
> response **on the trace**, wrap the ADK model with
> [`contrib/adk`](contrib/adk)'s `WrapModel`:
>
> ```go
> import nladk "go.neatlogs.com/contrib/adk"
>
> model, _ := gemini.NewModel(ctx, "gemini-2.5-flash", cfg)
> agent, _ := llmagent.New(llmagent.Config{Model: nladk.WrapModel(model), ...})
> ```
>
> `WrapModel` writes input/output messages onto the `generate_content` span ADK
> already emits. It lives in its own module so ADK's dependency tree stays out of
> the core SDK.

### A2A (agent-to-agent)

A2A calls cross an HTTP boundary, so two extra pieces from `contrib/adk` keep the
trace whole:

- `A2AHTTPClient()` — an HTTP client whose transport injects the W3C
  `traceparent`; pass it to the A2A client factory so outbound calls carry the
  trace context.
- `A2ABeforeRequest` / `A2AAfterRequest` — request/response callbacks that record
  the sent message and the reply on the client's `invoke_agent` span (which has
  no local LLM to capture I/O from otherwise).
- `A2AHandler(mux)` — server middleware that extracts the incoming `traceparent`,
  so a server you own nests its spans under the caller's trace (one linked trace
  end to end).

### Examples

- [examples/genai](examples/genai/main.go) — the `WrapGenAI` path.
- [examples/adk](examples/adk/main.go) — every ADK path (single agent, streaming,
  tools, sequential/parallel/loop workflow agents, A2A, concurrent). Each runs
  one at a time to keep traces clean:

  ```bash
  go run . -scenario=tools      # or non-streaming, streaming, sequential,
                                # parallel, loop, a2a, concurrent, or 'all'
  ```

  Both example modules are separate from the core SDK so heavy deps (ADK, a2a)
  stay out of it. The [end-to-end test](examples/adk/main_test.go) runs real ADK
  agents and asserts the spans arrive normalized to `neatlogs.*`.

Export runs on a background batch processor, so instrumentation never blocks or
delays your agent code.

## Configuration

| `Config` field | Env fallback       | Notes |
|----------------|--------------------|-------|
| `APIKey`       | `NEATLOGS_API_KEY` | Your Neatlogs project key. Required to export; if empty, spans are dropped. |
| `WorkflowName` | —                  | Service/run label grouping your traces. Defaults to the caller source file (e.g. `main.go`). |
| `Tags`         | —                  | Attached to every span (optional). |

## Transport

Standard OTLP/HTTP via
`go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`, targeting
`{endpoint}/v1/traces` with an `x-api-key` header. Attribute normalization to
the `neatlogs.*` namespace happens at the exporter boundary, so spans from any
source are translated before they leave the process.

## Status

v1 — Google Gemini (`google.golang.org/genai`) active wrapping + OTel/ADK
passthrough. More providers to follow.
