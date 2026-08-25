# NeatLogs canonical telemetry contract v2

This directory is the public, language-neutral source of truth for telemetry captured by NeatLogs SDKs and accepted by NeatLogs ingestion adapters. The canonical artifact is [`neatlogs-telemetry.schema.json`](neatlogs-telemetry.schema.json).

Contract version: `2.0.0`  
Envelope `schema_version`: `2`  
Canonical namespace: `neatlogs.v2.*`  
Default SDK transport: OTLP HTTP/protobuf

## Where the contract applies

The same schema bytes are vendored into the Python, TypeScript, Go, backend, and wizard repositories. Each repository has a hash gate. Runtime normalizers and doctor output refer to schema version 2; golden fixtures validate the finished normalized envelope, not provider-specific input.

The envelope is the boundary after capture normalization and before masking, attachment selection, batching, and network export:

```text
capture → canonical v2 normalization → mask → inline/reference decision → batch → export
```

Source attributes may remain in `attributes` for compatibility and diagnosis. They cannot silently overwrite canonical fields.

## One field for one meaning

Every envelope has one `kind`, one typed `input`, one typed `output`, one terminal `status`, an optional structured `error`, an optional code location, ownership, wrapper/capture fidelity, field-level provenance, and explicit conflicts.

Kind-specific content lives only under `semantic`:

| Kind | Canonical semantic structure |
| --- | --- |
| `LLM` | request, response choices, usage, stream summary/events |
| `TOOL`, `MCP_TOOL` | definition, requested call, execution result, explicit linkage, optional MCP transport |
| `RETRIEVER` | query, filters, every returned document |
| `RERANKER` | query, input documents, ranked output documents, `top_n` |
| `EMBEDDING` | provider/model, inputs, vectors, dimensions, usage |
| `VECTOR_STORE` | provider, operation, collection, query, documents |
| `MEMORY` | operation, memory ID, scope, value |
| `GUARDRAIL` | name, action, triggered state, score, reason |
| `LOG` | severity, body, logger name |
| `WORKFLOW`, `AGENT`, `CHAIN`, `TASK` | operation, role, metadata, optional recovery projection |

`HTTP` and `UNKNOWN` remain compatibility kinds for externally ingested telemetry. SDKs do not create HTTP spans as their AI semantic model.

## LLM messages, choices, and streams

- Preserve every response choice with its original `choice_index` and finish reason.
- Preserve assistant-requested `tool_calls` inside the assistant response message.
- Model actual tool execution as a separate `TOOL` or `MCP_TOOL` span.
- Accumulate streamed content and tool fragments by `(choice_index, tool_index)`.
- Emit semantic stream events for text, reasoning, tool-call deltas, finish, usage, and error.
- Record exactly one terminal `completion_state`: `not_streamed`, `complete`, `consumer_cancelled`, or `provider_error`. Early consumer termination preserves partial output and must not be represented as a fully completed stream.
- A forensic raw-chunk capture, when enabled, is a bounded compressed media reference. It is not an unbounded event per transport chunk.
- Mark `capture_fidelity=flattened` when the upstream callback exposes only a flattened result. Never invent missing choices.

## Tool identity and linkage

Tool-call ID precedence is:

1. Existing direct `neatlogs.tool.*` or canonical v2 ID.
2. Provider-supplied ID.
3. Deterministic synthetic ID derived from trace ID, requesting LLM span ID, choice index, tool index, normalized name, and arguments digest.

Record the origin in `id_origin`. A synthetic ID must be stable for identical input and must be propagated through runtime context when execution occurs.

Never merge an execution span by name and timing alone. Retain a standalone unlinked TOOL span and report its missing link explicitly.

## Media and overflow references

Every image, audio item, document, video, forensic stream, or oversized object reference records:

- opaque ID;
- SHA-256 digest;
- MIME type;
- byte length;
- source;
- purpose;
- state;
- safe preview, or `null`.

SDKs never receive storage credentials. Presigned object-scoped upload and generic overflow claim-check behavior are delivery concerns built on this reference shape. No integration silently truncates canonical content.

## Conflict precedence and provenance

Canonicalization uses this fixed order:

1. native v2;
2. existing direct NeatLogs attributes;
3. OTel GenAI;
4. OpenInference;
5. provider-specific;
6. accepted external legacy;
7. unknown raw.

For every populated canonical target, append a provenance record containing the source dialect, source key, precedence rank, and action. If two non-empty values disagree, keep the higher-precedence value and append a conflict with a stable reason code. Never silently overwrite.

Unknown attributes remain raw and cannot populate canonical fields without an explicit source-adapter mapping.

## Launch compatibility

The schema’s `x-neatlogs-policy.compatibility` section is normative:

- Direct `neatlogs.*` attributes remain accepted.
- `gen_ai.*` remains accepted at the normalization boundary for OTLP sources.
- OpenInference input/output, LLM, tool, retrieval, and embedding attributes remain for active Python integrations.
- Provider-specific attributes are mapped only in their source adapter.
- OpenLLMetry/Traceloop and OpenLIT attributes are accepted with provenance.
- Unknown attributes are retained raw and counted as schema-drift telemetry; they do not override canonical data.

A compatibility family can be removed only after two releases, at least 30 days of observation with zero mapping hits, migration of every source adapter, and explicit golden-fixture review.

## Root and finalization semantics for launch

- Keep current automatic SDK WORKFLOW roots and completion markers for launch.
- Backend stale-root recovery is a safety net, never the normal completion contract.
- A recovered root is explicitly synthetic and records a recovery reason.
- Recovery must never fabricate `OK`; an absent application result remains `UNSET` unless real error evidence exists.
- Preserve a late genuine root’s original span ID and raw row. The recovery projection records reconciliation instead of substituting the genuine identity.
- Logical-root-only finalization and removal of automatic roots/completion markers are deferred to a separately shadowed post-launch project.

## Launch scope

Included:

- Python, TypeScript, and Go capture fidelity for existing integrations;
- OTLP HTTP/protobuf as the SDK default;
- active Python OpenInference compatibility;
- current Go integrations only;
- existing prompt-client reliability.

Excluded from SDK launch:

- datasets, evaluations, feedback/scoring, experiments;
- new prompt-management SDK APIs;
- new Go integrations;
- an unpublished TypeScript Edge entry point.

## Explicitly deferred

The schema policy records every deferred item. These are not implicit launch blockers:

- logical-root-only finalization and automatic-root/completion-marker removal;
- user-defined export filters;
- runtime tracing toggles;
- new Go integrations;
- TypeScript Edge entry point;
- SDK datasets/evaluations/scoring/experiments.
