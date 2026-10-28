# Session agent connection (v2)

`NewAgentClient(ABTestConfig{Endpoint, APIKey, Keys})` connects stable agent keys to session-only server enrollment. Existing `NewABTestClient` remains protocol v1. End-user assignment across conversations and goal-level rollout decisions are not supported by this bridge.

```go
agents, err := neatlogs.NewAgentClient(neatlogs.ABTestConfig{
    Endpoint: endpoint, APIKey: apiKey, Keys: []string{"support"},
})
if err != nil { return err }
defer agents.Close() // Application shutdown, after flushing.

// In the handler, before opening its root trace:
version := agents.Choose(ctx, "support", neatlogs.AgentChoiceOptions{
    Unit: conversationID,
    Attributes: map[string]any{"environment": "production"},
    Fallback: neatlogs.ABParams{"model": deployedModel},
})
ctx = neatlogs.Identify(ctx, neatlogs.IdentifyOptions{SessionID: conversationID})
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

Reuse one client. `Choose` may make bounded HTTP calls when the config or session lease is missing/expired; its context controls cancellation. `Start(ctx)` enables periodic config refresh and outbox delivery; `Refresh(ctx)` explicitly observes changes. Attributes accept bounded strings/booleans. The server freezes the first audience snapshot for the session.

Assignment validity ends at the earlier config/assignment expiry. Transient transport/408/429/5xx refresh failures preserve only the original valid lease. Invalid/revoked config clears it; a known pause serves Current. Remote pause observation is bounded by the60s TTL. Unavailable settings use your explicit deployed fallback and create no experiment receipt. Current/outside-audience/outside-share/paused choices also do not count as tested traffic.

Choose before starting the root trace; record only when actually invoking the selected settings. The adapter tags the private Neatlogs root. Roots marked as simulation/replay cannot produce live receipts. This adapter does not depend on the separate local-replay implementation. Complete only after the final trace of a session; do not reuse an end-user ID for later conversations. Preserve the decision through export and completion even if config changes. `Diagnostics()` exposes pending/rejected receipts. Queues and caches are bounded and process-local; flush before closing.
