package neatlogs

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

// ABExposureOptions describes the version used by one trace. Completion is explicit.
type ABExposureOptions struct {
	UnitID       string `json:"unitId"`
	Arm          string `json:"arm"`
	TraceID      string `json:"traceId"`
	EventID      string `json:"eventId"`
	UnitComplete bool   `json:"unitComplete"`
	// AllocationVersion pins an earlier assignment when reporting after a refresh. Zero uses the saved trace receipt or current cache.
	AllocationVersion int `json:"-"`
}
type abExposure struct {
	ABExposureOptions
	AllocationVersion int    `json:"allocationVersion"`
	Source            string `json:"source"`
}
type abSeenExposure struct {
	eventID           string
	allocationVersion int
}
type abPendingExposure struct {
	key      string
	exposure abExposure
	queuedAt time.Time
}
type ABDiagnostics struct {
	Pending  int
	Rejected int
}

var ErrABExposurePending = errors.New("neatlogs: A/B exposure receipts are waiting for delivery")
var ErrABExposureRejected = errors.New("neatlogs: some A/B exposure receipts were rejected")

// LogExposure records an externally selected version without changing assignment.
func (c *ABTestClient) LogExposure(ctx context.Context, key string, options ABExposureOptions) bool {
	return c.queueExposure(ctx, key, options, "external")
}

// RecordRun reports the served version at the start of the agent invocation, inside its trace.
func (c *ABTestClient) RecordRun(ctx context.Context, decision ABDecision) bool {
	if decision.Reason != "assigned" || decision.AllocationVersion == nil {
		return false
	}
	return c.queueExposure(ctx, decision.Key, ABExposureOptions{UnitID: decision.UnitID, Arm: decision.Arm, AllocationVersion: *decision.AllocationVersion}, "sdk")
}

// CompleteDecision closes a unit after its final trace has been exported.
func (c *ABTestClient) CompleteDecision(ctx context.Context, decision ABDecision, traceID string) bool {
	if decision.Reason != "assigned" || decision.AllocationVersion == nil {
		return false
	}
	return c.CompleteUnit(ctx, decision.Key, ABExposureOptions{UnitID: decision.UnitID, Arm: decision.Arm, TraceID: traceID, AllocationVersion: *decision.AllocationVersion}, "sdk")
}

// CompleteUnit closes a session or user only after its last trace has been exported.
// source is "sdk" or "external"; an empty source means "sdk".
func (c *ABTestClient) CompleteUnit(ctx context.Context, key string, options ABExposureOptions, source string) bool {
	if source == "" {
		source = "sdk"
	}
	options.UnitComplete = true
	return c.queueExposure(ctx, key, options, source)
}
func (c *ABTestClient) queueExposure(ctx context.Context, key string, options ABExposureOptions, source string) bool {
	if options.TraceID == "" {
		if sc := privateSpanContext(ctx); sc.IsValid() {
			options.TraceID = sc.TraceID().String()
		}
	}
	if !utf8.ValidString(options.UnitID) || !utf8.ValidString(options.TraceID) || abLength(options.UnitID) < 1 || abLength(options.UnitID) > 1024 || abLength(options.TraceID) < 1 || abLength(options.TraceID) > 255 || (options.Arm != "a" && options.Arm != "b") || (source != "sdk" && source != "external") {
		return false
	}
	id, err := datasetIdempotencyKey(options.EventID)
	if err != nil {
		return false
	}
	options.EventID = id
	identityJSON, _ := json.Marshal([]any{key, options.UnitID, options.TraceID, options.Arm, source, options.UnitComplete})
	identity := string(identityJSON)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	registered := false
	for _, known := range c.keys {
		if key == known {
			registered = true
			break
		}
	}
	if !registered {
		c.mu.Unlock()
		return false
	}
	allocationVersion := options.AllocationVersion
	if options.UnitComplete {
		originalJSON, _ := json.Marshal([]any{key, options.UnitID, options.TraceID, options.Arm, source, false})
		if original, exists := c.seen[string(originalJSON)]; exists {
			if allocationVersion != 0 && original.allocationVersion != 0 && allocationVersion != original.allocationVersion {
				c.mu.Unlock()
				return false
			}
			if allocationVersion == 0 {
				allocationVersion = original.allocationVersion
			}
		}
	}
	if options.UnitComplete && allocationVersion == 0 {
		c.mu.Unlock()
		return false
	}
	if allocationVersion == 0 {
		allocationVersion = 1
		if config, exists := c.configs[key]; exists {
			allocationVersion = config.AllocationVersion
		}
	}
	if allocationVersion < 1 || allocationVersion > 2147483647 {
		c.mu.Unlock()
		return false
	}
	if existing, exists := c.seen[identity]; exists {
		if existing.allocationVersion != allocationVersion {
			c.seen[identity] = abSeenExposure{eventID: existing.eventID, allocationVersion: 0}
		}
		c.mu.Unlock()
		return existing.allocationVersion == allocationVersion
	}
	for _, pending := range c.queue {
		if pending.exposure.EventID == id {
			c.mu.Unlock()
			return false
		}
	}
	if len(c.queue) >= 1000 {
		c.rejected++
		c.mu.Unlock()
		return false
	}
	c.seen[identity] = abSeenExposure{eventID: id, allocationVersion: allocationVersion}
	c.seenOrder = append(c.seenOrder, identity)
	if len(c.seenOrder) > 10000 {
		delete(c.seen, c.seenOrder[0])
		c.seenOrder = c.seenOrder[1:]
	}
	c.queue = append(c.queue, abPendingExposure{key: key, exposure: abExposure{ABExposureOptions: options, AllocationVersion: allocationVersion, Source: source}, queuedAt: time.Now()})
	c.mu.Unlock()
	abTagTrace(ctx, key, options.TraceID, options.Arm)
	return true
}
func abTagTrace(ctx context.Context, key, traceID, arm string) {
	binding, _ := ctx.Value(privateTraceContextKey{}).(privateTraceContext)
	if binding.owner == nil || !binding.spanContext.IsValid() || binding.spanContext.TraceID().String() != traceID {
		return
	}
	registry := binding.owner.lifecycle
	registry.mu.Lock()
	span := registry.spans[binding.spanContext.SpanID()]
	if span != nil {
		seen := map[string]bool{}
		for span.Parent().IsValid() {
			id := span.Parent().SpanID()
			if seen[id.String()] {
				break
			}
			seen[id.String()] = true
			parent := registry.spans[id]
			if parent == nil || parent.SpanContext().TraceID().String() != traceID {
				break
			}
			span = parent
		}
	}
	registry.mu.Unlock()
	if span != nil {
		span.SetAttributes(attribute.String("neatlogs.ab."+key, arm))
	}
}

