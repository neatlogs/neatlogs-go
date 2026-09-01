# Phase 5 Go SDK launch-readiness workflows

This suite exercises Go-specific runtime behavior from application code through hosted persistence and UI rendering.

| Scenario | Go/SDK boundary | Expected trace |
|---|---|---|
| `generator-channel` | Producer goroutine + channel generator + root output | 1 WORKFLOW, 1 AGENT, 5 TOOL = 7 spans. |
| `streaming-complete` | Real Gemini `iter.Seq2` stream fully consumed | 1 WORKFLOW + 1 LLM; numeric usage and `complete` stream state. |
| `streaming-early-stop` | Consumer breaks after the first generated chunk | 1 WORKFLOW + 1 LLM; stream state `consumer_cancelled`; span closes exactly once. |
| `async-fanout` | Six concurrent goroutines sharing the root context | 1 WORKFLOW, 6 AGENT, 6 LLM = 13 spans with intact parents. |
| `retry` | Two deterministic transient failures and a successful third attempt | 1 WORKFLOW, 3 AGENT, 3 LLM = 7 spans; four errored child spans; stable idempotency key. |
| `numeric-pii` | Final-boundary masking and ClickHouse numeric safety | 3 spans; tokens remain `120/50/170` numbers; email and credential are redacted. |
| `multi-batch` | Three explicit flushes while the root remains active | 1 WORKFLOW + 24 TOOL = 25 spans after finalization. |

## Local build and deterministic scenarios

```bash
cd examples/genai
go test ./...
NEATLOGS_LOCAL_ONLY=true go run ./phase5 generator-channel
NEATLOGS_LOCAL_ONLY=true go run ./phase5 async-fanout
NEATLOGS_LOCAL_ONLY=true go run ./phase5 retry
NEATLOGS_LOCAL_ONLY=true go run ./phase5 numeric-pii
NEATLOGS_LOCAL_ONLY=true go run ./phase5 multi-batch
```

The two streaming scenarios call Gemini even when NeatLogs export is disabled.

## Hosted execution

```bash
NEATLOGS_API_KEY=... \
GOOGLE_API_KEY=... \
GEMINI_MODEL=gemini-2.5-flash \
NEATLOGS_ENDPOINT=https://ingest.neatlogs.com \
PHASE5_RUN_ID=release-candidate-001 \
go run ./phase5 all
```

## Workflow-to-UI acceptance

For each trace verify exact span count, unique IDs, one root, complete parentage, root output, stream state, retries/errors, numeric token types, PII masking, finalization status, and payload rendering. Also check that Kafka offsets advance normally and no ClickHouse type rejection or shared retry loop occurs.

Malformed numeric injection, forced Kafka failure, and missing-object recovery are isolated-backend tests. Do not intentionally send them to shared production or staging ingestion.
