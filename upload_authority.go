package neatlogs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type uploadPurpose string

const (
	uploadPurposeTypedMedia   uploadPurpose = "typed_media"
	uploadPurposeOTLPOverflow uploadPurpose = "otlp_overflow"

	uploadEncodingIdentity = "identity"
	uploadEncodingGZIP     = "gzip"

	uploadSchemaTracesV1 = "otlp.traces.v1"
	uploadSchemaMediaV1  = "neatlogs.media.v1"

	defaultUploadTimeout   = 30 * time.Second
	defaultUploadStageTime = 10 * time.Second
	defaultUploadAttempts  = 3
	maxUploadResponseBytes = 64 * 1024
	maxOTLPOverflowBytes   = 20 * 1024 * 1024
	maxTypedMediaBytes     = 25 * 1024 * 1024
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
var headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
var diagnosticCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
var uploadMIMEPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

var typedMediaMIMETypes = map[string]struct{}{
	"application/pdf": {},
	"audio/flac":      {},
	"audio/mpeg":      {},
	"audio/ogg":       {},
	"audio/wav":       {},
	"image/gif":       {},
	"image/jpeg":      {},
	"image/png":       {},
	"image/webp":      {},
}

type uploadPayload struct {
	Content         []byte
	Purpose         uploadPurpose
	MIMEType        string
	ContentEncoding string
	PayloadSchema   string
	IdempotencyKey  string
}

type uploadReference struct {
	ID              string        `json:"id"`
	Purpose         uploadPurpose `json:"purpose"`
	SHA256          string        `json:"sha256"`
	ByteLength      int64         `json:"byte_length"`
	MIMEType        string        `json:"mime_type"`
	ContentEncoding string        `json:"content_encoding"`
	State           string        `json:"state"`
}

type uploadDiagnostic struct {
	Stage      string `json:"stage"`
	ReasonCode string `json:"reason_code"`
	Retryable  *bool  `json:"retryable"`
}

type uploadReceipt struct {
	UploadID   string
	State      string
	Reference  uploadReference
	Diagnostic *uploadDiagnostic
}

type uploadAuthority interface {
	Upload(context.Context, uploadPayload) (uploadReceipt, error)
}

type uploadFailure struct {
	stage      string
	reasonCode string
	retryable  bool
}

func (e *uploadFailure) Error() string {
	return fmt.Sprintf("neatlogs: upload %s failed (%s)", e.stage, e.reasonCode)
}

func newUploadFailure(stage, reasonCode string, retryable bool) error {
	return &uploadFailure{stage: stage, reasonCode: reasonCode, retryable: retryable}
}

type httpUploadAuthority struct {
	baseURL      *url.URL
	apiKey       string
	client       *http.Client
	timeout      time.Duration
	stageTimeout time.Duration
	attempts     int
	now          func() time.Time
}

type prepareUploadRequest struct {
	Version         int           `json:"version"`
	Purpose         uploadPurpose `json:"purpose"`
	SHA256          string        `json:"sha256"`
	ByteLength      int64         `json:"byte_length"`
	MIMEType        string        `json:"mime_type"`
	ContentEncoding string        `json:"content_encoding"`
	IdempotencyKey  string        `json:"idempotency_key"`
	PayloadSchema   string        `json:"payload_schema,omitempty"`
}

type preparedUpload struct {
	httpStatus int
	UploadID   string `json:"upload_id"`
	State      string `json:"state"`
	Expires    string `json:"expires_at"`
	Upload     struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"upload"`
	Reference  uploadReference   `json:"reference"`
	Diagnostic *uploadDiagnostic `json:"diagnostic,omitempty"`
}

type completeUploadRequest struct {
	SHA256     string `json:"sha256"`
	ByteLength int64  `json:"byte_length"`
}

type completedUpload struct {
	httpStatus int
	UploadID   string            `json:"upload_id"`
	State      string            `json:"state"`
	Reference  uploadReference   `json:"reference"`
	Diagnostic *uploadDiagnostic `json:"diagnostic,omitempty"`
}

type uploadErrorResponse struct {
	ReasonCode string `json:"reason_code"`
	Retryable  *bool  `json:"retryable"`
}

func newHTTPUploadAuthority(base *url.URL, apiKey string) *httpUploadAuthority {
	client := &http.Client{
		Timeout: defaultUploadStageTime,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &httpUploadAuthority{
		baseURL: base, apiKey: apiKey, client: client, timeout: defaultUploadTimeout,
		stageTimeout: defaultUploadStageTime, attempts: defaultUploadAttempts, now: time.Now,
	}
}

func (a *httpUploadAuthority) Upload(ctx context.Context, payload uploadPayload) (uploadReceipt, error) {
	if err := validateUploadPayload(payload); err != nil {
		return uploadReceipt{}, err
	}
	uploadCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	digestHex, err := uploadDigest(uploadCtx, payload.Content)
	if err != nil {
		return uploadReceipt{}, newUploadFailure("prepare", contextReason(uploadCtx), contextRetryable(uploadCtx))
	}
	request := prepareUploadRequest{
		Version: 1, Purpose: payload.Purpose, SHA256: digestHex,
		ByteLength: int64(len(payload.Content)), MIMEType: payload.MIMEType,
		ContentEncoding: payload.ContentEncoding, IdempotencyKey: payload.IdempotencyKey,
		PayloadSchema: payload.PayloadSchema,
	}
	prepared, err := a.prepare(uploadCtx, request)
	if err != nil {
		return uploadReceipt{}, err
	}
	if prepared.State == "ready" {
		return uploadReceipt{
			UploadID: prepared.UploadID, State: prepared.State,
			Reference: prepared.Reference, Diagnostic: prepared.Diagnostic,
		}, nil
	}
	if prepared.State == "prepared" {
		if err := a.put(uploadCtx, prepared, payload.Content); err != nil {
			return uploadReceipt{}, err
		}
	}
	return a.complete(uploadCtx, prepared.UploadID, request)
}

func uploadDigest(ctx context.Context, content []byte) (string, error) {
	hash := sha256.New()
	const chunkSize = 256 * 1024
	for offset := 0; offset < len(content); offset += chunkSize {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		end := offset + chunkSize
		if end > len(content) {
			end = len(content)
		}
		_, _ = hash.Write(content[offset:end])
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validateUploadPayload(payload uploadPayload) error {
	if payload.Purpose != uploadPurposeTypedMedia && payload.Purpose != uploadPurposeOTLPOverflow {
		return newUploadFailure("prepare", "invalid_purpose", false)
	}
	if len(payload.Content) == 0 {
		return newUploadFailure("prepare", "empty_payload", false)
	}
	limit := maxTypedMediaBytes
	if payload.Purpose == uploadPurposeOTLPOverflow {
		limit = maxOTLPOverflowBytes
	}
	if len(payload.Content) > limit {
		return newUploadFailure("prepare", "payload_too_large", false)
	}
	if len(payload.MIMEType) < 3 || len(payload.MIMEType) > 160 || !uploadMIMEPattern.MatchString(payload.MIMEType) {
		return newUploadFailure("prepare", "invalid_mime_type", false)
	}
	if payload.ContentEncoding != uploadEncodingIdentity && payload.ContentEncoding != uploadEncodingGZIP {
		return newUploadFailure("prepare", "invalid_content_encoding", false)
	}
	if payload.IdempotencyKey == "" || len(payload.IdempotencyKey) > 128 || strings.ContainsAny(payload.IdempotencyKey, "\r\n") {
		return newUploadFailure("prepare", "invalid_idempotency_key", false)
	}
	if payload.Purpose == uploadPurposeTypedMedia {
		if payload.ContentEncoding != uploadEncodingIdentity {
			return newUploadFailure("prepare", "invalid_content_encoding", false)
		}
		if _, supported := typedMediaMIMETypes[payload.MIMEType]; !supported {
			return newUploadFailure("prepare", "unsupported_mime_type", false)
		}
		if payload.PayloadSchema != "" && payload.PayloadSchema != uploadSchemaMediaV1 {
			return newUploadFailure("prepare", "invalid_payload_schema", false)
		}
		return nil
	}
	if payload.PayloadSchema != uploadSchemaTracesV1 {
		return newUploadFailure("prepare", "invalid_payload_schema", false)
	}
	if payload.MIMEType != "application/x-protobuf" {
		return newUploadFailure("prepare", "unsupported_mime_type", false)
	}
	return nil
}

func (a *httpUploadAuthority) prepare(ctx context.Context, request prepareUploadRequest) (preparedUpload, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return preparedUpload{}, newUploadFailure("prepare", "encode_failed", false)
	}
	endpoint := a.apiURL("/v1/telemetry/uploads")
	var prepared preparedUpload
	prepared.httpStatus, err = a.doJSON(
		ctx, "prepare", http.MethodPost, endpoint, body,
		[]int{http.StatusOK, http.StatusCreated, http.StatusAccepted}, &prepared,
	)
	if err != nil {
		return preparedUpload{}, err
	}
	if err := a.validatePrepared(prepared, request); err != nil {
		return preparedUpload{}, err
	}
	return prepared, nil
}

func (a *httpUploadAuthority) validatePrepared(prepared preparedUpload, request prepareUploadRequest) error {
	if !uuidPattern.MatchString(prepared.UploadID) ||
		prepared.Reference.ID != prepared.UploadID ||
		!referenceMatches(prepared.Reference, request, prepared.State) ||
		!validUploadDiagnostic(prepared.Diagnostic) {
		return newUploadFailure("prepare", "invalid_response", false)
	}
	switch prepared.State {
	case "ready":
		if prepared.httpStatus != http.StatusOK {
			return newUploadFailure("prepare", "invalid_response", false)
		}
		return nil
	case "uploaded", "validating":
		if prepared.httpStatus != http.StatusOK && prepared.httpStatus != http.StatusAccepted {
			return newUploadFailure("prepare", "invalid_response", false)
		}
		if prepared.Diagnostic == nil {
			return newUploadFailure("prepare", "invalid_response", false)
		}
		return nil
	case "rejected":
		if prepared.httpStatus != http.StatusOK {
			return newUploadFailure("prepare", "invalid_response", false)
		}
		reason := "rejected"
		if prepared.Diagnostic != nil {
			reason = prepared.Diagnostic.ReasonCode
		}
		return newUploadFailure("prepare", reason, false)
	case "prepared":
		if prepared.httpStatus != http.StatusCreated {
			return newUploadFailure("prepare", "invalid_response", false)
		}
	default:
		return newUploadFailure("prepare", "invalid_state", false)
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	expires, err := time.Parse(time.RFC3339, prepared.Expires)
	if err != nil || !expires.After(now()) {
		return newUploadFailure("prepare", "invalid_expiry", false)
	}
	if prepared.Upload.Method != http.MethodPut || prepared.Upload.Headers == nil || len(prepared.Upload.Headers) > 32 {
		return newUploadFailure("prepare", "invalid_upload_authority", false)
	}
	uploadURL, err := url.Parse(prepared.Upload.URL)
	if err != nil || uploadURL.IsAbs() == false || uploadURL.Host == "" || uploadURL.User != nil || !secureUploadScheme(uploadURL) {
		return newUploadFailure("prepare", "invalid_upload_authority", false)
	}
	for name, value := range prepared.Upload.Headers {
		if textproto.TrimString(name) != name || !headerNamePattern.MatchString(name) ||
			forbiddenUploadHeader(name) || strings.ContainsAny(value, "\r\n") || len(name)+len(value) > 8192 {
			return newUploadFailure("prepare", "invalid_upload_headers", false)
		}
	}
	return nil
}

func secureUploadScheme(uploadURL *url.URL) bool {
	return uploadURL.Scheme == "https"
}

func forbiddenUploadHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "cookie", "proxy-authorization", "x-api-key":
		return true
	default:
		return false
	}
}

func referenceMatches(reference uploadReference, request prepareUploadRequest, state string) bool {
	return uuidPattern.MatchString(reference.ID) && reference.Purpose == request.Purpose &&
		reference.SHA256 == request.SHA256 && reference.ByteLength == request.ByteLength &&
		reference.MIMEType == request.MIMEType && reference.ContentEncoding == request.ContentEncoding &&
		reference.State == state
}

func (a *httpUploadAuthority) put(ctx context.Context, prepared preparedUpload, content []byte) error {
	_, err := a.do(ctx, "upload", func(stageCtx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(stageCtx, http.MethodPut, prepared.Upload.URL, bytes.NewReader(content))
		if err != nil {
			return nil, err
		}
		for name, value := range prepared.Upload.Headers {
			request.Header.Set(name, value)
		}
		request.ContentLength = int64(len(content))
		return request, nil
	}, []int{http.StatusOK, http.StatusCreated, http.StatusNoContent}, nil)
	return err
}

func (a *httpUploadAuthority) complete(ctx context.Context, uploadID string, request prepareUploadRequest) (uploadReceipt, error) {
	attempts := a.attempts
	if attempts <= 0 {
		attempts = 1
	}
	var receipt uploadReceipt
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		var validationPending bool
		receipt, validationPending, err = a.completeAttempt(ctx, uploadID, request)
		if !validationPending {
			return receipt, err
		}
		if attempt+1 < attempts && waitForUploadRetry(ctx, retryDelay(attempt, "")) {
			continue
		}
		if ctx.Err() != nil {
			return receipt, newUploadFailure("complete", contextReason(ctx), contextRetryable(ctx))
		}
		return receipt, err
	}
	return receipt, err
}

func (a *httpUploadAuthority) completeAttempt(ctx context.Context, uploadID string, request prepareUploadRequest) (uploadReceipt, bool, error) {
	body, err := json.Marshal(completeUploadRequest{SHA256: request.SHA256, ByteLength: request.ByteLength})
	if err != nil {
		return uploadReceipt{}, false, newUploadFailure("complete", "encode_failed", false)
	}
	endpoint := a.apiURL("/v1/telemetry/uploads/" + url.PathEscape(uploadID) + "/complete")
	var completed completedUpload
	completed.httpStatus, err = a.doJSON(ctx, "complete", http.MethodPost, endpoint, body, []int{http.StatusOK, http.StatusAccepted}, &completed)
	if err != nil {
		return uploadReceipt{}, false, err
	}
	if completed.UploadID != uploadID || completed.Reference.ID != uploadID ||
		!referenceMatches(completed.Reference, request, completed.State) {
		return uploadReceipt{}, false, newUploadFailure("complete", "invalid_reference", false)
	}
	if completed.State != "ready" && completed.State != "uploaded" &&
		completed.State != "validating" && completed.State != "rejected" {
		return uploadReceipt{}, false, newUploadFailure("complete", "invalid_state", false)
	}
	if (completed.State == "validating" && completed.httpStatus != http.StatusAccepted) ||
		(completed.State != "validating" && completed.httpStatus != http.StatusOK) {
		return uploadReceipt{}, false, newUploadFailure("complete", "invalid_response", false)
	}
	if !validUploadDiagnostic(completed.Diagnostic) {
		return uploadReceipt{}, false, newUploadFailure("complete", "invalid_diagnostic", false)
	}
	receipt := uploadReceipt{
		UploadID: completed.UploadID, State: completed.State,
		Reference: completed.Reference, Diagnostic: completed.Diagnostic,
	}
	if completed.State != "ready" {
		pending := completed.State == "uploaded" || completed.State == "validating"
		reason, retryable := "not_ready", pending
		if completed.Diagnostic != nil {
			if completed.Diagnostic.ReasonCode != "" {
				reason = completed.Diagnostic.ReasonCode
			}
			if !pending {
				retryable = *completed.Diagnostic.Retryable
			}
		}
		return receipt, pending, newUploadFailure("complete", reason, retryable)
	}
	return receipt, false, nil
}

func validUploadDiagnostic(diagnostic *uploadDiagnostic) bool {
	return diagnostic == nil || (diagnosticCodePattern.MatchString(diagnostic.Stage) &&
		diagnosticCodePattern.MatchString(diagnostic.ReasonCode) && diagnostic.Retryable != nil)
}

func uploadReceiptReady(receipt uploadReceipt) bool {
	return uuidPattern.MatchString(receipt.UploadID) && receipt.State == "ready" &&
		receipt.Reference.ID == receipt.UploadID && receipt.Reference.State == "ready"
}

func (a *httpUploadAuthority) apiURL(path string) string {
	endpoint := *a.baseURL
	endpoint.Path = path
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	endpoint.User = nil
	return endpoint.String()
}

func (a *httpUploadAuthority) doJSON(ctx context.Context, stage, method, endpoint string, body []byte, statuses []int, target any) (int, error) {
	return a.do(ctx, stage, func(stageCtx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(stageCtx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("content-type", "application/json")
		request.Header.Set("x-api-key", a.apiKey)
		return request, nil
	}, statuses, target)
}

func (a *httpUploadAuthority) do(
	ctx context.Context,
	stage string,
	makeRequest func(context.Context) (*http.Request, error),
	statuses []int,
	target any,
) (int, error) {
	attempts := a.attempts
	if attempts <= 0 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		stageTimeout := a.stageTimeout
		if stageTimeout <= 0 {
			stageTimeout = defaultUploadStageTime
		}
		stageCtx, cancel := context.WithTimeout(ctx, stageTimeout)
		request, err := makeRequest(stageCtx)
		if err != nil {
			cancel()
			return 0, newUploadFailure(stage, "request_failed", false)
		}
		response, requestErr := a.client.Do(request)
		if requestErr != nil {
			cancel()
			if attempt+1 < attempts && ctx.Err() == nil {
				if !waitForUploadRetry(ctx, retryDelay(attempt, "")) {
					return 0, newUploadFailure(stage, contextReason(ctx), contextRetryable(ctx))
				}
				continue
			}
			if ctx.Err() != nil {
				return 0, newUploadFailure(stage, contextReason(ctx), contextRetryable(ctx))
			}
			return 0, newUploadFailure(stage, "transport_error", true)
		}
		accepted := containsStatus(statuses, response.StatusCode)
		if accepted {
			if target == nil {
				_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxUploadResponseBytes+1))
				response.Body.Close()
				cancel()
				if readErr != nil {
					return 0, newUploadFailure(stage, "response_read_failed", true)
				}
				return response.StatusCode, nil
			}
			decodeErr := decodeUploadJSON(response.Body, target)
			response.Body.Close()
			cancel()
			if decodeErr != nil {
				return 0, newUploadFailure(stage, "invalid_response", false)
			}
			return response.StatusCode, nil
		}
		retryable := retryableUploadStatus(response.StatusCode)
		retryAfter := response.Header.Get("retry-after")
		responseBytes, _ := io.ReadAll(io.LimitReader(response.Body, maxUploadResponseBytes+1))
		response.Body.Close()
		cancel()
		reasonCode := fmt.Sprintf("http_status_%d", response.StatusCode)
		if len(responseBytes) <= maxUploadResponseBytes {
			var problem uploadErrorResponse
			if json.Unmarshal(responseBytes, &problem) == nil && diagnosticCodePattern.MatchString(problem.ReasonCode) && problem.Retryable != nil {
				reasonCode = problem.ReasonCode
				retryable = *problem.Retryable
			}
		}
		if retryable && attempt+1 < attempts {
			if !waitForUploadRetry(ctx, retryDelay(attempt, retryAfter)) {
				return 0, newUploadFailure(stage, contextReason(ctx), contextRetryable(ctx))
			}
			continue
		}
		return 0, newUploadFailure(stage, reasonCode, retryable)
	}
	return 0, newUploadFailure(stage, "retry_exhausted", true)
}

func decodeUploadJSON(reader io.Reader, target any) error {
	limited := io.LimitReader(reader, maxUploadResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || len(body) == 0 || len(body) > maxUploadResponseBytes {
		return errors.New("invalid upload response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid trailing upload response")
	}
	return nil
}

func containsStatus(statuses []int, status int) bool {
	for _, accepted := range statuses {
		if status == accepted {
			return true
		}
	}
	return false
}

func retryableUploadStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		delay := time.Duration(seconds) * time.Second
		if delay > 2*time.Second {
			return 2 * time.Second
		}
		return delay
	}
	return time.Duration(1<<attempt) * 100 * time.Millisecond
}

func waitForUploadRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func contextReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	return "cancelled"
}

func contextRetryable(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

func uploadIdempotencyKey(purpose uploadPurpose, mimeType, encoding, schema, digest string) string {
	seed := strings.Join([]string{string(purpose), mimeType, encoding, schema, digest}, "\x00")
	hash := sha256.Sum256([]byte(seed))
	prefix := "nlgo-upload-"
	if purpose == uploadPurposeTypedMedia {
		prefix = "nlgo-media-"
	} else if purpose == uploadPurposeOTLPOverflow {
		prefix = "nlgo-otlp-"
	}
	return prefix + hex.EncodeToString(hash[:])
}
