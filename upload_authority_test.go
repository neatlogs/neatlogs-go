package neatlogs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPUploadAuthorityUsesAuthenticatedPrepareScopedPutAndComplete(t *testing.T) {
	content := []byte("complete masked payload")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	uploadID := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	referenceID := uploadID

	var mu sync.Mutex
	var prepared prepareUploadRequest
	var completed completeUploadRequest
	var uploaded []byte
	var putAPIKey string
	var putHeader string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/telemetry/uploads":
			if request.Method != http.MethodPost || request.Header.Get("x-api-key") != "project-key" {
				t.Errorf("prepare request = %s auth=%q", request.Method, request.Header.Get("x-api-key"))
			}
			if err := json.NewDecoder(request.Body).Decode(&prepared); err != nil {
				t.Errorf("decode prepare: %v", err)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"upload_id": uploadID, "state": "prepared",
				"expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
				"upload": map[string]any{
					"method": "PUT", "url": server.URL + "/object?X-Amz-Signature=secret-signed-value",
					"headers": map[string]string{"x-object-token": "scoped-secret"},
				},
				"reference": map[string]any{
					"id": referenceID, "purpose": string(uploadPurposeTypedMedia), "sha256": digestHex,
					"byte_length": len(content), "mime_type": "image/png", "content_encoding": "identity", "state": "prepared",
				},
				"unknown_future_field": true,
			})
		case "/object":
			putAPIKey = request.Header.Get("x-api-key")
			putHeader = request.Header.Get("x-object-token")
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read PUT: %v", err)
			}
			mu.Lock()
			uploaded = append([]byte(nil), body...)
			mu.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case "/v1/telemetry/uploads/" + uploadID + "/complete":
			if request.Method != http.MethodPost || request.Header.Get("x-api-key") != "project-key" {
				t.Errorf("complete request = %s auth=%q", request.Method, request.Header.Get("x-api-key"))
			}
			if err := json.NewDecoder(request.Body).Decode(&completed); err != nil {
				t.Errorf("decode complete: %v", err)
			}
			_ = json.NewEncoder(response).Encode(map[string]any{
				"upload_id": uploadID, "state": "ready",
				"reference": map[string]any{
					"id": referenceID, "purpose": string(uploadPurposeTypedMedia), "sha256": digestHex,
					"byte_length": len(content), "mime_type": "image/png", "content_encoding": "identity", "state": "ready",
				},
				"another_future_field": map[string]any{"safe": true},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "project-key")
	receipt, err := authority.Upload(context.Background(), uploadPayload{
		Content: content, Purpose: uploadPurposeTypedMedia, MIMEType: "image/png",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaMediaV1,
		IdempotencyKey: "media-idempotency-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !uploadReceiptReady(receipt) {
		t.Fatalf("receipt = %#v", receipt)
	}
	if prepared.Version != 1 || prepared.Purpose != uploadPurposeTypedMedia || prepared.SHA256 != digestHex ||
		prepared.ByteLength != int64(len(content)) || prepared.MIMEType != "image/png" ||
		prepared.ContentEncoding != uploadEncodingIdentity || prepared.IdempotencyKey != "media-idempotency-key" ||
		prepared.PayloadSchema != uploadSchemaMediaV1 {
		t.Fatalf("prepare body = %#v", prepared)
	}
	if completed.SHA256 != digestHex || completed.ByteLength != int64(len(content)) {
		t.Fatalf("complete body = %#v", completed)
	}
	mu.Lock()
	gotUpload := string(uploaded)
	mu.Unlock()
	if gotUpload != string(content) || putHeader != "scoped-secret" {
		t.Fatalf("PUT content/header = %q/%q", gotUpload, putHeader)
	}
	if putAPIKey != "" {
		t.Fatalf("project API key leaked to object PUT: %q", putAPIKey)
	}
	if rendered := fmt.Sprintf("%#v", receipt); strings.Contains(rendered, "secret-signed-value") || strings.Contains(rendered, "scoped-secret") {
		t.Fatalf("receipt retained signed upload authority: %s", rendered)
	}
}

func TestHTTPUploadAuthorityRetriesTransientPrepareWithStableIdempotency(t *testing.T) {
	content := []byte("retry payload")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	uploadID := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	referenceID := uploadID
	var prepares atomic.Int32
	var keysMu sync.Mutex
	var keys []string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/telemetry/uploads":
			var body prepareUploadRequest
			_ = json.NewDecoder(request.Body).Decode(&body)
			keysMu.Lock()
			keys = append(keys, body.IdempotencyKey)
			keysMu.Unlock()
			if prepares.Add(1) == 1 {
				response.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			response.WriteHeader(http.StatusCreated)
			writeUploadResponse(t, response, server.URL+"/object", uploadID, referenceID, "prepared", digestHex, len(content))
		case "/object":
			response.WriteHeader(http.StatusOK)
		case "/v1/telemetry/uploads/" + uploadID + "/complete":
			writeUploadResponse(t, response, "", uploadID, referenceID, "ready", digestHex, len(content))
		}
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "key")
	_, err := authority.Upload(context.Background(), uploadPayload{
		Content: content, Purpose: uploadPurposeOTLPOverflow, MIMEType: "application/x-protobuf",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaTracesV1,
		IdempotencyKey: "stable-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	keysMu.Lock()
	defer keysMu.Unlock()
	if len(keys) != 2 || keys[0] != "stable-key" || keys[1] != "stable-key" {
		t.Fatalf("prepare retry keys = %v", keys)
	}
}

func TestHTTPUploadAuthorityFailureDoesNotExposeResponseOrSignedURL(t *testing.T) {
	secret := "do-not-log-this-secret"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte(`{"error":"` + secret + `"}`))
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "also-secret")
	_, err := authority.Upload(context.Background(), uploadPayload{
		Content: []byte("payload"), Purpose: uploadPurposeTypedMedia, MIMEType: "image/png",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaMediaV1,
		IdempotencyKey: "key",
	})
	if err == nil {
		t.Fatal("upload unexpectedly succeeded")
	}
	if message := err.Error(); strings.Contains(message, secret) || strings.Contains(message, "also-secret") || strings.Contains(message, server.URL) {
		t.Fatalf("failure exposed secret material: %q", message)
	}
}

