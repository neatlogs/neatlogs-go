---
name: neatlogs-sdk-testing
description: Plan and execute contract-driven testing of the Neatlogs Go SDK and contrib modules from clean external consumers without modifying SDK source during execution.
---

# Neatlogs Go SDK testing

Use this skill for Go SDK release qualification, lifecycle/concurrency testing, Gemini wrapper validation, propagation testing, and published-module developer experience.

## Boundaries

- Record clean worktree, branch, commit, Go version, module versions, OS, and architecture.
- Create external consumer modules in a temporary directory. Do not edit SDK or example code during execution.
- Never push, tag, or publish without separate authorization.
- Keep credentials only in ignored local configuration or process environment and never print them.
- Use the exact endpoint, backend, provider, and model requested. Do not substitute ADK for Gemini or Gemini for Vertex.
- Declare behavior and telemetry expectations before execution.

## PASS requirements

Require intended Go result/error, exact local telemetry, expected `Flush`/shutdown results, exact backend persistence, and persisted hierarchy equality. A nil Go error is not sufficient if export or persistence failed.

## Consumer and module matrix

- Test the root module and `contrib/genai` from a fresh external module.
- Use minimum Go 1.25 and the supported current version on Linux, macOS, and Windows.
- Run compile, `go test`, `go test -race`, `go vet`, and package examples where applicable.
- Verify that root and contrib versions resolve compatibly without repository `replace` directives.

## Initialization and lifecycle matrix

- Default and explicit config.
- Missing, empty, whitespace, and invalid ingestion credentials.
- Environment and explicit endpoints, including normalization and malformed values.
- Duplicate and concurrent `Init` calls.
- Reinitialize after shutdown.
- `Flush` before initialization, twice, and after shutdown.
- Invoke shutdown twice and from concurrent goroutines.
- Start spans during closing and after shutdown.
- Shutdown with active children.
- Cached wrapped client after shutdown.
- Normal exit without shutdown.
- Signal handlers disabled by default, enabled explicitly, and overridden by deprecated disable option.
- `DisableExport` behavior.
- Mark sampling cases `NOT APPLICABLE` unless sampling becomes part of the public Go configuration.

## Go-specific coverage

- Multiple Clients bound to separate contexts/projects and independently shut down.
- Goroutine fanout/fanin with and without context propagation.
- W3C inject/extract and foreign global OpenTelemetry isolation.
- `GenerateContent`, streaming, `EmbedContent`, `CountTokens`, tool calls, and `Raw()` passthrough.
- Nil client/content/config, deadlines, cancellation, partial streams, and early consumer break.
- Gemini versus Vertex backend labeling.
- Deprecated ADK only as compatibility coverage, not as the supported primary path.

## Report

For every case include module/toolchain versions, expected contract, Go result/error, flush/shutdown error, exporter evidence, persistence result, trace ID, valid dashboard URL, race/leak observations, classification, and defect notes.

