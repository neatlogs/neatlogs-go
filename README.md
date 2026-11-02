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
go get github.com/neatlogs/neatlogs-go/contrib/adk
```

The library install does not install the optional Doctor CLI. Install that
binary separately when you want to run diagnostics:

```bash
go install github.com/neatlogs/neatlogs-go/cmd/neatlogs@latest
```

Ensure `$(go env GOPATH)/bin` is on `PATH`. When Wizard cannot discover the
binary from `PATH`, set `NEATLOGS_DOCTOR_GO` to its absolute path.

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

Frameworks that resolve tracers only from the global OTel API are not passively
captured. They require explicit wrappers that create spans on Neatlogs' private
provider. Google ADK is supported through `contrib/adk`: use `adk.Run`,
`adk.InstrumentConfig`, and the private A2A propagation helpers.

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
- [examples/adk](examples/adk/main.go) — Google ADK model, tool, workflow,
  streaming, concurrent, and A2A instrumentation.

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
| `EnableUploads` | `NEATLOGS_UPLOADS_ENABLED` | Opt in to the draft authenticated upload authority for large typed media and individually oversized OTLP spans. Defaults to `false` and requires the normal API key. |
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

When uploads are explicitly enabled, large typed media and a complete masked
span that exceeds the ordinary OTLP request limit use the authenticated
`/v1/telemetry/uploads` prepare/PUT/complete flow. Only prepare and completion
receive the project API key; the object PUT receives only its short-lived,
object-scoped headers. Signed URLs and headers are discarded before telemetry
export. Only a validated `ready` response counts as delivery. Disabled,
pending, rejected, and failed uploads are reported through canonical failure
references and delivery diagnostics rather than silently retaining a digest.
Large media bytes are staged outside span data in a per-pipeline store capped at
25 MiB and 32 unique items; masks see only canonical metadata plus an opaque
token, and shutdown or any dropped span releases its staged bytes.

## Datasets

Dataset writes use a public API user or service account token with
`evaluation:write` and project access. Keep this token separate from the
telemetry ingestion key. Creation works without initializing telemetry:

```go
datasets, err := neatlogs.NewDatasetClient(neatlogs.DatasetConfig{
    Endpoint: "https://app.neatlogs.com",
    Token: publicToken,
    ProjectID: projectID,
})
if err != nil { return err }
dataset, err := datasets.Create(ctx, neatlogs.CreateDatasetOptions{Name: "Support cases"})
if err != nil { return err }
```

Capture an execution with an existing telemetry client. Pass the callback context
through your normal wrappers and helpers, and join child goroutines before
returning. Each root trace becomes a separate unlabeled case:

```go
receipt, err := datasets.Capture(ctx, telemetry, neatlogs.CaptureDatasetOptions{
    Environment: "local",
    Destination: neatlogs.DatasetDestination{DatasetID: dataset.DatasetID},
}, func(ctx context.Context) error {
    agentCtx, root, end := neatlogs.Trace(ctx, "support request")
    defer end()
    if err := neatlogs.SetTraceInput(root, question); err != nil { return err }
    answer, err := runAgent(agentCtx)
    if err != nil { return err }
    return neatlogs.SetTraceOutput(root, answer)
})
if err != nil { return err }
status, err := datasets.Wait(ctx, receipt.CaptureID)
if err != nil { return err }
publishedVersion := *status.DatasetVersionID
```

Use the child context returned by `Trace` when the agent should create child
spans under that root. Pass a nil telemetry client to select the client already
bound to `ctx`, or the running `Init` pipeline. Named destinations require
`CreateIfMissing: true` to create a dataset. `Environment` must be `local`, `ci`,
or `production`; production also requires `AllowProduction: true`. The workload
environment is independent of the backend hostname.

Capture requires full sampling and enabled export. It rejects open spans,
queue or mask drops, truncated evidence, failed uploads, and missing final
export acknowledgments. Cancellation before sealing aborts the recording.
Callback errors remain available through `errors.Is`, and complete failed
executions can still publish. Use `Status` to inspect progress or `Abort` to
cancel an unfinished recording. `Wait` accepts a context deadline and reports
failed, cancelled, or review-required jobs as errors.

If completion returns a `*neatlogs.DatasetCaptureError` with a non-nil
`Completion`, call `captureError.Completion.Retry(ctx)` to resend the same
manifest and idempotency key, then call `Wait`. This recovery never reruns the
agent. Do not rerun `Capture` to recover a failed completion.

When you supply a capture registration `IdempotencyKey`, `Capture` checks the
job's current status before executing the callback. Reusing a sealed job's key
is rejected even if the registration API replays an older `recording` receipt.
This check does not serialize concurrent `Capture` calls with the same key.

Set an outcome only on its intended case by calling `UpdateOutcomes` with the
published base version and each case's revision ID. `NewDatasetOutcome(value)`
creates a human draft outcome. A nil `Outcome` clears that case's outcome;
cases absent from `Changes` retain their outcomes. Each update publishes a new
immutable version. Creation and outcome options accept an `IdempotencyKey`
UUID for retrying the same logical write. Requests, including capture manifests,
must fit within 64 KiB. Capture retains at most 2,000 span IDs per scope and
32 concurrent scopes per telemetry client.

## Status

v0.1 — Google Gemini (`google.golang.org/genai`) active wrapping, direct
LLM/retriever/tool helpers, and an isolated private provider. More providers and
explicit framework wrappers will follow.

## Doctor v2

Install the Doctor CLI as shown above before running these commands.

Run the isolated, network-free SDK check:

```bash
neatlogs doctor --local --json
```

Run the authenticated backend path probe:

```bash
NEATLOGS_API_KEY='<project-key>' \
NEATLOGS_ENDPOINT='https://ingest.neatlogs.com' \
neatlogs doctor --probe --json
```

Probe uses the normal `/v1/traces` OTLP route with the
`x-neatlogs-doctor: v1` observability marker, then polls the existing
`/api/traces/v3/:traceId` product route with the same project key. It passes
only when the exact trace is visible and its persisted hierarchy, span
semantics, input/output, versioned metadata, and numeric token totals validate.
HTTP acceptance or exporter flush alone never reports success.

## A/B tests

Use cached assignments, record actual exposures, and explicitly close completed sessions or users. See [A/B testing in Go](AB_TESTING.md) for setup and delivery order.
