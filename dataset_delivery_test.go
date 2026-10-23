package neatlogs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestDatasetCaptureRejectsOTLPPartialSuccess(t *testing.T) {
	body, err := proto.Marshal(&collectortrace.ExportTraceServiceResponse{PartialSuccess: &collectortrace.ExportTracePartialSuccess{RejectedSpans: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var exports atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("unexpected OTLP request %s", r.URL.Path)
		}
		exports.Add(1)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(body)
	}))
	defer collector.Close()
	telemetry, err := NewClient(context.Background(), Config{APIKey: "ingest-test", Endpoint: collector.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer telemetry.Shutdown(context.Background())
	aborted, sealed := false, false
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/abort") {
			aborted = true
			datasetCaptureReply(w, "failed")
			return
		}
		if strings.HasSuffix(r.URL.Path, "/complete") {
			sealed = true
			datasetCaptureReply(w, "queued")
			return
		}
		datasetCaptureReply(w, "recording")
	})
	_, err = client.Capture(context.Background(), telemetry, CaptureDatasetOptions{Environment: "ci", Destination: DatasetDestination{DatasetID: datasetTestID}}, func(ctx context.Context) error { _, _, end := Trace(ctx, "root"); end(); return nil })
	if exports.Load() != 1 || err == nil || !aborted || sealed {
		t.Fatalf("exports=%d err=%v aborted=%v sealed=%v", exports.Load(), err, aborted, sealed)
	}
}