// Flush sends receipts in order. Trace-finalization delays preserve the queue.
// It never ends a unit implicitly and retains the same event ID on retries.
func (c *ABTestClient) Flush(ctx context.Context) error {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.closed || len(c.queue) == 0 {
			c.mu.Unlock()
			break
		}
		item := c.queue[0]
		if time.Since(item.queuedAt) > 24*time.Hour {
			c.queue = c.queue[1:]
			c.rejected++
			c.mu.Unlock()
			continue
		}
		c.mu.Unlock()
		body, _ := json.Marshal(item.exposure)
		status, _, data, err := c.request(ctx, item.key, "exposures", body, nil)
		if err != nil {
			break
		}
		var response struct {
			Accepted bool   `json:"accepted"`
			Code     string `json:"code"`
		}
		parsed := json.Unmarshal(data, &response) == nil
		accepted := status >= 200 && status < 300 && parsed && response.Accepted
		rejected := status == 400 || status == 401 || status == 403 || status == 404 || status == 422 || status == 409 && parsed && response.Code != "" && response.Code != "AB_TRACE_NOT_READY"
		if !accepted && !rejected {
			break
		}
		c.mu.Lock()
		if len(c.queue) > 0 && c.queue[0].exposure.EventID == item.exposure.EventID {
			c.queue = c.queue[1:]
			if rejected {
				c.rejected++
			}
		}
		c.mu.Unlock()
	}
	diagnostics := c.Diagnostics()
	if diagnostics.Pending > 0 {
		return ErrABExposurePending
	}
	if diagnostics.Rejected > 0 {
		return ErrABExposureRejected
	}
	return nil
}
func (c *ABTestClient) Diagnostics() ABDiagnostics {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ABDiagnostics{Pending: len(c.queue), Rejected: c.rejected}
}

type abClientContextKey struct{}

func WithABTestClient(ctx context.Context, client *ABTestClient) context.Context {
	return context.WithValue(ctx, abClientContextKey{}, client)
}
func ABTest(ctx context.Context, key string, options ABAssignmentOptions) ABAssignment {
	if client, ok := ctx.Value(abClientContextKey{}).(*ABTestClient); ok && client != nil {
		return client.Assign(ctx, key, options)
	}
	return ABAssignment{Key: key, Arm: "a", Params: abCopyParams(options.Fallback), Reason: "unavailable"}
}
func ChooseABTest(ctx context.Context, key string, options ABAssignmentOptions) ABDecision {
	if client, ok := ctx.Value(abClientContextKey{}).(*ABTestClient); ok && client != nil {
		return client.Choose(key, options)
	}
	return ABDecision{ABAssignment: ABAssignment{Key: key, Arm: "a", Params: abCopyParams(options.Fallback), Reason: "unavailable"}, UnitID: options.Unit}
}
func RecordABTestRun(ctx context.Context, decision ABDecision) bool {
	client, _ := ctx.Value(abClientContextKey{}).(*ABTestClient)
	return client != nil && client.RecordRun(ctx, decision)
}
func CompleteABTestDecision(ctx context.Context, decision ABDecision, traceID string) bool {
	client, _ := ctx.Value(abClientContextKey{}).(*ABTestClient)
	return client != nil && client.CompleteDecision(ctx, decision, traceID)
}
func LogABExposure(ctx context.Context, key string, options ABExposureOptions) bool {
	client, _ := ctx.Value(abClientContextKey{}).(*ABTestClient)
	return client != nil && client.LogExposure(ctx, key, options)
}
func CompleteABTestUnit(ctx context.Context, key string, options ABExposureOptions, source string) bool {
	client, _ := ctx.Value(abClientContextKey{}).(*ABTestClient)
	return client != nil && client.CompleteUnit(ctx, key, options, source)
}
