# Neatlogs for Google ADK

`contrib/adk` instruments Google ADK without installing a process-global
OpenTelemetry provider.

```bash
go get github.com/neatlogs/neatlogs-go@latest
go get github.com/neatlogs/neatlogs-go/contrib/adk@latest
```

```go
config := nladk.InstrumentConfig(llmagent.Config{
    Name:  "support_agent",
    Model: model,
    Tools: tools,
})
adkAgent, err := llmagent.New(config)

for event, err := range nladk.Run(
    ctx, runner, userID, sessionID, message, agent.RunConfig{},
) {
    // Handle the unchanged ADK event stream.
}
```

`InstrumentConfig` captures model and tool spans. `Run` adds the workflow root,
session identity, input, final output, and private trace context. For remote ADK
agents, use `A2AHTTPClient` and `A2AHandler` for propagation and the A2A request
callbacks for client-side I/O capture. These helpers do not create HTTP spans.

See [`examples/adk`](../../examples/adk) for streaming, tools, workflow agents,
concurrent execution, and A2A.
