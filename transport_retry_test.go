package neatlogs

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func retryTestSpan(t *testing.T) sdktrace.ReadOnlySpan {
	t.Helper()
	sink := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(sink))
	_, span := provider.Tracer("retry-test").Start(context.Background(), "span")
	span.End()
	spans := sink.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("captured spans = %d, want 1", len(spans))
	}
	return spans[0].Snapshot()
}

func TestOTLPHTTPRetriesTransientStatusesAndUsesGzip(t *testing.T) {
	for _, transient := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(transient), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				attempt := attempts.Add(1)
				if request.Header.Get("Content-Encoding") != "gzip" {
					t.Errorf("Content-Encoding = %q, want gzip", request.Header.Get("Content-Encoding"))
				}
				reader, err := gzip.NewReader(request.Body)
				if err != nil {
					t.Errorf("gzip reader: %v", err)
				} else if decoded, readErr := io.ReadAll(reader); readErr != nil || len(decoded) == 0 {
					t.Errorf("decoded payload bytes = %d, err=%v", len(decoded), readErr)
				}
				if attempt == 1 {
					response.Header().Set("Retry-After", "0")
					response.WriteHeader(transient)
					return
				}
				response.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			base, _ := url.Parse(server.URL)
			exporter, err := newOTLPExporter(context.Background(), base, "test-key", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{retryTestSpan(t)}); err != nil {
				t.Fatal(err)
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("attempts = %d, want 2", got)
			}
			if err := exporter.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOTLPHTTPHonorsTimeoutAndRejectsAfterShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	exporter, err := newOTLPExporter(context.Background(), base, "test-key", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	span := retryTestSpan(t)
	if err := exporter.ExportSpans(ctx, []sdktrace.ReadOnlySpan{span}); err == nil {
		t.Fatal("timed-out export returned nil")
	}
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{span}); err == nil {
		t.Fatal("export after shutdown returned nil")
	}
}
