package neatlogs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const datasetCaptureTestID = "33333333-3333-4333-8333-333333333333"

func datasetCaptureReply(w http.ResponseWriter, state string) {
	w.Write([]byte(`{"success":true,"data":{"captureId":"` + datasetCaptureTestID + `","datasetId":"` + datasetTestID + `","state":"` + state + `"}}`))
}

func TestDatasetCaptureExportsExactContextAndPreservesCallbackError(t *testing.T) {
	sink := tracetest.NewInMemoryExporter()
	telemetry, err := NewClient(context.Background(), Config{}, WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	var manifest []DatasetTraceManifest
	requests := []string{}
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/complete") {
			if len(sink.GetSpans()) < 5 {
				t.Error("sealed before export acknowledgment")
			}
			var body struct {
				Traces []DatasetTraceManifest `json:"traces"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			manifest = body.Traces
			datasetCaptureReply(w, "queued")
			return
		}
		datasetCaptureReply(w, "recording")
	})
	callbackErr := errors.New("agent failed")
	_, _, outsideEnd := Trace(telemetry.Context(context.Background()), "outside")
	outsideEnd()
	receipt, err := client.Capture(context.Background(), telemetry, CaptureDatasetOptions{Destination: DatasetDestination{DatasetID: datasetTestID}}, func(ctx context.Context) error {
		for range 2 {
			rootCtx, _, end := Trace(ctx, "root")
			_, _, childEnd := StartSpan(rootCtx, "tool", "TOOL")
			childEnd()
			end()
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) || receipt.State != "queued" || len(manifest) != 2 || len(manifest[0].SpanIDs) != 2 || len(manifest[1].SpanIDs) != 2 || len(requests) != 2 {
		t.Fatalf("%+v %v manifest=%+v requests=%v", receipt, err, manifest, requests)
	}
}

func TestDatasetCaptureAbortsOpenSpansAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "cancelled"}[cancelled], func(t *testing.T) {
			sink := tracetest.NewInMemoryExporter()
			telemetry, _ := NewClient(context.Background(), Config{}, WithExporter(sink))
			defer telemetry.Shutdown(context.Background())
			aborted := false
			client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/complete") {
					t.Error("completed incomplete capture")
				}
				if strings.HasSuffix(r.URL.Path, "/abort") {
					aborted = true
					datasetCaptureReply(w, "cancelled")
					return
				}
				datasetCaptureReply(w, "recording")
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := client.Capture(ctx, telemetry, CaptureDatasetOptions{Destination: DatasetDestination{Name: "test", CreateIfMissing: true}}, func(ctx context.Context) error {
				_, _, _ = Trace(ctx, "open")
				if cancelled {
					cancel()
				}
				return nil
			})
			if err == nil || !aborted {
				t.Fatalf("err=%v aborted=%v", err, aborted)
			}
			if cancelled && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestDatasetCaptureRequiresEnabledFullSampling(t *testing.T) {
	rate := 0.5
	for _, cfg := range []Config{{DisableExport: true}, {SampleRate: &rate}} {
		telemetry, _ := NewClient(context.Background(), cfg, WithExporter(tracetest.NewInMemoryExporter()))
		defer telemetry.Shutdown(context.Background())
		client := datasetTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid client registered capture") })
		_, err := client.Capture(context.Background(), telemetry, CaptureDatasetOptions{Destination: DatasetDestination{DatasetID: datasetTestID}}, func(context.Context) error { t.Error("callback executed"); return nil })
		if err == nil {
			t.Fatal("accepted incomplete pipeline")
		}
	}
}

func TestDatasetCaptureCompletionUncertaintyDoesNotAbortOrRerun(t *testing.T) {
	telemetry, _ := NewClient(context.Background(), Config{}, WithExporter(tracetest.NewInMemoryExporter()))
	defer telemetry.Shutdown(context.Background())
	calls := 0
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/abort") {
			t.Error("aborted possibly sealed job")
		}
		if strings.HasSuffix(r.URL.Path, "/complete") {
			w.WriteHeader(503)
			return
		}
		datasetCaptureReply(w, "recording")
	})
	receipt, err := client.Capture(context.Background(), telemetry, CaptureDatasetOptions{Destination: DatasetDestination{DatasetID: datasetTestID}}, func(ctx context.Context) error { calls++; _, _, end := Trace(ctx, "root"); end(); return nil })
	var captureErr *DatasetCaptureError
	if !errors.As(err, &captureErr) || !captureErr.CompletionUncertain || calls != 1 || receipt.CaptureID != datasetCaptureTestID {
		t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, calls)
	}
}

func TestDatasetCaptureWaitPublicationAndFailure(t *testing.T) {
	for _, state := range []string{"completed", "failed", "needs_review", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"success":true,"data":{"job":{"id":"` + datasetCaptureTestID + `","datasetId":"` + datasetTestID + `","datasetVersionId":"` + datasetTestVersion + `","state":"` + state + `"},"failures":[],"nextCursor":null}}`))
			})
			status, err := client.Wait(context.Background(), datasetCaptureTestID)
			if status.State != state || (err == nil) != (state == "completed") {
				t.Fatalf("%+v %v", status, err)
			}
		})
	}
}
