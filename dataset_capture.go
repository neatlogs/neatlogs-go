package neatlogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
)

// DatasetDestination selects exactly one existing ID or dataset name.
// Names create a dataset only when CreateIfMissing is explicitly true.
type DatasetDestination struct {
	DatasetID       string
	Name            string
	CreateIfMissing bool
	Description     string
}

type CaptureDatasetOptions struct {
	Destination DatasetDestination
	// Reuse only for the same registration. An agent callback is never retried.
	IdempotencyKey string
}

type DatasetCaptureReceipt struct {
	CaptureID string `json:"captureId"`
	DatasetID string `json:"datasetId"`
	State     string `json:"state"`
}

// DatasetCaptureError retains the durable receipt for Status or Wait recovery.
// CompletionUncertain means completion may have reached the server; do not rerun execution.
type DatasetCaptureError struct {
	Receipt             DatasetCaptureReceipt
	Phase               string
	CompletionUncertain bool
	Err                 error
}

func (e *DatasetCaptureError) Error() string {
	return fmt.Sprintf("neatlogs: dataset capture %s failed: %v", e.Phase, e.Err)
}
func (e *DatasetCaptureError) Unwrap() error { return e.Err }

func datasetDestinationBody(destination DatasetDestination) (any, error) {
	if destination.DatasetID != "" {
		if !datasetUUID.MatchString(destination.DatasetID) || destination.Name != "" || destination.CreateIfMissing || destination.Description != "" {
			return nil, errors.New("neatlogs: dataset destination must select only an existing UUID or a name")
		}
		return struct {
			Kind      string `json:"kind"`
			DatasetID string `json:"datasetId"`
		}{"existing", strings.ToLower(destination.DatasetID)}, nil
	}
	name := strings.TrimSpace(destination.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 255 {
		return nil, errors.New("neatlogs: dataset destination name must contain 1 to 255 characters")
	}
	if utf8.RuneCountInString(destination.Description) > 2000 {
		return nil, errors.New("neatlogs: dataset destination description exceeds 2000 characters")
	}
	return struct {
		Kind            string `json:"kind"`
		Name            string `json:"name"`
		CreateIfMissing bool   `json:"createIfMissing"`
		Description     string `json:"description,omitempty"`
	}{"name", name, destination.CreateIfMissing, destination.Description}, nil
}

func validDatasetCaptureState(state string) bool {
	switch state {
	case "recording", "queued", "running", "needs_review", "completed", "failed", "cancelled":
		return true
	}
	return false
}

func validateDatasetReceipt(receipt *DatasetCaptureReceipt, captureID string) error {
	receipt.CaptureID = strings.ToLower(receipt.CaptureID)
	receipt.DatasetID = strings.ToLower(receipt.DatasetID)
	if !datasetUUID.MatchString(receipt.CaptureID) || !datasetUUID.MatchString(receipt.DatasetID) || !validDatasetCaptureState(receipt.State) || (captureID != "" && receipt.CaptureID != captureID) {
		return errors.New("neatlogs: invalid dataset capture receipt")
	}
	return nil
}

// Capture runs execute once on the supplied private telemetry Client, flushes
// normal OTLP delivery, and seals the exact trace/span IDs for publication.
// Pass its context into all helpers and join child goroutines before returning.
// A callback error is preserved while complete failed executions can still publish.
func (c *DatasetClient) Capture(ctx context.Context, telemetry *Client, options CaptureDatasetOptions, execute func(context.Context) error) (receipt DatasetCaptureReceipt, err error) {
	if c == nil || telemetry == nil || telemetry.runtime == nil || execute == nil {
		return receipt, errors.New("neatlogs: dataset capture requires dataset and telemetry clients and an execution callback")
	}
	if existing, _ := ctx.Value(datasetScopeKey{}).(*datasetCaptureScope); existing != nil {
		return receipt, errors.New("neatlogs: nested dataset captures are not supported")
	}
	destination, err := datasetDestinationBody(options.Destination)
	if err != nil {
		return receipt, err
	}
	key, err := datasetIdempotencyKey(options.IdempotencyKey)
	if err != nil {
		return receipt, err
	}
	runtime := telemetry.runtime
	runtime.mu.Lock()
	ready := runtime.state == stateRunning && runtime.datasetCaptureEnabled
	runtime.mu.Unlock()
	if !ready {
		return receipt, errors.New("neatlogs: dataset capture requires a running telemetry client with full sampling and export enabled")
	}
	scope, err := runtime.datasetCaptures.begin()
	if err != nil {
		return receipt, err
	}
	defer scope.release()
	if err = c.request(ctx, http.MethodPost, "/dataset-capture-jobs", key, struct {
		Destination any `json:"destination"`
	}{destination}, &receipt); err != nil {
		return receipt, &DatasetCaptureError{Phase: "registration", Receipt: receipt, Err: err}
	}
	if err = validateDatasetReceipt(&receipt, ""); err != nil {
		return receipt, &DatasetCaptureError{Phase: "registration", Receipt: receipt, Err: err}
	}
	if receipt.State != "recording" {
		// Replayed registration may already be sealed. It must never execute again.
		return receipt, &DatasetCaptureError{Phase: "registration", Receipt: receipt, Err: errors.New("neatlogs: registration is already sealed; inspect its status instead of rerunning execution")}
	}
	abort := func(reason string, cause error) (DatasetCaptureReceipt, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		aborted, abortErr := c.Abort(cleanupCtx, receipt.CaptureID, reason, "")
		if abortErr == nil {
			receipt = aborted
		}
		return receipt, &DatasetCaptureError{Phase: reason, Receipt: receipt, Err: errors.Join(cause, abortErr)}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			scope.closeExecution()
			_, _ = abort("incomplete_execution", errors.New("neatlogs: execution panicked"))
			panic(recovered)
		}
	}()
	if err := ctx.Err(); err != nil {
		return abort("cancelled", err)
	}
	// Capture always begins new roots. It cannot claim a caller's earlier trace.
	executionCtx := withPrivateTraceContext(WithClient(ctx, telemetry), trace.SpanContext{}, runtime)
	executionCtx = trace.ContextWithSpanContext(executionCtx, trace.SpanContext{})
	executionCtx = context.WithValue(executionCtx, datasetScopeKey{}, scope)
	executionErr := execute(executionCtx)
	scope.closeExecution()
	if err := ctx.Err(); err != nil {
		return abort("cancelled", errors.Join(executionErr, err))
	}
	if err := telemetry.Flush(ctx); err != nil {
		return abort("export_failed", errors.Join(executionErr, err))
	}
	manifest, err := scope.manifest()
	if err != nil {
		return abort("incomplete_execution", errors.Join(executionErr, err))
	}
	body := struct {
		Traces []DatasetTraceManifest `json:"traces"`
	}{manifest}
	encoded, _ := json.Marshal(body)
	if len(encoded) > datasetRequestLimit {
		return abort("incomplete_execution", errors.Join(executionErr, errors.New("neatlogs: dataset capture manifest exceeds 64 KiB")))
	}
	if err := ctx.Err(); err != nil {
		return abort("cancelled", errors.Join(executionErr, err))
	}
	completeKey, err := datasetIdempotencyKey("")
	if err != nil {
		return abort("incomplete_execution", errors.Join(executionErr, err))
	}
	var completed DatasetCaptureReceipt
	if err := c.request(ctx, http.MethodPost, "/dataset-capture-jobs/"+receipt.CaptureID+"/complete", completeKey, body, &completed); err != nil {
		return receipt, &DatasetCaptureError{Phase: "completion", Receipt: receipt, CompletionUncertain: true, Err: errors.Join(executionErr, err)}
	}
	if err := validateDatasetReceipt(&completed, receipt.CaptureID); err != nil || completed.DatasetID != receipt.DatasetID {
		if err == nil {
			err = errors.New("neatlogs: dataset capture response changed destination")
		}
		return receipt, &DatasetCaptureError{Phase: "completion", Receipt: receipt, CompletionUncertain: true, Err: errors.Join(executionErr, err)}
	}
	return completed, executionErr
}

