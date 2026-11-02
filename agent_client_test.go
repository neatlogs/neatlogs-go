package neatlogs

import (
	"context"
	"encoding/json"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

type agentFixture struct {
	Enhanced         map[string]map[string]any `json:"enhanced"`
	Config           map[string]any            `json:"config"`
	Assignment       map[string]any            `json:"assignment"`
	AttributeVectors []struct {
		Attributes map[string]any `json:"attributes"`
		Valid      bool           `json:"valid"`
	} `json:"attributeVectors"`
	UnitVectors []struct {
		Unit  string `json:"unit"`
		Valid bool   `json:"valid"`
	} `json:"unitVectors"`
	CompletionVectors []struct {
		Unit             string `json:"unit"`
		CompletionPolicy string `json:"completionPolicy"`
		Valid            bool   `json:"valid"`
	} `json:"completionVectors"`
	CustomerJourney struct {
		UnitID string `json:"unitId"`
		Traces []struct {
			SessionID            string `json:"sessionId"`
			ConversationComplete bool   `json:"conversationComplete"`
		} `json:"traces"`
	} `json:"customerJourney"`
}

func agentVectors(t *testing.T) agentFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/agent-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var result agentFixture
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

type agentRequest struct {
	Path       string
	Body       map[string]any
	Capability string
}
type agentTestState struct {
	mu                                             sync.Mutex
	config, assignment                             map[string]any
	configStatus, assignmentStatus, exposureStatus int
	changed                                        bool
	acknowledge                                    bool
	assignmentForAttributes                        func(map[string]any) map[string]any
	requests                                       []agentRequest
}

func (s *agentTestState) change(fn func(*agentTestState)) { s.mu.Lock(); defer s.mu.Unlock(); fn(s) }
func agentTestClient(t *testing.T, configSeconds, assignmentSeconds int) (*AgentClient, *agentTestState) {
	t.Helper()
	fixture := agentVectors(t)
	now := time.Now()
	fixture.Config["expiresAt"] = now.Add(time.Duration(configSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
	fixture.Assignment["expiresAt"] = now.Add(time.Duration(assignmentSeconds) * time.Second).UTC().Format(time.RFC3339Nano)
	state := &agentTestState{config: fixture.Config, assignment: fixture.Assignment, configStatus: 200, assignmentStatus: 200, exposureStatus: 200}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		state.requests = append(state.requests, agentRequest{Path: r.URL.Path, Body: body, Capability: r.Header.Get(agentCapabilityHeader)})
		if state.acknowledge {
			w.Header().Set(agentCapabilityHeader, agentProfile)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/config") {
			w.Header().Set("ETag", "\"agent\"")
			w.Header().Set("X-AB-Config-Expires-At", state.config["expiresAt"].(string))
			w.WriteHeader(state.configStatus)
			_ = json.NewEncoder(w).Encode(state.config)
		} else if strings.HasSuffix(r.URL.Path, "/assignments") {
			if state.changed {
				state.changed = false
				state.config["revision"] = float64(2)
				state.assignment["revision"] = float64(2)
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "CONFIG_CHANGED"})
			} else {
				w.WriteHeader(state.assignmentStatus)
				response := state.assignment
				if state.assignmentForAttributes != nil {
					response = state.assignmentForAttributes(body)
				}
				_ = json.NewEncoder(w).Encode(response)
			}
		} else {
			w.WriteHeader(state.exposureStatus)
			response := map[string]any{"accepted": true}
			if state.exposureStatus == 409 {
				response = map[string]any{"code": "AB_TRACE_NOT_READY"}
			}
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	client, err := NewAgentClient(ABTestConfig{Endpoint: server.URL, APIKey: "test-key", Keys: []string{"support"}})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, state
}
func agentChoose(client *AgentClient) AgentDecision {
	return client.Choose(context.Background(), "support", AgentChoiceOptions{Unit: "session:42", Attributes: map[string]any{"environment": "production", "premium": true}, Fallback: ABParams{"model": "deployed"}})
}
func TestAgentSharedVectors(t *testing.T) {
	fixture := agentVectors(t)
	for _, vector := range fixture.CompletionVectors {
		fixture.Config["expiresAt"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		fixture.Config["live"].(map[string]any)["unit"] = vector.Unit
		fixture.Config["live"].(map[string]any)["completionPolicy"] = vector.CompletionPolicy
		data, _ := json.Marshal(fixture.Config)
		var config agentConfiguration
		err := agentDecode(data, &config)
		if err == nil {
			err = config.validate("support", nil, time.Now())
		}
		if (err == nil) != vector.Valid {
			t.Fatalf("completion policy mismatch %+v: %v", vector, err)
		}
	}
	for _, v := range fixture.AttributeVectors {
		if agentValidAttributes(v.Attributes) != v.Valid {
			t.Fatalf("attributes mismatch %+v", v)
		}
	}
	for _, v := range fixture.UnitVectors {
		valid := abLength(v.Unit) > 0 && abLength(v.Unit) <= 512
		if valid != v.Valid {
			t.Fatalf("unit mismatch %+v", v)
		}
	}
}
func TestAgentEarlierLeaseAndTransientRefresh(t *testing.T) {
	for _, seconds := range [][2]int{{10, 60}, {60, 10}} {
		t.Run(time.Duration(seconds[0]).String(), func(t *testing.T) {
			client, state := agentTestClient(t, seconds[0], seconds[1])
			decision := agentChoose(client)
			if decision.Reason != "assigned" || decision.Arm != "b" || time.Until(decision.expires) > 11*time.Second {
				t.Fatalf("unexpected lease %+v", decision)
			}
			original := decision.expires
			state.change(func(s *agentTestState) { s.configStatus = 503; s.assignmentStatus = 503 })
			_ = client.Refresh(context.Background())
			if result := agentChoose(client); result.Reason != "assigned" || !result.expires.Equal(original) {
				t.Fatal("transient refresh extended or dropped lease")
			}
			client.agentMu.Lock()
			for key, lease := range client.leases {
				lease.expires = time.Now().Add(-time.Second)
				client.leases[key] = lease
			}
			if seconds[0] == 10 {
				client.publications["support"].expires = time.Now().Add(-time.Second)
			}
			client.agentMu.Unlock()
			if result := agentChoose(client); result.Reason != "unavailable" || result.Params["model"] != "deployed" {
				t.Fatal("expired assignment served")
			}
		})
	}
}
func TestAgentConfigRevocation(t *testing.T) {
	for _, status := range []int{401, 403, 404, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, state := agentTestClient(t, 60, 60)
			_ = agentChoose(client)
			state.change(func(s *agentTestState) { s.configStatus = status })
			_ = client.Refresh(context.Background())
			if agentChoose(client).Reason != "unavailable" {
				t.Fatal("revoked config served")
			}
		})
	}
}
func TestAgentPauseAndInvalidCompletionPolicy(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	_ = agentChoose(client)
	state.change(func(s *agentTestState) {
		s.config["revision"] = float64(2)
		s.config["live"].(map[string]any)["status"] = "paused"
	})
	_ = client.Refresh(context.Background())
	if result := agentChoose(client); result.Reason != "paused" || result.Params["model"] != "current" {
		t.Fatal("pause did not serve current")
	}
	state.change(func(s *agentTestState) {
		s.config["revision"] = float64(3)
		s.config["live"].(map[string]any)["unit"] = "end_user"
	})
	_ = client.Refresh(context.Background())
	if agentChoose(client).Reason != "unavailable" {
		t.Fatal("invalid completion policy served")
	}
}
func TestAgentChangedPublicationAndTypedAttributes(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	state.change(func(s *agentTestState) { s.changed = true })
	decision := agentChoose(client)
	if decision.Reason != "assigned" || decision.Revision != 2 {
		t.Fatalf("change retry failed %+v", decision)
	}
	state.change(func(s *agentTestState) {
		count := 0
		for _, request := range s.requests {
			if !strings.HasPrefix(request.Path, "/api/v2/experiment-agents/") {
				t.Error("wrong protocol path")
			}
			if strings.HasSuffix(request.Path, "/assignments") {
				count++
				if request.Body["attributes"].(map[string]any)["premium"] != true {
					t.Error("attribute changed")
				}
			}
		}
		if count != 2 {
			t.Errorf("assignments %d", count)
		}
	})
}
func TestAgentInvalidReceiptFailsClosed(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	state.change(func(s *agentTestState) { s.assignment["fingerprint"] = strings.Repeat("d", 64) })
	if agentChoose(client).Reason != "unavailable" {
		t.Fatal("mismatched version served")
	}
	state.change(func(s *agentTestState) {
		s.assignment["fingerprint"] = strings.Repeat("b", 64)
		s.config["futureTargeting"] = true
	})
	if agentChoose(client).Reason != "unavailable" {
		t.Fatal("unknown schema served")
	}
}
func TestAgentRootAttributionRetriesAndCompletion(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	decision := agentChoose(client)
	if client.Diagnostics().Pending != 0 || client.RecordRun(context.Background(), decision) {
		t.Fatal("choice counted without execution")
	}
	sink := tracetest.NewInMemoryExporter()
	telemetry, err := NewClient(context.Background(), Config{WorkflowName: "agent"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	ctx := Identify(telemetry.Context(context.Background()), IdentifyOptions{SessionID: "session:42"})
	ctx, root, endRoot := Trace(ctx, "root")
	child, _, endChild := StartSpan(ctx, "nested", "TOOL")
	if !client.RecordRun(child, decision) || !client.RecordRun(child, decision) {
		t.Fatal("root recording failed")
	}
	endChild()
	endRoot()
	_ = telemetry.Flush(ctx)
	found := false
	for _, span := range sink.GetSpans() {
		for _, attr := range span.Attributes {
			if string(attr.Key) == "neatlogs.agent.support.assignment_id" {
				if span.SpanContext.SpanID() != root.SpanContext().SpanID() || attr.Value.AsString() != decision.AssignmentID {
					t.Fatal("wrong root attribution")
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("missing root receipt")
	}
	state.change(func(s *agentTestState) { s.exposureStatus = 409 })
	if client.Flush(ctx) == nil {
		t.Fatal("pending trace not retained")
	}
	state.change(func(s *agentTestState) { s.exposureStatus = 200 })
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	decision.expires = time.Now().Add(-time.Second)
	if !client.CompleteDecision(ctx, decision, root.SpanContext().TraceID().String()) {
		t.Fatal("original closure rejected")
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	state.change(func(s *agentTestState) {
		var sent []map[string]any
		for _, r := range s.requests {
			if strings.HasSuffix(r.Path, "/exposures") {
				sent = append(sent, r.Body)
			}
		}
		if len(sent) != 3 || sent[0]["eventId"] != sent[1]["eventId"] || sent[2]["assignmentId"] != decision.AssignmentID || sent[2]["unitComplete"] != true || sent[2]["conversationComplete"] != true {
			t.Fatalf("incorrect receipts %+v", sent)
		}
	})
}

func TestAgentTransientStatusPreservesOriginalLease(t *testing.T) {
	for _, status := range []int{408, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, state := agentTestClient(t, 60, 60)
			original := agentChoose(client)
			state.change(func(s *agentTestState) { s.configStatus = status })
			_ = client.Refresh(context.Background())
			result := agentChoose(client)
			if result.Reason != "assigned" || !result.expires.Equal(original.expires) {
				t.Fatal("transient error changed lease")
			}
		})
	}
}

func TestAgentConcurrentAdmissionAndShorterConfigLease(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	results := make(chan AgentDecision, 2)
	for range 2 {
		go func() { results <- agentChoose(client) }()
	}
	first, second := <-results, <-results
	if first.AssignmentID != second.AssignmentID || first.Reason != "assigned" {
		t.Fatal("concurrent admission disagreed")
	}
	deadline := time.Now().Add(3 * time.Second)
	state.change(func(s *agentTestState) {
		count := 0
		for _, r := range s.requests {
			if strings.HasSuffix(r.Path, "/assignments") {
				count++
			}
		}
		if count != 1 {
			t.Errorf("admissions %d", count)
		}
		s.config["expiresAt"] = deadline.UTC().Format(time.RFC3339Nano)
		s.configStatus = 304
	})
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := agentChoose(client); !result.expires.Equal(deadline) {
		t.Fatal("renewal ignored earlier config expiry")
	}
}
func TestAgentRejectsReplayRoot(t *testing.T) {
	client, _ := agentTestClient(t, 60, 60)
	decision := agentChoose(client)
	sink := tracetest.NewInMemoryExporter()
	telemetry, err := NewClient(context.Background(), Config{WorkflowName: "agent-replay"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	ctx, root, end := Trace(telemetry.Context(context.Background()), "replay")
	defer end()
	root.SetAttributes(attribute.String("neatlogs.origin", "simulation"))
	if client.RecordRun(ctx, decision) || client.Diagnostics().Pending != 0 {
		t.Fatal("replay entered live evidence")
	}
}

func TestAgentNaturalExpiryWithoutRevisionBump(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	_ = agentChoose(client)
	state.change(func(s *agentTestState) { s.config["live"] = nil })
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if result := agentChoose(client); result.Reason != "current" || result.Params["model"] != "current" {
		t.Fatal("natural expiry failed")
	}
}

func TestAgentCustomerConversationsCompleteWithoutClosingCustomer(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	fixture := agentVectors(t)
	state.change(func(s *agentTestState) {
		s.config["live"].(map[string]any)["unit"] = "end_user"
		s.config["live"].(map[string]any)["completionPolicy"] = "fixed_horizon"
		s.assignment["unit"] = "end_user"
		s.assignment["completionPolicy"] = "fixed_horizon"
	})
	sink := tracetest.NewInMemoryExporter()
	telemetry, err := NewClient(context.Background(), Config{WorkflowName: "customer"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	traceIDs := map[string]bool{}
	for index, step := range fixture.CustomerJourney.Traces {
		selected := client.Choose(context.Background(), "support", AgentChoiceOptions{Unit: fixture.CustomerJourney.UnitID, Attributes: map[string]any{"environment": "production", "premium": true}, Fallback: ABParams{"model": "deployed"}})
		if selected.Reason != "assigned" {
			t.Fatalf("customer assignment rejected %+v", selected)
		}
		data, _ := json.Marshal(selected.agentAssignment)
		var wire map[string]any
		_ = json.Unmarshal(data, &wire)
		if wire["unit"] != "end_user" || wire["completionPolicy"] != "fixed_horizon" {
			t.Fatalf("missing frozen completion policy %s", data)
		}
		ctx := Identify(telemetry.Context(context.Background()), IdentifyOptions{SessionID: step.SessionID, EndUserID: fixture.CustomerJourney.UnitID})
		ctx, root, end := Trace(ctx, "customer-turn")
		if !client.RecordRun(ctx, selected) || !client.RecordRun(ctx, selected) {
			t.Fatal("customer run not recorded")
		}
		traceID := root.SpanContext().TraceID().String()
		traceIDs[traceID] = true
		end()
		if err := telemetry.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if index == 2 {
			state.change(func(s *agentTestState) {
				s.config["revision"] = float64(2)
				s.config["live"].(map[string]any)["unit"] = "session"
				s.config["live"].(map[string]any)["completionPolicy"] = "explicit"
			})
			if err := client.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if step.ConversationComplete {
			if !client.CompleteDecision(ctx, selected, traceID) || !client.CompleteDecision(ctx, selected, traceID) {
				t.Fatal("conversation completion rejected")
			}
		}
		if err := client.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var roots tracetest.SpanStubs
	for _, span := range sink.GetSpans() {
		if !span.Parent.IsValid() {
			roots = append(roots, span)
		}
	}
	if len(roots) != 3 || len(traceIDs) != 3 {
		t.Fatalf("unexpected traces %d / %d", len(roots), len(traceIDs))
	}
	for i, root := range roots {
		attributes := map[string]string{}
		for _, attr := range root.Attributes {
			attributes[string(attr.Key)] = attr.Value.AsString()
		}
		if attributes["neatlogs.session.id"] != fixture.CustomerJourney.Traces[i].SessionID || attributes["neatlogs.end_user.id"] != fixture.CustomerJourney.UnitID || attributes["neatlogs.agent.support.assignment_id"] != fixture.Assignment["assignmentId"] {
			t.Fatalf("wrong root identity %+v", attributes)
		}
	}
	state.change(func(s *agentTestState) {
		var admissions, sent []map[string]any
		for _, request := range s.requests {
			if strings.HasSuffix(request.Path, "/assignments") {
				admissions = append(admissions, request.Body)
			}
			if strings.HasSuffix(request.Path, "/exposures") {
				sent = append(sent, request.Body)
			}
		}
		if len(admissions) != 1 || admissions[0]["unitId"] != fixture.CustomerJourney.UnitID || len(admissions[0]) != 4 {
			t.Fatalf("incorrect admission %+v", admissions)
		}
		if len(sent) != 5 {
			t.Fatalf("incorrect receipts %+v", sent)
		}
		completions := []bool{false, false, true, false, true}
		events := map[string]bool{}
		for i, receipt := range sent {
			events[receipt["eventId"].(string)] = true
			if len(receipt) != 7 || receipt["protocolVersion"] != float64(2) || receipt["assignmentId"] != fixture.Assignment["assignmentId"] || receipt["unitId"] != fixture.CustomerJourney.UnitID || !traceIDs[receipt["traceId"].(string)] || receipt["conversationComplete"] != completions[i] || receipt["unitComplete"] != false {
				t.Fatalf("incorrect customer receipt %+v", receipt)
			}
		}
		if len(events) != 5 {
			t.Fatal("duplicate event identities")
		}
	})
}

func TestAgentAssignmentPolicyMustMatchPublication(t *testing.T) {
	client, state := agentTestClient(t, 60, 60)
	for _, policy := range []map[string]any{{"unit": "end_user", "completionPolicy": "fixed_horizon"}, {"unit": "session", "completionPolicy": "unknown"}, {"unit": "session"}} {
		state.change(func(s *agentTestState) {
			delete(s.assignment, "completionPolicy")
			for key, value := range policy {
				s.assignment[key] = value
			}
		})
		if agentChoose(client).Reason != "unavailable" {
			t.Fatalf("invalid assignment policy served %+v", policy)
		}
	}
}

func TestAgentCanonicalVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/agent-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		CanonicalVectors struct {
			AgentKey string `json:"agentKey"`
			Vectors  []struct {
				Label          string   `json:"label"`
				Params         ABParams `json:"params"`
				Fingerprint    string   `json:"fingerprint"`
				NumberBits     string   `json:"numberBits"`
				WireParamsJSON string   `json:"wireParamsJson"`
				UnicodeValid   *bool    `json:"profileUnicodeValid"`
				NFCValid       *bool    `json:"goNfcSupported"`
			} `json:"vectors"`
			Attributes []struct {
				Label      string         `json:"label"`
				Attributes map[string]any `json:"attributes"`
				Digest     string         `json:"digest"`
			} `json:"attributes"`
		} `json:"canonicalVectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.CanonicalVectors.Vectors {
		t.Run(vector.Label, func(t *testing.T) {
			if vector.UnicodeValid != nil && !*vector.UnicodeValid {
				if agentWireUnicode([]byte(vector.WireParamsJSON)) {
					t.Fatal("accepted lone surrogate")
				}
				return
			}
			if vector.NumberBits != "" {
				bits, err := strconv.ParseUint(vector.NumberBits, 16, 64)
				if err != nil {
					t.Fatal(err)
				}
				vector.Params["n"] = math.Float64frombits(bits)
			}
			actual, err := agentVersionFingerprint(fixture.CanonicalVectors.AgentKey, vector.Params)
			if vector.NFCValid != nil && !*vector.NFCValid && !unicode.IsGraphic('\U000105d2') {
				if err == nil {
					t.Fatal("expected unsupported Unicode NFC")
				}
				return
			}
			if err != nil || actual != vector.Fingerprint {
				t.Fatalf("canonical identity %q %v", actual, err)
			}
		})
	}
	for _, vector := range fixture.CanonicalVectors.Attributes {
		actual, err := agentAttributesDigest(vector.Attributes, nil)
		if err != nil || actual != vector.Digest {
			t.Fatalf("attribute identity %s %q %v", vector.Label, actual, err)
		}
	}
	for _, value := range []any{math.NaN(), math.Inf(1), json.Number("9007199254740993"), string([]byte{0xff})} {
		if _, err := agentVersionFingerprint("support", ABParams{"n": value}); err == nil {
			t.Fatalf("accepted unsupported scalar %v", value)
		}
	}
}

func agentEnhancedClient(t *testing.T, live bool) (*AgentClient, *agentTestState) {
	t.Helper()
	client, state := agentTestClient(t, 60, 60)
	fixture := agentVectors(t)
	state.change(func(s *agentTestState) {
		s.config = fixture.Enhanced["config"]
		if !live {
			s.config["live"] = nil
		}
		s.config["expiresAt"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		if live {
			s.assignment = fixture.Enhanced["assignment"]
		} else {
			s.assignment = fixture.Enhanced["published"]
		}
		s.assignment["expiresAt"] = s.config["expiresAt"]
		s.acknowledge = true
	})
	return client, state
}
func agentObservedRun(t *testing.T, client *AgentClient, decision AgentDecision, replay bool) (bool, map[string]any) {
	t.Helper()
	sink := tracetest.NewInMemoryExporter()
	telemetry, err := NewClient(context.Background(), Config{WorkflowName: "profile"}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	ctx := Identify(telemetry.Context(context.Background()), IdentifyOptions{SessionID: "session:42", EndUserID: "customer:42"})
	ctx, root, end := Trace(ctx, "profile-run")
	if replay {
		root.SetAttributes(attribute.String("neatlogs.origin", "simulation"))
	}
	accepted := client.RecordRun(ctx, decision)
	end()
	if err := telemetry.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	attrs := map[string]any{}
	for _, span := range sink.GetSpans() {
		if span.SpanContext.SpanID() == root.SpanContext().SpanID() {
			for _, a := range span.Attributes {
				attrs[string(a.Key)] = a.Value.AsInterface()
			}
		}
	}
	return accepted, attrs
}
func TestAgentEnhancedCurrentA(t *testing.T) {
	client, state := agentEnhancedClient(t, true)
	state.change(func(s *agentTestState) {
		current := s.config["live"].(map[string]any)["current"].(map[string]any)
		s.assignment["arm"] = "a"
		s.assignment["params"] = current["params"]
		s.assignment["fingerprint"] = current["fingerprint"]
	})
	decision := agentChoose(client)
	if decision.Reason != "assigned" || decision.Arm != "a" || decision.Fingerprint == state.config["current"].(map[string]any)["fingerprint"] {
		t.Fatalf("incorrect live A %+v", decision)
	}
	state.change(func(s *agentTestState) {
		for _, request := range s.requests {
			if request.Capability != agentProfile {
				t.Errorf("missing profile header %s", request.Path)
			}
		}
	})
	accepted, attrs := agentObservedRun(t, client, decision, false)
	if !accepted || attrs["neatlogs.agent.support.served_fingerprint"] != decision.Fingerprint {
		t.Fatalf("incorrect ordinary identity %+v", attrs)
	}
}
func TestAgentEnhancedLegacyPolicyKeepsReceipts(t *testing.T) {
	client, state := agentEnhancedClient(t, true)
	fixture := agentVectors(t)
	state.change(func(s *agentTestState) {
		expiry := s.config["expiresAt"]
		s.config = fixture.Enhanced["legacyConfig"]
		s.config["expiresAt"] = expiry
		s.assignment = fixture.Enhanced["legacyAssignment"]
		s.assignment["expiresAt"] = expiry
	})
	selected := agentChoose(client)
	if selected.Reason != "assigned" {
		t.Fatalf("legacy rejected %+v", selected)
	}
	accepted, _ := agentObservedRun(t, client, selected, false)
	if !accepted {
		t.Fatal("legacy receipt behavior changed")
	}
}
func TestAgentPublicationCacheAttributesAndCopies(t *testing.T) {
	client, state := agentEnhancedClient(t, false)
	fixture := agentVectors(t)
	state.change(func(s *agentTestState) {
		s.assignmentForAttributes = func(body map[string]any) map[string]any {
			kind := "publishedOther"
			if body["attributes"].(map[string]any)["premium"] == true {
				kind = "published"
			}
			raw, _ := json.Marshal(fixture.Enhanced[kind])
			var response map[string]any
			_ = json.Unmarshal(raw, &response)
			response["expiresAt"] = s.config["expiresAt"]
			return response
		}
	})
	first := agentChoose(client)
	if first.Reason != "published" || first.Arm != "" || first.AssignmentID != "" {
		t.Fatalf("invalid publication %+v", first)
	}
	first.Serving.PublicationID = "00000000-0000-4000-8000-000000000000"
	cached := client.Choose(context.Background(), "support", AgentChoiceOptions{Unit: "session:42", Attributes: map[string]any{"premium": true, "irrelevant": "different"}, Fallback: ABParams{"model": "deployed"}})
	if cached.Serving.PublicationID == first.Serving.PublicationID {
		t.Fatal("caller changed cached publication")
	}
	other := client.Choose(context.Background(), "support", AgentChoiceOptions{Unit: "session:42", Attributes: map[string]any{"premium": false}, Fallback: ABParams{"model": "deployed"}})
	if other.Params["model"] != "other" {
		t.Fatalf("wrong publication %+v", other)
	}
	state.change(func(s *agentTestState) {
		count := 0
		for _, request := range s.requests {
			if strings.HasSuffix(request.Path, "/assignments") {
				count++
			}
		}
		if count != 2 {
			t.Errorf("assignment calls %d", count)
		}
	})
	accepted, attrs := agentObservedRun(t, client, other, false)
	if accepted || attrs["neatlogs.agent.support.serving_policy_id"] != other.Serving.PublicationID || attrs["neatlogs.agent.support.assignment_id"] != nil || client.Diagnostics().Pending != 0 {
		t.Fatalf("invalid publication attribution %+v", attrs)
	}
}
func TestAgentConcurrentPublicationAttributes(t *testing.T) {
	client, state := agentEnhancedClient(t, false)
	fixture := agentVectors(t)
	state.change(func(s *agentTestState) {
		s.assignmentForAttributes = func(body map[string]any) map[string]any {
			kind := "publishedOther"
			if body["attributes"].(map[string]any)["premium"] == true {
				kind = "published"
			}
			raw, _ := json.Marshal(fixture.Enhanced[kind])
			var response map[string]any
			_ = json.Unmarshal(raw, &response)
			response["expiresAt"] = s.config["expiresAt"]
			return response
		}
	})
	results := make(chan AgentDecision, 2)
	go func() { results <- agentChoose(client) }()
	go func() {
		results <- client.Choose(context.Background(), "support", AgentChoiceOptions{Unit: "session:42", Attributes: map[string]any{"premium": false}, Fallback: ABParams{"model": "deployed"}})
	}()
	models := map[any]bool{(<-results).Params["model"]: true, (<-results).Params["model"]: true}
	if !models["current"] || !models["other"] {
		t.Fatalf("concurrent attrs shared response %+v", models)
	}
}
func TestAgentEnhancedAcknowledgement(t *testing.T) {
	for _, stage := range []string{"config", "assignment", "304"} {
		t.Run(stage, func(t *testing.T) {
			client, state := agentEnhancedClient(t, true)
			if stage != "config" {
				if err := client.Refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			state.change(func(s *agentTestState) {
				s.acknowledge = false
				if stage == "304" {
					s.configStatus = 304
				}
			})
			if stage == "304" {
				_ = client.Refresh(context.Background())
			}
			decision := agentChoose(client)
			if decision.Reason != "unavailable" || decision.Cause != "invalid_identity" {
				t.Fatalf("missing ack accepted %+v", decision)
			}
		})
	}
}
func TestAgentPublicationValidation(t *testing.T) {
	for _, field := range []string{"pair", "unadvertised", "revision", "unit", "attributes", "arm", "params", "currentparams"} {
		t.Run(field, func(t *testing.T) {
			client, state := agentEnhancedClient(t, false)
			fixture := agentVectors(t)
			state.change(func(s *agentTestState) {
				serving := s.assignment["serving"].(map[string]any)
				switch field {
				case "pair":
					serving["publicationId"] = fixture.Enhanced["publishedOther"]["serving"].(map[string]any)["publicationId"]
				case "unadvertised":
					serving["publicationId"] = "00000000-0000-4000-8000-000000000000"
				case "revision":
					serving["revision"] = float64(2)
				case "unit":
					s.assignment["unit"] = "end_user"
				case "attributes":
					s.assignment["attributesDigest"] = strings.Repeat("0", 64)
				case "arm":
					s.assignment["arm"] = "a"
				case "params":
					s.assignment["params"].(map[string]any)["model"] = "tampered"
				case "currentparams":
					s.assignment = map[string]any{"protocolVersion": 2, "profile": agentProfile, "agentKey": "support", "revision": s.config["revision"], "expiresAt": s.config["expiresAt"], "reason": "current", "arm": "a", "fingerprint": s.config["current"].(map[string]any)["fingerprint"], "params": map[string]any{"model": "tampered"}}
				}
			})
			decision := agentChoose(client)
			if decision.Reason != "unavailable" || decision.Cause != "invalid_identity" {
				t.Fatalf("invalid publication accepted %+v", decision)
			}
		})
	}
}
func TestAgentMutatedParamsObservation(t *testing.T) {
	client, _ := agentEnhancedClient(t, true)
	decision := agentChoose(client)
	decision.Params["model"] = "mutated"
	expected, _ := agentVersionFingerprint("support", decision.Params)
	accepted, attrs := agentObservedRun(t, client, decision, false)
	if accepted || attrs["neatlogs.agent.support.decision_reason"] != "assigned" || attrs["neatlogs.agent.support.decision_cause"] != "invalid_identity" || attrs["neatlogs.agent.support.served_fingerprint"] != expected || attrs["neatlogs.agent.support.assignment_id"] != nil || client.Diagnostics().Pending != 0 {
		t.Fatalf("mutation attributed to old receipt %+v", attrs)
	}
}
func TestAgentFallbackObservationAndReplay(t *testing.T) {
	client, state := agentEnhancedClient(t, true)
	state.change(func(s *agentTestState) { s.assignmentStatus = 503 })
	decision := agentChoose(client)
	if decision.Reason != "unavailable" || decision.Cause != "transport_unavailable" {
		t.Fatalf("missing transport cause %+v", decision)
	}
	accepted, attrs := agentObservedRun(t, client, decision, false)
	if accepted || attrs["neatlogs.agent.support.unit_id"] != "session:42" || attrs["neatlogs.agent.support.unit_kind"] != "session" || attrs["neatlogs.agent.support.decision_cause"] != "transport_unavailable" || attrs["neatlogs.agent.support.assignment_id"] != nil {
		t.Fatalf("missing fallback identity %+v", attrs)
	}
	_, attrs = agentObservedRun(t, client, decision, true)
	for key := range attrs {
		if strings.HasPrefix(key, "neatlogs.agent.support.") {
			t.Fatalf("replay ordinary production metadata %+v", attrs)
		}
	}
}
func TestAgentUnsupportedCanonicalCauseAndLegacy(t *testing.T) {
	if unicode.IsGraphic('\U000105d2') {
		t.Skip("runtime supports this NFC vector; unsupported cases remain in canonical vectors")
	}
	client, state := agentEnhancedClient(t, true)
	state.change(func(s *agentTestState) {
		s.config["current"].(map[string]any)["params"] = map[string]any{"prompt": "\U000105d2\u0307"}
	})
	decision := agentChoose(client)
	if decision.Reason != "unavailable" || decision.Cause != "unsupported_canonical_params" {
		t.Fatalf("unsupported canonical cause %+v", decision)
	}
	fixture := agentVectors(t)
	state.change(func(s *agentTestState) {
		expiry := s.config["expiresAt"]
		s.config = fixture.Enhanced["legacyConfig"]
		s.config["expiresAt"] = expiry
		s.assignment = fixture.Enhanced["legacyAssignment"]
		s.assignment["params"] = map[string]any{"prompt": "\U000105d2\u0307"}
		s.assignment["expiresAt"] = expiry
	})
	decision = agentChoose(client)
	if decision.Reason != "assigned" || decision.Cause != "" {
		t.Fatalf("legacy unavailable %+v", decision)
	}
	accepted, attrs := agentObservedRun(t, client, decision, false)
	if !accepted || attrs["neatlogs.agent.support.decision_cause"] != "unsupported_canonical_params" || attrs["neatlogs.agent.support.served_fingerprint"] != nil {
		t.Fatalf("legacy unsupported hash falsely attributed %+v", attrs)
	}
}

func TestAgentRealProviderSignedAttributeBudget(t *testing.T) {
	for _, test := range []struct {
		count    int
		enhanced bool
	}{{125, false}, {128, false}, {125, true}} {
		name := strconv.Itoa(test.count)
		if test.enhanced {
			name += "-enhanced"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "128")
			var client *AgentClient
			if test.enhanced {
				client, _ = agentEnhancedClient(t, true)
			} else {
				client, _ = agentTestClient(t, 60, 60)
			}
			decision := agentChoose(client)
			sink := tracetest.NewInMemoryExporter()
			telemetry, err := NewClient(context.Background(), Config{WorkflowName: "budget"}, WithExporter(sink))
			if err != nil {
				t.Fatal(err)
			}
			defer telemetry.Shutdown(context.Background())
			ctx, root, end := Trace(telemetry.Context(context.Background()), "budget-root")
			readable := abRootSpan(ctx, root.SpanContext().TraceID().String())
			if readable == nil {
				t.Fatal("private root is not readable")
			}
			initial := len(readable.Attributes())
			for index := 0; index < test.count-initial; index++ {
				root.SetAttributes(attribute.Int("caller."+strconv.Itoa(index), index))
			}
			if len(readable.Attributes()) != test.count {
				t.Fatalf("unexpected initial budget %d", len(readable.Attributes()))
			}
			recorded := client.RecordRun(ctx, decision)
			expected := test.count == 125
			if recorded != expected {
				t.Fatalf("recorded %v expected %v", recorded, expected)
			}
			attributes := map[string]any{}
			for _, item := range readable.Attributes() {
				attributes[string(item.Key)] = item.Value.AsInterface()
			}
			if expected {
				if attributes["neatlogs.agent.support.assignment_id"] != decision.AssignmentID || attributes["neatlogs.agent.support.fingerprint"] != decision.Fingerprint {
					t.Fatalf("lost signed identity %+v", attributes)
				}
			} else if client.Diagnostics().Pending != 0 {
				t.Fatal("queued unreadable signed identity")
			}
			if test.enhanced && attributes["neatlogs.agent.support.served_fingerprint"] != nil {
				t.Fatal("expected ordinary metadata capacity to remain incomplete")
			}
			end()
		})
	}
}
