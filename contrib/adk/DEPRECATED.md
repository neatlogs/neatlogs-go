# DEPRECATED: `contrib/adk` (Google ADK integration)

**Status: quarantined, non-functional. Do not use in new code.**

## Why

This integration was built when `neatlogs.Init` registered the process-**global**
OpenTelemetry `TracerProvider` (and the global propagator). Under that model:

- Google ADK auto-instruments agent runs on the global provider, so ADK's
  `invoke_agent` / `generate_content` / `execute_tool` spans flowed into Neatlogs
  for free.
- `WrapModel` only had to annotate the **live ADK span** (`trace.SpanFromContext`)
  with request/response message text, because ADK-Go records prompt/completion
  text on the OTel *logs* signal only, never on spans.
- `A2AHTTPClient` / A2A helpers propagated the W3C `traceparent` via the global
  propagator to link cross-process agent runs.

The Neatlogs SDK has since moved to an **isolated, private-provider** design: it
keeps its own `TracerProvider` and W3C propagator and **never registers them
globally**, so it can never export or become the parent of a co-tenant's spans
(e.g. Datadog, Braintrust) and vice versa. This is the SDK's core guarantee.

That guarantee is fundamentally incompatible with ADK's global-provider
auto-instrumentation:

- ADK starts its spans on the **global** provider (which Neatlogs no longer owns).
- `WrapModel` calls `trace.SpanFromContext(ctx)` and finds no **recording
  Neatlogs** span to annotate.
- Result: **zero ADK spans reach Neatlogs.** The `examples/adk` suite fails with
  `no spans captured — ADK did not flow through the global provider`.

This is not a bug to patch — reverting to global registration would abandon the
isolation guarantee. So the integration is quarantined rather than "fixed".

## What was done

- `examples/adk/*_test.go` gated behind the `adk_legacy` build tag, so the
  default `go test ./...` is green. Reproduce the incompatibility with
  `go test -tags adk_legacy ./...` (needs `GOOGLE_API_KEY` for the live suite).
- `WrapModel`, the `adk` package doc, and the A2A helpers carry `Deprecated:`
  godoc notices.
- The code remains compilable and is a separate Go module
  (`github.com/neatlogs/neatlogs-go/contrib/adk`), so its heavy Google `genai` /
  `adk` dependencies never reach root importers, and nothing in the main SDK or
  in cfoai imports it.

## Possible future redesigns (not implemented)

1. **ADK-side injected provider.** If ADK-Go exposes a way to supply a specific
   `TracerProvider` (rather than binding to the global one), pass the private
   Neatlogs provider so ADK spans land on it directly.
2. **Bridge span processor.** Run a second, Neatlogs-owned span processor that
   observes ADK's global-provider spans and re-emits normalized copies onto the
   private provider — accepting that this reads global state.
3. **Explicit Neatlogs spans around ADK calls.** Drop reliance on ADK's
   auto-instrumentation entirely and open Neatlogs spans (via the SDK's own
   helpers) around agent/model/tool calls, mirroring the approach used for the
   Go-native OpenAI/Anthropic, Inkeep RAG, and Hindsight instrumentation.
