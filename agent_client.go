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

// AgentClient reuses the bounded A/B transport and receipt outbox for customer and session v2 admission.
type AgentClient struct {
	*ABTestClient
	agentMu       sync.Mutex
	chooseMu      sync.Mutex
	configMu      sync.Mutex
	publications  map[string]*agentConfiguration
	leases        map[string]AgentDecision
	servingLeases map[string]AgentDecision
	observed      map[string]string
	refreshCauses map[string]AgentDecisionCause
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
	agent := &AgentClient{ABTestClient: transport, publications: map[string]*agentConfiguration{}, leases: map[string]AgentDecision{}, servingLeases: map[string]AgentDecision{}, observed: map[string]string{}, refreshCauses: map[string]AgentDecisionCause{}, recorded: map[string]string{}}
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
	for _, leases := range []map[string]AgentDecision{c.leases, c.servingLeases} {
		for identity, lease := range leases {
			if lease.AgentKey == key {
				delete(leases, identity)
			}
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
	headers := http.Header{agentCapabilityHeader: []string{agentProfile}}
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
	if err == nil && (config.Profile == agentProfile) != (responseHeaders.Get(agentCapabilityHeader) == agentProfile) {
		err = errAgentProtocol
	}
	if err == nil {
		err = config.validate(key, previous, time.Now())
	}
	c.agentMu.Lock()
	defer c.agentMu.Unlock()
	if err != nil {
		c.refreshCauses[key] = agentFailureCause(err, status)
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
	delete(c.refreshCauses, key)
	return nil
}

func agentFailureCause(err error, status int) AgentDecisionCause {
	if errors.Is(err, errAgentCanonical) {
		return "unsupported_canonical_params"
	}
	if errors.Is(err, errABTransient) || status == 408 || status == 429 || status >= 500 {
		return "transport_unavailable"
	}
	return "invalid_identity"
}
func agentDecisionCopy(value AgentDecision) AgentDecision {
	value.Params = abCopyParams(value.Params)
	if value.Serving != nil {
		serving := *value.Serving
		value.Serving = &serving
	}
	return value
}

// Choose admits a customer or session over HTTP when its lease is missing. Call before opening the root trace.
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
			c.agentMu.Lock()
			fallback.Cause = c.refreshCauses[key]
			c.agentMu.Unlock()
			return fallback
		}
		fallback.knownRevision = config.Revision
		if config.Serving != nil {
			fallback.knownUnit = config.Serving.Unit
		} else if config.Live != nil {
			fallback.knownUnit = config.Live.Unit
		}
		if (config.Live == nil || config.Live.Status != "running") && config.Serving == nil {
			reason := "current"
			if config.Live != nil {
				reason = "paused"
			}
			return AgentDecision{agentAssignment: agentAssignment{ProtocolVersion: 2, Profile: config.Profile, AgentKey: key, Revision: config.Revision, ExpiresAt: config.ExpiresAt, Arm: "a", Reason: reason, Params: abCopyParams(config.Current.Params), Fingerprint: config.Current.Fingerprint}, UnitID: options.Unit, knownUnit: fallback.knownUnit, expires: config.expires}
		}
		servingIdentity := ""
		if config.Serving != nil {
			digest, err := agentAttributesDigest(options.Attributes, config.Serving.RequiredAttributeKeys)
			if err != nil {
				fallback.Cause = "unsupported_canonical_params"
				return fallback
			}
			encoded, _ := json.Marshal([]any{key, config.Profile, config.Revision, config.Serving.RevisionID, config.Serving.Digest, options.Unit, digest})
			servingIdentity = string(encoded)
		}
		c.agentMu.Lock()
		lease, exists := c.leases[identity]
		if !exists && servingIdentity != "" {
			lease, exists = c.servingLeases[servingIdentity]
		}
		c.agentMu.Unlock()
		if exists && lease.Revision == config.Revision && lease.expires.After(time.Now()) {
			if config.expires.Before(lease.expires) {
				lease.expires = config.expires
				lease.ExpiresAt = config.expires.UTC().Format(time.RFC3339Nano)
			}
			return agentDecisionCopy(lease)
		}
		attrs := options.Attributes
		if attrs == nil {
			attrs = map[string]any{}
		}
		payload, _ := json.Marshal(map[string]any{"protocolVersion": 2, "revision": config.Revision, "unitId": options.Unit, "attributes": attrs})
		status, responseHeaders, data, err := c.request(ctx, key, "assignments", payload, http.Header{agentCapabilityHeader: []string{agentProfile}})
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
			fallback.Cause = "transport_unavailable"
			return fallback
		}
		result := agentAssignment{}
		if err == nil && status == 200 {
			err = agentDecode(data, &result)
		} else {
			err = errAgentProtocol
		}
		if err == nil && (result.Profile == agentProfile) != (responseHeaders.Get(agentCapabilityHeader) == agentProfile) {
			err = errAgentProtocol
		}
		var expires time.Time
		if err == nil {
			expires, err = result.validate(config, time.Now())
			if err == nil && result.Reason == "published" {
				digest, digestErr := agentAttributesDigest(options.Attributes, config.Serving.RequiredAttributeKeys)
				if digestErr != nil {
					err = digestErr
				} else if result.WireUnitID != options.Unit || result.AttributesDigest != digest {
					err = errAgentProtocol
				}
			}
		}
		c.agentMu.Lock()
		if err != nil {
			fallback.Cause = agentFailureCause(err, status)
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
		if servingIdentity != "" && selected.Reason != "assigned" {
			c.servingLeases[servingIdentity] = selected
			if len(c.servingLeases) > 1000 {
				for key := range c.servingLeases {
					delete(c.servingLeases, key)
					break
				}
			}
		} else {
			if _, exists := c.leases[identity]; !exists {
				c.leaseOrder = append(c.leaseOrder, identity)
			}
			c.leases[identity] = selected
			if len(c.leaseOrder) > 1000 {
				delete(c.leases, c.leaseOrder[0])
				c.leaseOrder = c.leaseOrder[1:]
			}
		}
		c.agentMu.Unlock()
		return agentDecisionCopy(selected)
	}
	return fallback
}

