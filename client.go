package neatlogs

import (
	"context"
	"fmt"
	"os"
)

// Client is an independent Neatlogs pipeline. Each Client owns its provider,
// exporter, resource, active-span registry, and running-to-closed gate. It can
// run concurrently with process-wide Init and with other Clients.
//
// Activate a Client by attaching it to the context passed through wrappers and
// span helpers:
//
//	client, err := neatlogs.NewClient(ctx, neatlogs.Config{APIKey: key})
//	if err != nil { /* handle */ }
//	defer client.Shutdown(context.Background())
//	ctx = neatlogs.WithClient(ctx, client)
//	ctx, span, end := neatlogs.Trace(ctx, "request")
//	defer end()
//
// A context remains bound to this Client after shutdown; helpers then return
// no-op spans rather than leaking telemetry to the global Init pipeline.
type Client struct {
	runtime *sdkRuntime
}

type clientContextKey struct{}

// NewClient constructs an isolated context-scoped pipeline. Signal handling is
// intentionally process-wide and is only available through Init; Clients never
// register signal.Notify handlers.
func NewClient(ctx context.Context, cfg Config, opts ...Option) (*Client, error) {
	var io initOptions
	for _, opt := range opts {
		opt(&io)
	}
	runtime, endpoint, exportEnabled, err := buildSDKRuntime(ctx, cfg, io)
	if err != nil {
		return nil, err
	}
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "neatlogs: client initialized (workflow=%q, endpoint=%s, export=%v)\n", runtime.workflowName, endpoint.String(), exportEnabled)
	}
	return &Client{runtime: runtime}, nil
}

// WithClient binds client to ctx. All Neatlogs wrappers and span helpers route
// through the bound Client. A nil client leaves ctx unchanged.
func WithClient(ctx context.Context, client *Client) context.Context {
	if client == nil {
		return ctx
	}
	return context.WithValue(ctx, clientContextKey{}, client)
}

// Context is a convenience alias for WithClient(ctx, client).
func (c *Client) Context(ctx context.Context) context.Context {
	return WithClient(ctx, c)
}

// ClientFromContext reports the Client explicitly bound to ctx.
func ClientFromContext(ctx context.Context) (*Client, bool) {
	client, ok := ctx.Value(clientContextKey{}).(*Client)
	return client, ok && client != nil
}

// Flush synchronously flushes only this Client's provider.
func (c *Client) Flush(ctx context.Context) error {
	if c == nil || c.runtime == nil {
		return nil
	}
	return c.runtime.forceFlush(ctx)
}

// Shutdown closes this Client's active spans child-first and shuts down only
// its provider/exporter. It is safe to call concurrently and repeatedly.
func (c *Client) Shutdown(ctx context.Context) error {
	if c == nil || c.runtime == nil {
		return nil
	}
	return c.runtime.shutdown(ctx, "shutdown")
}
