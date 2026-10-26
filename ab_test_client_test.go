package neatlogs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const abTestKey = "cheap_model"
const abTestSalt = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func abFixture() map[string]any {
	return map[string]any{"protocolVersion": 1, "id": "20000000-0000-4000-8000-000000000001", "key": abTestKey, "revision": 1, "allocationVersion": 1, "salt": abTestSalt, "unit": "session", "status": "running", "enabled": true, "defaultArm": "a", "expiresAt": time.Now().Add(time.Minute).Format(time.RFC3339Nano), "endsAt": time.Now().Add(time.Hour).Format(time.RFC3339Nano), "refreshAfterSeconds": 60, "assignment": abAssignmentAlgorithm, "etag": "settings-v1", "arms": []any{
		map[string]any{"key": "a", "label": "Original", "params": map[string]any{"model": "gpt-4o"}, "weight": 4105},
		map[string]any{"key": "b", "label": "Cheaper", "params": map[string]any{"model": "gpt-4o-mini"}, "weight": 5895},
	}}
}
func abTestClient(t *testing.T, handler http.HandlerFunc) *ABTestClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewABTestClient(ABTestConfig{Endpoint: server.URL, APIKey: "test-key", Keys: []string{abTestKey}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}
func abRespond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func TestABAssignmentProtocol(t *testing.T) {
	for unit, want := range map[string]uint32{"user-1": 2509, "用户-α": 2445, "session:42": 4105} {
		if got := abBucket(abTestSalt, unit); got != want {
			t.Fatalf("bucket(%q)=%d want %d", unit, got, want)
		}
	}
	var calls atomic.Int32
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing API credential")
		}
		abRespond(w, abFixture())
	})
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	choice := client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: "session:42"})
	if choice.Arm != "b" || choice.Params["model"] != "gpt-4o-mini" || choice.Reason != "assigned" || calls.Load() != 1 {
		t.Fatalf("choice=%+v calls=%d", choice, calls.Load())
	}
	choice.Params["model"] = "mutated"
	if client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: "session:42"}).Params["model"] != "gpt-4o-mini" {
		t.Fatal("mutated cached settings")
	}
	client.mu.Lock()
	config := client.configs[abTestKey]
	*config.Arms[0].Weight = 4106
	*config.Arms[1].Weight = 5894
	client.configs[abTestKey] = config
	client.mu.Unlock()
	if client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: "session:42"}).Arm != "a" {
		t.Fatal("assignment threshold differs from other SDKs")
	}
}
func TestABRefreshFailureFallsBackImmediately(t *testing.T) {
	var failure atomic.Bool
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if failure.Load() {
			w.WriteHeader(503)
			return
		}
		abRespond(w, abFixture())
	})
	_ = client.Refresh(context.Background())
	failure.Store(true)
	if client.Refresh(context.Background()) == nil {
		t.Fatal("expected refresh failure")
	}
	result := client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: "session:42", Fallback: ABParams{"model": "control"}})
	if result.Arm != "a" || result.Reason != "unavailable" || result.Revision != nil || result.Params["model"] != "control" {
		t.Fatalf("unsafe stale assignment %+v", result)
	}
}
func TestABShippedDefaultsAndExpiry(t *testing.T) {
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		v := abFixture()
		v["status"] = "shipped"
		v["enabled"] = false
		v["defaultArm"] = "b"
		arms := v["arms"].([]any)
		arms[0].(map[string]any)["weight"] = 0
		arms[1].(map[string]any)["weight"] = 10000
		abRespond(w, v)
	})
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: "u"}); got.Arm != "b" || got.Reason != "default" {
		t.Fatalf("shipped choice %+v", got)
	}
	if client.Diagnostics().Pending != 0 {
		t.Fatal("disabled assignment logged an exposure")
	}
	client.mu.Lock()
	config := client.configs[abTestKey]
	config.expires = time.Now().Add(-time.Second)
	client.configs[abTestKey] = config
	client.mu.Unlock()
	if got := client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: "u"}); got.Arm != "a" || got.Reason != "unavailable" {
		t.Fatalf("expired choice %+v", got)
	}
}
func TestABConditionalRefreshAndInvalidRevision(t *testing.T) {
	var calls atomic.Int32
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 2 {
			if r.Header.Get("If-None-Match") != "settings-v1" {
				t.Error("missing conditional cache header")
			}
			w.Header().Set("X-AB-Config-Expires-At", time.Now().Add(time.Minute).Format(time.RFC3339Nano))
			w.WriteHeader(304)
			return
		}
		value := abFixture()
		if n == 1 {
			value["revision"] = 2
		}
		abRespond(w, value)
	})
	if client.Refresh(context.Background()) != nil || client.Refresh(context.Background()) != nil {
		t.Fatal("refresh failed")
	}
	if client.Refresh(context.Background()) == nil {
		t.Fatal("accepted an older configuration")
	}
	if client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: "u"}).Reason != "unavailable" {
		t.Fatal("kept invalid settings")
	}
}
func TestABRejectsInvalidConfiguration(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){"identity": func(v map[string]any) { v["id"] = "bad" }, "algorithm": func(v map[string]any) { v["assignment"] = "custom" }, "expiry": func(v map[string]any) { v["expiresAt"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano) }, "allocation": func(v map[string]any) { v["arms"].([]any)[0].(map[string]any)["weight"] = 5 }, "status": func(v map[string]any) { v["status"] = "paused" }, "missing_enabled": func(v map[string]any) { delete(v, "enabled") }, "nested_params": func(v map[string]any) {
		v["arms"].([]any)[0].(map[string]any)["params"] = map[string]any{"nested": map[string]any{}}
	}} {
		t.Run(name, func(t *testing.T) {
			client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { v := abFixture(); edit(v); abRespond(w, v) })
			if client.Refresh(context.Background()) == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestABExposureOrderAndRetries(t *testing.T) {
	var mu sync.Mutex
	var events []abExposure
	ready := false
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var event abExposure
		_ = json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		events = append(events, event)
		accept := ready
		mu.Unlock()
		if !accept {
			w.WriteHeader(409)
			abRespond(w, map[string]any{"code": "AB_TRACE_NOT_READY"})
			return
		}
		abRespond(w, map[string]any{"accepted": true})
	})
	ctx := context.Background()
	opts := ABExposureOptions{UnitID: "session", Arm: "b", TraceID: "trace"}
	if !client.LogExposure(ctx, abTestKey, opts) || !client.LogExposure(ctx, abTestKey, opts) || !client.CompleteUnit(ctx, abTestKey, opts, "external") {
		t.Fatal("queue failed")
	}
	if client.Diagnostics().Pending != 2 {
		t.Fatal("duplicate exposure was queued")
	}
	if client.Flush(ctx) != ErrABExposurePending {
		t.Fatal("must retain not-ready receipt")
	}
	mu.Lock()
	ready = true
	mu.Unlock()
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 || events[0].EventID != events[1].EventID || events[0].UnitComplete || !events[2].UnitComplete || events[2].Source != "external" {
		t.Fatalf("incorrect retry or closure order %+v", events)
	}
}
func TestABExposureBoundaries(t *testing.T) {
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	ctx := context.Background()
	event := "20000000-0000-4000-8000-000000000003"
	if !client.LogExposure(ctx, abTestKey, ABExposureOptions{UnitID: "u", Arm: "a", TraceID: "t", EventID: event}) {
		t.Fatal("first receipt")
	}
	if client.LogExposure(ctx, abTestKey, ABExposureOptions{UnitID: "different", Arm: "b", TraceID: "t", EventID: event}) {
		t.Fatal("overwrote a pending event ID")
	}
	if client.LogExposure(ctx, "unregistered", ABExposureOptions{UnitID: "u", Arm: "a", TraceID: "t"}) || client.LogExposure(ctx, abTestKey, ABExposureOptions{UnitID: "u", Arm: "a"}) {
		t.Fatal("accepted missing trace or unknown key")
	}
	if client.Flush(ctx) != ErrABExposureRejected || client.Diagnostics().Pending != 0 {
		t.Fatal("permanent rejection was retried")
	}
	client.Close()
	if client.LogExposure(ctx, abTestKey, ABExposureOptions{UnitID: "v", Arm: "a", TraceID: "t"}) {
		t.Fatal("closed client queued exposure")
	}
}
func TestABQueueLimitAndRetention(t *testing.T) {
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("expired queue made HTTP request") })
	for i := 0; i < 1000; i++ {
		if !client.LogExposure(context.Background(), abTestKey, ABExposureOptions{UnitID: fmt.Sprint(i), Arm: "a", TraceID: "trace"}) {
			t.Fatalf("queue failed at %d", i)
		}
	}
	if client.LogExposure(context.Background(), abTestKey, ABExposureOptions{UnitID: "extra", Arm: "a", TraceID: "trace"}) {
		t.Fatal("unbounded queue")
	}
	client.mu.Lock()
	for i := range client.queue {
		client.queue[i].queuedAt = time.Now().Add(-25 * time.Hour)
	}
	client.mu.Unlock()
	if client.Flush(context.Background()) != ErrABExposureRejected || client.Diagnostics().Pending != 0 || client.Diagnostics().Rejected != 1001 {
		t.Fatal("expired receipts were not counted")
	}
}
func TestABHTTPBoundaries(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com?x=y"} {
		if _, err := NewABTestClient(ABTestConfig{Endpoint: endpoint, APIKey: "key"}); err == nil {
			t.Errorf("accepted endpoint %s", endpoint)
		}
	}
	t.Run("bounded_response", func(t *testing.T) {
		client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 65537))) })
		if client.Refresh(context.Background()) == nil {
			t.Fatal("accepted oversized response")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var redirected atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true) }))
		defer target.Close()
		client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) })
		_ = client.Refresh(context.Background())
		if redirected.Load() {
			t.Fatal("credentials followed redirect")
		}
	})
}
func TestABRejectsInvalidUTF8Unit(t *testing.T) {
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { abRespond(w, abFixture()) })
	_ = client.Refresh(context.Background())
	if client.Assign(context.Background(), abTestKey, ABAssignmentOptions{Unit: string([]byte{255})}).Reason != "unavailable" {
		t.Fatal("invalid UTF-8 unit was hashed differently from the server")
	}
	if client.LogExposure(context.Background(), abTestKey, ABExposureOptions{UnitID: string([]byte{255}), Arm: "a", TraceID: "t"}) {
		t.Fatal("invalid UTF-8 receipt")
	}
}
func TestABConcurrentAssignmentAndContext(t *testing.T) {
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { abRespond(w, abFixture()) })
	_ = client.Refresh(context.Background())
	ctx := WithABTestClient(context.Background(), client)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ABTest(ctx, abTestKey, ABAssignmentOptions{Unit: "session:42"}).Arm != "b" {
				t.Error("unstable assignment")
			}
		}()
	}
	wg.Wait()
	if ABTest(context.Background(), abTestKey, ABAssignmentOptions{Unit: "u", Fallback: ABParams{"model": "control"}}).Reason != "unavailable" {
		t.Fatal("context leaked client")
	}
}
