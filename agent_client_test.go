package neatlogs

import (
	"context"
	"encoding/json"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type agentFixture struct {
	Config           map[string]any `json:"config"`
	Assignment       map[string]any `json:"assignment"`
	AttributeVectors []struct {
		Attributes map[string]any `json:"attributes"`
		Valid      bool           `json:"valid"`
	} `json:"attributeVectors"`
	UnitVectors []struct {
		Unit  string `json:"unit"`
		Valid bool   `json:"valid"`
	} `json:"unitVectors"`
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
	Path string
	Body map[string]any
}
type agentTestState struct {
	mu                                             sync.Mutex
	config, assignment                             map[string]any
	configStatus, assignmentStatus, exposureStatus int
	changed                                        bool
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
		state.requests = append(state.requests, agentRequest{r.URL.Path, body})
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
				_ = json.NewEncoder(w).Encode(state.assignment)
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
func TestAgentPauseAndUnsupportedUnit(t *testing.T) {
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
		t.Fatal("unsupported unit served")
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
		if len(sent) != 3 || sent[0]["eventId"] != sent[1]["eventId"] || sent[2]["assignmentId"] != decision.AssignmentID || sent[2]["unitComplete"] != true {
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