// Abort cancels an unfinished recording. It never cancels an already sealed job.
// reason is cancelled, export_failed, or incomplete_execution.
func (c *DatasetClient) Abort(ctx context.Context, captureID, reason, idempotencyKey string) (DatasetCaptureReceipt, error) {
	var receipt DatasetCaptureReceipt
	if !datasetUUID.MatchString(captureID) {
		return receipt, errors.New("neatlogs: capture ID must be a UUID")
	}
	switch reason {
	case "cancelled", "export_failed", "incomplete_execution":
	default:
		return receipt, errors.New("neatlogs: invalid dataset capture abort reason")
	}
	captureID = strings.ToLower(captureID)
	key, err := datasetIdempotencyKey(idempotencyKey)
	if err != nil {
		return receipt, err
	}
	err = c.request(ctx, http.MethodPost, "/dataset-capture-jobs/"+captureID+"/abort", key, struct {
		Reason string `json:"reason"`
	}{reason}, &receipt)
	if err == nil {
		err = validateDatasetReceipt(&receipt, captureID)
	}
	return receipt, err
}

type DatasetCaptureFailure struct {
	Position int             `json:"position"`
	Source   json.RawMessage `json:"source"`
	Error    struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

type DatasetCaptureStatus struct {
	CaptureID        string                  `json:"id"`
	DatasetID        string                  `json:"datasetId"`
	State            string                  `json:"state"`
	DatasetVersionID *string                 `json:"datasetVersionId"`
	TotalCount       int                     `json:"totalCount"`
	CapturedCount    int                     `json:"capturedCount"`
	FailedCount      int                     `json:"failedCount"`
	PendingCount     int                     `json:"pendingCount"`
	PublicationError *string                 `json:"publicationError"`
	Failures         []DatasetCaptureFailure `json:"-"`
	NextCursor       *string                 `json:"-"`
}

// Status returns durable progress and the first page of failures.
func (c *DatasetClient) Status(ctx context.Context, captureID string) (DatasetCaptureStatus, error) {
	var response struct {
		Job        DatasetCaptureStatus    `json:"job"`
		Failures   []DatasetCaptureFailure `json:"failures"`
		NextCursor *string                 `json:"nextCursor"`
	}
	if !datasetUUID.MatchString(captureID) {
		return response.Job, errors.New("neatlogs: capture ID must be a UUID")
	}
	captureID = strings.ToLower(captureID)
	if err := c.request(ctx, http.MethodGet, "/dataset-capture-jobs/"+captureID, "", nil, &response); err != nil {
		return response.Job, err
	}
	response.Job.CaptureID = strings.ToLower(response.Job.CaptureID)
	response.Job.DatasetID = strings.ToLower(response.Job.DatasetID)
	if response.Job.CaptureID != captureID || !datasetUUID.MatchString(response.Job.DatasetID) || !validDatasetCaptureState(response.Job.State) {
		return response.Job, errors.New("neatlogs: invalid dataset capture status")
	}
	if response.Job.DatasetVersionID != nil {
		version := strings.ToLower(*response.Job.DatasetVersionID)
		if !datasetUUID.MatchString(version) {
			return response.Job, errors.New("neatlogs: invalid dataset capture version")
		}
		response.Job.DatasetVersionID = &version
	}
	if response.Job.State == "completed" && response.Job.DatasetVersionID == nil {
		return response.Job, errors.New("neatlogs: completed dataset capture has no published version")
	}
	response.Job.Failures = response.Failures
	response.Job.NextCursor = response.NextCursor
	return response.Job, nil
}

type DatasetCaptureFailedError struct{ Status DatasetCaptureStatus }

func (e *DatasetCaptureFailedError) Error() string {
	return "neatlogs: dataset capture ended in state " + e.Status.State
}

// Wait polls until publication or a terminal failure, honoring ctx cancellation.
func (c *DatasetClient) Wait(ctx context.Context, captureID string) (DatasetCaptureStatus, error) {
	for {
		status, err := c.Status(ctx, captureID)
		if err != nil {
			return status, err
		}
		switch status.State {
		case "completed":
			return status, nil
		case "failed", "cancelled", "needs_review":
			return status, &DatasetCaptureFailedError{Status: status}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, ctx.Err()
		case <-timer.C:
		}
	}
}
