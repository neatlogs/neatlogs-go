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
if version.Reason == "assigned" && !agents.RecordRun(ctx, version) {
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
