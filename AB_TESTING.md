# A/B tests in Go

A live A/B test selects the settings used by your application. The SDK returns settings; your code applies them to the model call. Creating a test in the dashboard alone does not change customer traffic.

For new integrations, call `ChooseABTest` before the trace, then `RecordABTestRun` inside the trace when the chosen agent actually begins. An abandoned choice is not traffic. After the final trace has been exported, call `CompleteABTestDecision` and flush the A/B client. `ABTest` remains available for older integrations and records exposure when called inside a trace.

Use an A/B client alongside your telemetry client. Use your application API origin for `Endpoint`, not the OTLP ingestion endpoint. Register the test keys you will use.

```go
experiments, err := neatlogs.NewABTestClient(neatlogs.ABTestConfig{
    Endpoint: "https://your-neatlogs-app.example",
    APIKey: os.Getenv("NEATLOGS_API_KEY"),
    Keys: []string{"cheaper_support_model"},
})
if err != nil { return err }
defer experiments.Close()
// If startup cannot fetch settings, assignments use the supplied control fallback.
_ = experiments.Start(context.Background())

ctx = neatlogs.WithABTestClient(ctx, experiments)
ctx = neatlogs.Identify(ctx, neatlogs.IdentifyOptions{SessionID: sessionID})
ctx, span, end := neatlogs.Trace(ctx, "support_reply")
choice := neatlogs.ChooseABTest(ctx, "cheaper_support_model", neatlogs.ABAssignmentOptions{
    Unit: sessionID,
    Fallback: neatlogs.ABParams{"model": "your-current-model"},
})
modelName, ok := choice.Params["model"].(string)
if !ok { modelName = "your-current-model" }
if choice.Reason == "assigned" && !neatlogs.RecordABTestRun(ctx, choice) {
    end()
    return errors.New("A/B run could not be recorded")
}
// Pass modelName to the model request your application already makes.
end()

// Export the completed trace before sending its exposure receipt.
if err := neatlogs.Flush(ctx); err != nil { return err }
if err := experiments.Flush(ctx); err != nil {
    // Pending receipts remain queued and retry on the next flush/background tick.
    log.Printf("A/B receipts: %v", err)
}

// Do this once the session's FINAL trace has been exported, not after every turn.
if choice.Reason == "assigned" {
    neatlogs.CompleteABTestDecision(ctx, choice, span.SpanContext().TraceID().String())
}
_ = experiments.Flush(ctx)
```

Set `Unit` to the session or end-user identity configured for the test. Assignment is synchronous and uses cached settings, with the same deterministic hash as the TypeScript and Python SDKs. Invalid or expired settings and a closed client select arm A with your supplied fallback. Temporary network failures, HTTP 408/429 and server errors retain validated settings only until their original expiry; failures never extend it. Revoked access and other invalid responses clear cached settings immediately. A monitored rollout assigns 50% and then 100% to arm B using the same hash and salt. Pausing, rolling back, or reaching the monitoring deadline returns new requests to arm A. Legacy analysis configurations that shipped before staged rollouts retain their configured arm B default. Default assignments do not generate new exposure receipts.

`choice.AllocationVersion` identifies the exact assignment epoch. Queued receipts keep that epoch through retries and later configuration refreshes. Completion reuses the original trace receipt when available. If the client has restarted or forgotten that receipt, pass `AllocationVersion` explicitly in `ABExposureOptions`; completion returns false rather than guessing the current epoch.

`Start` refreshes settings and retries receipts every 60 seconds. Each HTTP request has a three-second timeout, a 64 KiB response limit, and refuses redirects. The queue holds at most 1,000 receipts for up to 24 hours. `Diagnostics` reports pending and rejected counts. Keep the client alive until final receipts are delivered; `Close` stops polling and cancels requests, but does not flush or implicitly close units.

For an external feature flag, use `LogABExposure` or `experiments.LogExposure` with the actual arm and trace ID. Finish those units with source `"external"`. An exposure is evidence that your code used a version, so record the arm actually used rather than the intended allocation.

The Go A/B client has its own lifecycle. Telemetry `Init`, `Flush`, and `Shutdown` do not automatically start or flush it.
