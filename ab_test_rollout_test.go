package neatlogs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func abRolloutFixture(percentage, epoch int) map[string]any {
	v := abFixture()
	v["phase"] = "rollout"
	v["rolloutPercentage"] = percentage
	v["allocationVersion"] = epoch
	v["revision"] = epoch
	v["etag"] = fmt.Sprintf("stage-%d", epoch)
	v["status"] = "ramping"
	if percentage == 100 {
		v["status"] = "shipped"
	}
	arms := v["arms"].([]any)
	arms[0].(map[string]any)["weight"] = 10000 - percentage*100
	arms[1].(map[string]any)["weight"] = percentage * 100
	return v
}
func TestABRolloutMonotoneAndLocal(t *testing.T) {
	var stage atomic.Int32
	stage.Store(50)
	var calls atomic.Int32
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		epoch := 2
		if stage.Load() == 100 {
			epoch = 3
		}
		abRespond(w, abRolloutFixture(int(stage.Load()), epoch))
	})
	ctx := context.Background()
	if err := client.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	original := map[string]string{}
	for i := 0; i < 1000; i++ {
		unit := fmt.Sprint(i)
		choice := client.Assign(ctx, abTestKey, ABAssignmentOptions{Unit: unit})
		original[unit] = choice.Arm
		if choice.AllocationVersion == nil || *choice.AllocationVersion != 2 {
			t.Fatal("missing allocation epoch")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("assignment performed network I/O")
	}
	stage.Store(100)
	if err := client.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for unit, previous := range original {
		choice := client.Assign(ctx, abTestKey, ABAssignmentOptions{Unit: unit})
		if choice.Arm != "b" || *choice.AllocationVersion != 3 {
			t.Fatalf("non-monotone %s from %s to %+v", unit, previous, choice)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("assignment performed network I/O")
	}
}
func TestABRolloutDelayedClosureKeepsOriginalEpoch(t *testing.T) {
	var stage atomic.Int32
	stage.Store(50)
	var received []abExposure
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			epoch := 2
			if stage.Load() == 100 {
				epoch = 3
			}
			abRespond(w, abRolloutFixture(int(stage.Load()), epoch))
			return
		}
		var item abExposure
		if err := json.NewDecoder(r.Body).Decode(&item); err != nil {
			t.Error(err)
		}
		received = append(received, item)
		abRespond(w, map[string]any{"accepted": true})
	})
	ctx := context.Background()
	if err := client.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	options := ABExposureOptions{UnitID: "u", TraceID: "original-trace", Arm: "b"}
	if !client.LogExposure(ctx, abTestKey, options) {
		t.Fatal("exposure failed")
	}
	stage.Store(100)
	if err := client.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !client.CompleteUnit(ctx, abTestKey, options, "external") {
		t.Fatal("original closure failed")
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(received) != 2 || received[0].AllocationVersion != 2 || received[1].AllocationVersion != 2 || !received[1].UnitComplete {
		t.Fatalf("wrong epochs: %+v", received)
	}
	if client.CompleteUnit(ctx, abTestKey, ABExposureOptions{UnitID: "missing", TraceID: "missing", Arm: "a"}, "external") {
		t.Fatal("guessed unknown closure epoch")
	}
	if !client.CompleteUnit(ctx, abTestKey, ABExposureOptions{UnitID: "explicit", TraceID: "explicit", Arm: "a", AllocationVersion: 2}, "external") {
		t.Fatal("explicit original epoch rejected")
	}
}
func TestABRolloutPauseAndExpiryServeControl(t *testing.T) {
	var paused atomic.Bool
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		v := abRolloutFixture(100, 3)
		if paused.Load() {
			v["revision"] = 4
			v["status"] = "auto_paused"
			v["enabled"] = false
			arms := v["arms"].([]any)
			arms[0].(map[string]any)["weight"] = 10000
			arms[1].(map[string]any)["weight"] = 0
		}
		abRespond(w, v)
	})
	ctx := context.Background()
	if err := client.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if client.Assign(ctx, abTestKey, ABAssignmentOptions{Unit: "u"}).Arm != "b" {
		t.Fatal("100% must choose treatment")
	}
	paused.Store(true)
	if err := client.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if client.Assign(ctx, abTestKey, ABAssignmentOptions{Unit: "u"}).Arm != "a" {
		t.Fatal("pause must choose control")
	}
	client.mu.Lock()
	cfg := client.configs[abTestKey]
	cfg.expires = time.Now().Add(-time.Second)
	client.configs[abTestKey] = cfg
	client.mu.Unlock()
	if choice := client.Assign(ctx, abTestKey, ABAssignmentOptions{Unit: "u"}); choice.Arm != "a" || choice.Reason != "unavailable" {
		t.Fatal("expired cache served rollout")
	}
}
func TestABRolloutRejectsInconsistentSettings(t *testing.T) {
	for name, change := range map[string]func(map[string]any){
		"missing_phase": func(v map[string]any) { delete(v, "phase") }, "epoch1": func(v map[string]any) { v["allocationVersion"] = 1 }, "overflow": func(v map[string]any) { v["allocationVersion"] = 2147483648 },
		"wrong_status": func(v map[string]any) { v["status"] = "shipped" }, "wrong_weights": func(v map[string]any) { v["rolloutPercentage"] = 100 }, "default_treatment": func(v map[string]any) { v["defaultArm"] = "b" }, "ended": func(v map[string]any) { v["endsAt"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano) },
	} {
		t.Run(name, func(t *testing.T) {
			client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { v := abRolloutFixture(50, 2); change(v); abRespond(w, v) })
			if client.Refresh(context.Background()) == nil {
				t.Fatal("invalid rollout accepted")
			}
		})
	}
}

func TestABRolloutAmbiguousClosureNeedsExplicitEpoch(t *testing.T) {
	client := abTestClient(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("queue must not perform HTTP") })
	ctx := context.Background()
	options := ABExposureOptions{UnitID: "u", TraceID: "trace", Arm: "b", AllocationVersion: 2}
	if !client.LogExposure(ctx, abTestKey, options) {
		t.Fatal("original receipt failed")
	}
	options.AllocationVersion = 3
	if client.LogExposure(ctx, abTestKey, options) {
		t.Fatal("same trace silently changed epoch")
	}
	options.AllocationVersion = 0
	if client.CompleteUnit(ctx, abTestKey, options, "external") {
		t.Fatal("ambiguous completion guessed epoch")
	}
	options.AllocationVersion = 2
	if !client.CompleteUnit(ctx, abTestKey, options, "external") {
		t.Fatal("explicit original epoch rejected")
	}
}
