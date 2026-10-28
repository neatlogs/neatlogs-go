package neatlogs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

// AgentClient reuses the bounded A/B transport and receipt outbox for session-only v2 admission.
type AgentClient struct {
	*ABTestClient
	agentMu       sync.Mutex
	chooseMu      sync.Mutex
	configMu      sync.Mutex
	publications  map[string]*agentConfiguration
	leases        map[string]AgentDecision
	leaseOrder    []string
	recorded      map[string]string
	recordedOrder []string
}

func NewAgentClient(cfg ABTestConfig) (*AgentClient, error) {
	transport, err := NewABTestClient(cfg)
	if err != nil {
		return nil, err
	}
	transport.apiPath = "/api/v2/experiment-agents"
	agent := &AgentClient{ABTestClient: transport, publications: map[string]*agentConfiguration{}, leases: map[string]AgentDecision{}, recorded: map[string]string{}}
	transport.onAuthorizationFailure = func(key string) { agent.agentMu.Lock(); defer agent.agentMu.Unlock(); agent.revoke(key) }
	return agent, nil
}

// Start refreshes v2 config and retries queued receipts every minute.
func (c *AgentClient) Start(ctx context.Context) error {
	err := c.Refresh(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started && !c.closed {
		c.started = true
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-c.ctx.Done():
					return
				case <-ticker.C:
					_ = c.Refresh(c.ctx)
					_ = c.Flush(c.ctx)
				}
			}
		}()
	}
	return err
}
func (c *AgentClient) revoke(key string) {
	delete(c.publications, key)
	for identity, lease := range c.leases {
		if lease.AgentKey == key {
			delete(c.leases, identity)
		}
	}
}
func (c *AgentClient) Refresh(ctx context.Context) error {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	var result error
	for _, key := range c.keys {
		if err := c.refreshAgent(ctx, key); err != nil {
			result = err
		}
	}
	return result
}
func (c *AgentClient) refreshAgent(ctx context.Context, key string) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errAgentProtocol
	}
	c.agentMu.Lock()
	previous := c.publications[key]
	c.agentMu.Unlock()
	headers := http.Header{}
	if previous != nil && previous.etag != "" {
		headers.Set("If-None-Match", previous.etag)
	}
	status, responseHeaders, data, err := c.request(ctx, key, "config", nil, headers)
	transient := errors.Is(err, errABTransient) || status == 408 || status == 429 || status >= 500
	config := &agentConfiguration{}
	if err == nil {
		if status == 304 && previous != nil {
			*config = *previous
			config.ExpiresAt = responseHeaders.Get("X-AB-Config-Expires-At")
		} else if status == 200 {
			err = agentDecode(data, config)
		} else {
			err = errAgentProtocol
		}
	}
	if err == nil {
		err = config.validate(key, previous, time.Now())
	}
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	if err != nil {
		if !transient || previous == nil || !previous.expires.After(time.Now()) {
			c.revoke(key)
		}
		return err
	}
	c.mu.Lock()
	closed = c.closed
	c.mu.Unlock()
	if closed {
		return errAgentProtocol
	}
	changed := previous == nil
	if previous != nil {
		old, next := *previous, *config
		old.ExpiresAt, next.ExpiresAt = "", ""
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(next)
		changed = string(a) != string(b)
	}
	if changed || config.Live == nil || config.Live.Status != "running" {
		c.revoke(key)
	}
	config.etag = responseHeaders.Get("ETag")
	c.publications[key] = config
	return nil
}

// Choose admits a session over HTTP when its lease is missing. Call before opening the root trace.
func (c *AgentClient) Choose(ctx context.Context, key string, options AgentChoiceOptions) AgentDecision {
	fallback := AgentDecision{agentAssignment: agentAssignment{ProtocolVersion: 2, AgentKey: key, Arm: "a", Reason: "unavailable", Params: abCopyParams(options.Fallback)}, UnitID: options.Unit}
	registered := false
	for _, known := range c.keys {
		registered = registered || known == key
	}
	if !registered || !utf8.ValidString(options.Unit) || abLength(options.Unit) < 1 || abLength(options.Unit) > 512 || !agentValidAttributes(options.Attributes) {
		return fallback
	}
	c.chooseMu.Lock()
	defer c.chooseMu.Unlock()
	identityBytes, _ := json.Marshal([]string{key, options.Unit})
	identity := string(identityBytes)
	for attempt := 0; attempt < 2; attempt++ {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed || ctx.Err() != nil {
			return fallback
		}
		c.agentMu.Lock()
		config := c.publications[key]
		c.agentMu.Unlock()
		if config == nil || !config.expires.After(time.Now()) {
			c.configMu.Lock()
			_ = c.refreshAgent(ctx, key)
			c.configMu.Unlock()
			c.agentMu.Lock()
			config = c.publications[key]
			c.agentMu.Unlock()
		}
		if config == nil || !config.expires.After(time.Now()) {
			return fallback
		}
		if config.Live == nil || config.Live.Status != "running" {
			reason := "current"
			if config.Live != nil {
				reason = "paused"
			}
			return AgentDecision{agentAssignment: agentAssignment{ProtocolVersion: 2, AgentKey: key, Revision: config.Revision, ExpiresAt: config.ExpiresAt, Arm: "a", Reason: reason, Params: abCopyParams(config.Current.Params), Fingerprint: config.Current.Fingerprint}, UnitID: options.Unit, expires: config.expires}
		}
		c.agentMu.Lock()
		lease, exists := c.leases[identity]
		c.agentMu.Unlock()
		if exists && lease.Revision == config.Revision && lease.expires.After(time.Now()) {
			lease.Params = abCopyParams(lease.Params)
			if config.expires.Before(lease.expires) {
				lease.expires = config.expires
				lease.ExpiresAt = config.expires.UTC().Format(time.RFC3339Nano)
			}
			return lease
		}
		attrs := options.Attributes
		if attrs == nil {
			attrs = map[string]any{}
		}
		payload, _ := json.Marshal(map[string]any{"protocolVersion": 2, "revision": config.Revision, "unitId": options.Unit, "attributes": attrs})
		status, _, data, err := c.request(ctx, key, "assignments", payload, nil)
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &failure)
		if status == 409 && failure.Code == "CONFIG_CHANGED" && attempt == 0 {
			c.agentMu.Lock()
			c.revoke(key)
			c.agentMu.Unlock()
			continue
		}
		if errors.Is(err, errABTransient) || status == 408 || status == 429 || status >= 500 {
			return fallback
		}
		result := agentAssignment{}
		if err == nil && status == 200 {
			err = agentDecode(data, &result)
		} else {
			err = errAgentProtocol
		}
		var expires time.Time
		if err == nil {
			expires, err = result.validate(config, time.Now())
		}
		c.agentMu.Lock()
		if err != nil {
			c.revoke(key)
			c.agentMu.Unlock()
			return fallback
		}
		c.mu.Lock()
		closed = c.closed
		c.mu.Unlock()
		if closed || c.publications[key] != config {
			c.agentMu.Unlock()
			return fallback
		}
		selected := AgentDecision{agentAssignment: result, UnitID: options.Unit, expires: expires}
		selected.ExpiresAt = expires.UTC().Format(time.RFC3339Nano)
		if _, exists := c.leases[identity]; !exists {
			c.leaseOrder = append(c.leaseOrder, identity)
		}
		c.leases[identity] = selected
		if len(c.leaseOrder) > 1000 {
			delete(c.leases, c.leaseOrder[0])
			c.leaseOrder = c.leaseOrder[1:]
		}
		c.agentMu.Unlock()
		selected.Params = abCopyParams(selected.Params)
		return selected
	}
	return fallback
}