func TestHTTPUploadAuthorityShortCircuitsAlreadyReadyIdempotentPrepare(t *testing.T) {
	content := []byte("already uploaded")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	uploadID := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	referenceID := uploadID
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/telemetry/uploads" {
			t.Errorf("unexpected request after ready prepare: %s", request.URL.Path)
		}
		writeUploadResponse(t, response, "", uploadID, referenceID, "ready", digestHex, len(content))
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "key")
	receipt, err := authority.Upload(context.Background(), uploadPayload{
		Content: content, Purpose: uploadPurposeOTLPOverflow, MIMEType: "application/x-protobuf",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaTracesV1,
		IdempotencyKey: "stable-key",
	})
	if err != nil || !uploadReceiptReady(receipt) {
		t.Fatalf("ready replay = %#v, %v", receipt, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want prepare only", got)
	}
}

func TestHTTPUploadAuthorityTreatsValidatingPrepareAsRetryableNonSuccess(t *testing.T) {
	content := []byte("validation pending")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	uploadID := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	referenceID := uploadID
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/telemetry/uploads" {
			t.Errorf("validating replay made unexpected request: %s", request.URL.Path)
		}
		response.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(response).Encode(map[string]any{
			"upload_id": uploadID, "state": "validating",
			"reference": map[string]any{
				"id": referenceID, "purpose": "otlp_overflow", "sha256": digestHex,
				"byte_length": len(content), "mime_type": "application/x-protobuf", "content_encoding": "identity", "state": "validating",
			},
			"diagnostic": map[string]any{"stage": "validation", "reason_code": "scan_pending", "retryable": false},
		})
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "key")
	_, err := authority.Upload(context.Background(), uploadPayload{
		Content: content, Purpose: uploadPurposeOTLPOverflow, MIMEType: "application/x-protobuf",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaTracesV1,
		IdempotencyKey: "stable-key",
	})
	var failure *uploadFailure
	if !errors.As(err, &failure) || !failure.retryable || failure.reasonCode != "scan_pending" {
		t.Fatalf("validating prepare error = %#v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want prepare only", got)
	}
}

func TestHTTPUploadAuthorityRetriesValidatingCompletionWithoutAnotherPut(t *testing.T) {
	content := []byte("completion pending")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	uploadID := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	var puts atomic.Int32
	var completes atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/telemetry/uploads":
			response.WriteHeader(http.StatusCreated)
			writeUploadResponse(t, response, server.URL+"/object", uploadID, uploadID, "prepared", digestHex, len(content))
		case "/object":
			puts.Add(1)
			response.WriteHeader(http.StatusNoContent)
		case "/v1/telemetry/uploads/" + uploadID + "/complete":
			state := "ready"
			if completes.Add(1) == 1 {
				state = "validating"
				response.WriteHeader(http.StatusAccepted)
			}
			value := map[string]any{
				"upload_id": uploadID, "state": state,
				"reference": map[string]any{
					"id": uploadID, "purpose": "otlp_overflow", "sha256": digestHex,
					"byte_length": len(content), "mime_type": "application/x-protobuf", "content_encoding": "identity", "state": state,
				},
			}
			if state == "validating" {
				value["diagnostic"] = map[string]any{"stage": "validation", "reason_code": "UPLOAD_VALIDATION_IN_PROGRESS", "retryable": true}
			}
			_ = json.NewEncoder(response).Encode(value)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "key")
	authority.attempts = 2
	receipt, err := authority.Upload(context.Background(), uploadPayload{
		Content: content, Purpose: uploadPurposeOTLPOverflow, MIMEType: "application/x-protobuf",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaTracesV1,
		IdempotencyKey: "stable-key",
	})
	if err != nil || !uploadReceiptReady(receipt) {
		t.Fatalf("completion retry = %#v, %v", receipt, err)
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("object PUTs = %d, want 1", got)
	}
	if got := completes.Load(); got != 2 {
		t.Fatalf("completion requests = %d, want 2", got)
	}
}

