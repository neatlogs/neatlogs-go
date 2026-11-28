# Agent connection (v2)

`NewAgentClient(ABTestConfig{Endpoint, APIKey, Keys})` connects stable agent keys to Customer ID (`end_user`) or Conversation ID (`session`) server enrollment. Existing `NewABTestClient` remains protocol v1. The server freezes the assignment unit and completion policy. Goal-level rollout decisions are not made by this bridge.

```go
agents, err := neatlogs.NewAgentClient(neatlogs.ABTestConfig{
    Endpoint: endpoint, APIKey: apiKey, Keys: []string{"support"},
})
if err != nil { return err }
defer agents.Close() // Application shutdown, after flushing.

// In the handler, before opening its root trace:
version := agents.Choose(ctx, "support", neatlogs.AgentChoiceOptions{
    Unit: customerID,
    Attributes: map[string]any{"environment": "production"},
    Fallback: neatlogs.ABParams{"model": deployedModel},
})
ctx = neatlogs.Identify(ctx, neatlogs.IdentifyOptions{SessionID: conversationID, EndUserID: customerID})
ctx, root, end := neatlogs.Trace(ctx, "reply")
recorded := agents.RecordRun(ctx, version) // Record every actual execution.
if version.Reason == "assigned" && !recorded {
    end()
    return errors.New("unable to record assigned execution")
}
agentErr := runAgent(ctx, version.Params)
end()
if err := neatlogs.Flush(ctx); err != nil { return err }
if conversationFinished { agents.CompleteDecision(ctx, version, root.SpanContext().TraceID().String()) }
if err := agents.Flush(ctx); err != nil { return err }
return agentErr
```

Reuse one client. `Choose` may make bounded HTTP calls when the config or customer/conversation lease is missing/expired; its context controls cancellation. `Start(ctx)` enables periodic config refresh and outbox delivery; `Refresh(ctx)` explicitly observes changes. Attributes accept bounded strings/booleans. The server freezes the first audience snapshot for the selected customer or conversation.

Assignment validity ends at the earlier config/assignment expiry. Transient transport/408/429/5xx refresh failures preserve only the original valid lease. Invalid/revoked config clears it; a known pause serves Current. Remote pause observation is bounded by the60s TTL. Unavailable settings use your explicit deployed fallback and create no experiment receipt. Current/outside-audience/outside-share/paused choices also do not count as tested traffic.

Choose before starting the root trace; record only when actually invoking the selected settings. The adapter tags the private Neatlogs root. Roots marked as simulation/replay cannot produce live receipts. This adapter does not depend on the separate local-replay implementation. For customer assignment, use the customer ID as `Unit` and `EndUserID`, retaining the actual conversation ID as `SessionID`. For conversation assignment, use its ID as `Unit` and `SessionID`. Call `CompleteDecision` after exporting the final trace of each conversation. It sends `conversationComplete: true`; only session/`explicit` assignment also sends `unitComplete: true`. Customer/`fixed_horizon` assignment stays open until the server cutoff and delivery grace, so another conversation can use the same active customer assignment. Preserve the decision through export and completion even if config changes. `Diagnostics()` exposes pending/rejected receipts. Queues and caches are bounded and process-local; flush before closing.

The client negotiates `serving-trace-v1` through the existing endpoints and validates enhanced acknowledgements, including304 refreshes. Legacy servers/policies retain their existing receipt behavior without acquiring enhanced historical coverage. Publications require a compatible deployed SDK/source build that executes returned params. Published decisions validate the advertised publication/fingerprint pair, serving revision and relevant raw attribute digest, and carry no AB assignment identity.

Call `RecordRun` inside the root before every actual execution, including fallback, Current and publications. It records ordinary agent/unit/reason/actual-fingerprint metadata. Its boolean still means a signed AB receipt was queued, so nonassigned choices normally return false. Optional `decision.Cause` and root `decision_cause` are limited to `transport_unavailable`, `invalid_identity` and `unsupported_canonical_params`; unknown causes are omitted. Mutating params before recording rejects new-profile signed/publication attribution. Replay/simulation stays excluded, and no raw exception or credential data is added.

The agent-only scalar encoder preserves backend UTF16 key order, ECMAScript number forms and JSON string escapes, NFC-normalizing parameter string values only. It uses existing standard-library and Unicode dependencies, independently of local replay files. Unsupported Unicode normalization versions, invalid UTF8/lone wire surrogates, or non-lossless native number representations fail closed for new-profile evidence. Legacy policy execution remains unchanged and unsupported ordinary identity remains unknown. Keys and audience strings are never normalized. Entirely unexported executions remain an instrumentation assumption.

Update every instance that serves this agent before starting a new test.

A true recorder result means a signed receipt was queued; it does not mean final telemetry was accepted or primary cohort coverage is complete. Signed identity keeps priority over ordinary tags near attribute limits; known missing signed tags are not queued.