// RecordRun tags the private root and queues exposure immediately before using decision.Params.
func (c *AgentClient) RecordRun(ctx context.Context, decision AgentDecision) bool {
	if decision.Reason != "assigned" {
		return false
	}
	sc := privateSpanContext(ctx)
	if !sc.IsValid() {
		return false
	}
	traceID := sc.TraceID().String()
	root := abRootSpan(ctx, traceID)
	if root == nil || !root.IsRecording() {
		return false
	}
	for _, attr := range root.Attributes() {
		if string(attr.Key) == "neatlogs.origin" && attr.Value.AsString() == "simulation" || strings.HasPrefix(string(attr.Key), "neatlogs.replay.") {
			return false
		}
	}
	identity := decision.AgentKey + ":" + traceID
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	config := c.publications[decision.AgentKey]
	if config == nil || config.Live == nil || config.Live.Status != "running" || config.Revision != decision.Revision || !config.expires.After(time.Now()) || !decision.expires.After(time.Now()) {
		return false
	}
	if prior := c.recorded[identity]; prior != "" && prior != decision.AssignmentID {
		return false
	}
	if !c.queueAgent(decision, traceID, false) {
		return false
	}
	root.SetAttributes(attribute.String("neatlogs.agent."+decision.AgentKey+".assignment_id", decision.AssignmentID), attribute.String("neatlogs.agent."+decision.AgentKey+".fingerprint", decision.Fingerprint))
	if _, exists := c.recorded[identity]; !exists {
		c.recordedOrder = append(c.recordedOrder, identity)
	}
	c.recorded[identity] = decision.AssignmentID
	if len(c.recordedOrder) > 10000 {
		delete(c.recorded, c.recordedOrder[0])
		c.recordedOrder = c.recordedOrder[1:]
	}
	return true
}
func (c *AgentClient) CompleteDecision(ctx context.Context, decision AgentDecision, traceID string) bool {
	if ctx.Err() != nil || decision.Reason != "assigned" {
		return false
	}
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	if c.recorded[decision.AgentKey+":"+traceID] != decision.AssignmentID {
		return false
	}
	return c.queueAgent(decision, traceID, true)
}
func (c *AgentClient) queueAgent(decision AgentDecision, traceID string, complete bool) bool {
	if !utf8.ValidString(traceID) || abLength(traceID) < 1 || abLength(traceID) > 128 {
		return false
	}
	identityBytes, _ := json.Marshal([]any{"agent", decision.AgentKey, decision.AssignmentID, traceID, complete})
	identity := string(identityBytes)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if _, exists := c.seen[identity]; exists {
		return true
	}
	if len(c.queue) >= 1000 {
		c.rejected++
		return false
	}
	eventID, err := datasetIdempotencyKey("")
	if err != nil {
		return false
	}
	payload, _ := json.Marshal(map[string]any{"protocolVersion": 2, "eventId": eventID, "assignmentId": decision.AssignmentID, "unitId": decision.UnitID, "traceId": traceID, "unitComplete": complete})
	c.seen[identity] = abSeenExposure{eventID: eventID}
	c.seenOrder = append(c.seenOrder, identity)
	if len(c.seenOrder) > 10000 {
		delete(c.seen, c.seenOrder[0])
		c.seenOrder = c.seenOrder[1:]
	}
	c.queue = append(c.queue, abPendingExposure{key: decision.AgentKey, exposure: abExposure{ABExposureOptions: ABExposureOptions{EventID: eventID}}, payload: payload, queuedAt: time.Now()})
	return true
}
func (c *AgentClient) Close() {
	c.ABTestClient.Close()
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	c.publications = map[string]*agentConfiguration{}
	c.leases = map[string]AgentDecision{}
	c.recorded = map[string]string{}
}