// RecordRun tags the private root and queues exposure immediately before using decision.Params.
func (c *AgentClient) RecordRun(ctx context.Context, decision AgentDecision) bool {
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
	actual, canonicalErr := agentVersionFingerprint(decision.AgentKey, abCopyParams(decision.Params))
	config := c.publications[decision.AgentKey]
	revision, kind := decision.Revision, decision.Unit
	if revision == 0 {
		revision = decision.knownRevision
	}
	if kind == "" {
		kind = decision.knownUnit
	}
	prefix := "neatlogs.agent." + decision.AgentKey + "."
	cause := decision.Cause
	strict := decision.Reason == "published" || config != nil && config.Live != nil && config.Live.AgentProfile == agentProfile
	if cause == "" && canonicalErr != nil {
		cause = "unsupported_canonical_params"
	} else if cause == "" && strict && actual != decision.Fingerprint {
		cause = "invalid_identity"
	}
	observation, _ := json.Marshal([]any{decision.Reason, actual, decision.UnitID, revision, kind})
	signedPrior := c.recorded[identity]
	observationPrior := c.observed[identity]
	if decision.Reason == "assigned" && config != nil && config.Live != nil && config.Live.Status == "running" && config.Revision == decision.Revision && config.expires.After(time.Now()) && decision.expires.After(time.Now()) && !(strict && (canonicalErr != nil || actual != decision.Fingerprint)) && (signedPrior == "" || signedPrior == decision.AssignmentID) && (observationPrior == "" || observationPrior == string(observation)) {
		// Establish the legacy signed identity before ordinary attributes use remaining slots.
		root.SetAttributes(attribute.String(prefix+"assignment_id", decision.AssignmentID), attribute.String(prefix+"fingerprint", decision.Fingerprint))
	}
	root.SetAttributes(attribute.Int(prefix+"serving_trace_version", 1), attribute.String(prefix+"unit_id", decision.UnitID), attribute.String(prefix+"decision_reason", decision.Reason))
	if actual != "" {
		root.SetAttributes(attribute.String(prefix+"served_fingerprint", actual))
	}
	if cause != "" {
		root.SetAttributes(attribute.String(prefix+"decision_cause", string(cause)))
	}
	if revision > 0 {
		root.SetAttributes(attribute.Int(prefix+"config_revision", revision))
	}
	if kind != "" {
		root.SetAttributes(attribute.String(prefix+"unit_kind", kind))
	}

	if prior, exists := c.observed[identity]; exists && prior != string(observation) {
		root.SetAttributes(attribute.String(prefix+"decision_reason", "unavailable"), attribute.String(prefix+"decision_cause", "invalid_identity"), attribute.String(prefix+"assignment_id", ""), attribute.String(prefix+"fingerprint", ""), attribute.String(prefix+"serving_policy_id", ""), attribute.String(prefix+"serving_policy_digest", ""))
		return false
	}
	c.observed[identity] = string(observation)
	if len(c.observed) > 10000 {
		for key := range c.observed {
			delete(c.observed, key)
			break
		}
	}
	if decision.Reason == "published" {
		if canonicalErr == nil && actual == decision.Fingerprint && config != nil && config.Revision == decision.Revision && config.expires.After(time.Now()) && decision.expires.After(time.Now()) && config.Serving != nil && decision.Serving != nil && agentSameServing(&config.Serving.agentServingRef, &decision.Serving.agentServingRef) && config.Serving.allows(decision.Serving.PublicationID, actual) {
			root.SetAttributes(attribute.String(prefix+"serving_policy_id", decision.Serving.PublicationID), attribute.Int(prefix+"serving_policy_revision", decision.Serving.Revision), attribute.String(prefix+"serving_policy_digest", decision.Serving.Digest))
		}
		return false
	}
	if decision.Reason != "assigned" {
		return false
	}
	if config != nil && config.Live != nil && config.Live.AgentProfile == agentProfile && (canonicalErr != nil || actual != decision.Fingerprint) {
		return false
	}
	if config == nil || config.Live == nil || config.Live.Status != "running" || config.Revision != decision.Revision || !config.expires.After(time.Now()) || !decision.expires.After(time.Now()) {
		return false
	}
	if prior := c.recorded[identity]; prior != "" && prior != decision.AssignmentID {
		return false
	}
	signedID, signedFingerprint := "", ""
	for _, attr := range root.Attributes() {
		if string(attr.Key) == prefix+"assignment_id" {
			signedID = attr.Value.AsString()
		}
		if string(attr.Key) == prefix+"fingerprint" {
			signedFingerprint = attr.Value.AsString()
		}
	}
	if signedID != decision.AssignmentID || signedFingerprint != decision.Fingerprint {
		return false
	}
	if !c.queueAgent(decision, traceID, false) {
		return false
	}
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

// CompleteDecision finishes an exported conversation; fixed-horizon customer units stay open.
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
func (c *AgentClient) queueAgent(decision AgentDecision, traceID string, conversationComplete bool) bool {
	if !utf8.ValidString(traceID) || abLength(traceID) < 1 || abLength(traceID) > 128 {
		return false
	}
	identityBytes, _ := json.Marshal([]any{"agent", decision.AgentKey, decision.AssignmentID, traceID, conversationComplete})
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
	payload, _ := json.Marshal(map[string]any{"protocolVersion": 2, "eventId": eventID, "assignmentId": decision.AssignmentID, "unitId": decision.UnitID, "traceId": traceID, "conversationComplete": conversationComplete, "unitComplete": conversationComplete && decision.Unit == "session" && decision.CompletionPolicy == "explicit"})
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
	c.servingLeases = map[string]AgentDecision{}
	c.observed = map[string]string{}
	c.refreshCauses = map[string]AgentDecisionCause{}
	c.recorded = map[string]string{}
}