func TestHTTPUploadAuthorityRejectsAmbientCredentialPutHeaders(t *testing.T) {
	content := []byte("credential header")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	uploadID := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(map[string]any{
			"upload_id": uploadID, "state": "prepared",
			"expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			"upload": map[string]any{
				"method": "PUT", "url": server.URL + "/object", "headers": map[string]string{"authorization": "scoped-secret"},
			},
			"reference": map[string]any{
				"id": uploadID, "purpose": "typed_media", "sha256": digestHex,
				"byte_length": len(content), "mime_type": "image/png", "content_encoding": "identity", "state": "prepared",
			},
		})
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "key")
	_, err := authority.Upload(context.Background(), uploadPayload{
		Content: content, Purpose: uploadPurposeTypedMedia, MIMEType: "image/png",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaMediaV1,
		IdempotencyKey: "stable-key",
	})
	var failure *uploadFailure
	if !errors.As(err, &failure) || failure.reasonCode != "invalid_upload_headers" {
		t.Fatalf("credential header error = %#v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want prepare only", got)
	}
}

func TestHTTPUploadAuthorityHonorsCallerDeadline(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		time.Sleep(150 * time.Millisecond)
		response.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	authority := testHTTPUploadAuthority(t, server, "key")
	authority.attempts = 1
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := authority.Upload(ctx, uploadPayload{
		Content: []byte("payload"), Purpose: uploadPurposeTypedMedia, MIMEType: "image/png",
		ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaMediaV1,
		IdempotencyKey: "key",
	})
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("deadline result = %v after %v", err, time.Since(started))
	}
}

func TestUploadsRequireExplicitValidActivation(t *testing.T) {
	t.Setenv("NEATLOGS_UPLOADS_ENABLED", "")
	enabled, err := resolveUploadsEnabled(Config{})
	if err != nil || enabled {
		t.Fatalf("default activation = %v, %v; want false", enabled, err)
	}
	t.Setenv("NEATLOGS_UPLOADS_ENABLED", "true")
	enabled, err = resolveUploadsEnabled(Config{})
	if err != nil || !enabled {
		t.Fatalf("environment activation = %v, %v; want true", enabled, err)
	}
	t.Setenv("NEATLOGS_UPLOADS_ENABLED", "not-a-boolean")
	if _, err := resolveUploadsEnabled(Config{}); err == nil {
		t.Fatal("invalid upload activation unexpectedly accepted")
	}
}

func TestEnabledUploadsRequireAPIKeyEvenWithCustomExporter(t *testing.T) {
	t.Setenv("NEATLOGS_API_KEY", "")
	t.Setenv("NEATLOGS_UPLOADS_ENABLED", "")
	client, err := NewClient(context.Background(), Config{EnableUploads: true}, WithExporter(&batchRecordingExporter{}))
	if err == nil || client != nil {
		t.Fatalf("client, error = %#v, %v; want authenticated upload configuration failure", client, err)
	}
}

func writeUploadResponse(t *testing.T, response http.ResponseWriter, uploadURL, uploadID, referenceID, state, digest string, length int) {
	t.Helper()
	value := map[string]any{
		"upload_id": uploadID, "state": state,
		"reference": map[string]any{
			"id": referenceID, "purpose": string(uploadPurposeOTLPOverflow), "sha256": digest,
			"byte_length": length, "mime_type": "application/x-protobuf", "content_encoding": "identity", "state": state,
		},
	}
	if state == "prepared" {
		value["expires_at"] = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
		value["upload"] = map[string]any{"method": "PUT", "url": uploadURL, "headers": map[string]string{}}
	}
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func testHTTPUploadAuthority(t *testing.T, server *httptest.Server, apiKey string) *httpUploadAuthority {
	t.Helper()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	authority := newHTTPUploadAuthority(base, apiKey)
	authority.client = server.Client()
	authority.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return authority
}
