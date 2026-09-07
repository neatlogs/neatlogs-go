# Legacy Google ADK example

This directory is retained only as a compatibility fixture. Google ADK obtains
its tracer from OpenTelemetry's process-global provider, while Neatlogs uses a
private provider so traces cannot leak between projects or other observability
SDKs. Consequently, the old passive-passthrough integration does not send ADK
semantic spans to Neatlogs.

The source and failure-reproduction tests are gated behind the `adk_legacy`
build tag. Do not use this directory as a setup example and do not restore
global-provider ownership to make it pass. Supported Go instrumentation should
use the core SDK's explicit span helpers or an integration that accepts the
Neatlogs provider directly.

The known incompatibility can be reproduced without credentials:

```bash
go test -tags adk_legacy -run '^TestADKPassthrough$' ./...
```

That command is expected to fail with `no spans captured` until Google ADK can
be connected to the private Neatlogs provider.
