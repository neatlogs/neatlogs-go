# Google ADK instrumentation

This example uses explicit wrappers because Google ADK obtains its own spans
from OpenTelemetry's process-global provider, while Neatlogs deliberately keeps
each project on a private provider.

Install both modules:

```bash
go get github.com/neatlogs/neatlogs-go@latest
go get github.com/neatlogs/neatlogs-go/contrib/adk@latest
```

The integration has three deliberate hooks:

```go
config := nladk.InstrumentConfig(llmagent.Config{
    Name:  "weather_agent",
    Model: geminiModel,
    Tools: tools,
})
agent, err := llmagent.New(config)

for event, err := range nladk.Run(
    ctx, runner, userID, sessionID, message, agent.RunConfig{},
) {
    // consume the unchanged ADK event stream
}
```

- `InstrumentConfig` wraps model calls and tool callbacks.
- `Run` creates the workflow root and carries the private trace context through
  the ADK runner.
- `A2AHTTPClient`, `A2AHandler`, `A2ABeforeRequest`, and `A2AAfterRequest`
  preserve context and I/O across A2A boundaries without emitting HTTP spans.

Set `NEATLOGS_API_KEY` and `GOOGLE_API_KEY`, then run:

```bash
go run . -scenario=non-streaming
go run . -scenario=tools
go run . -scenario=all
```

The example covers non-streaming, streaming, tools, sequential, parallel, loop,
A2A, and concurrent executions.
