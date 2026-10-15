package neatlogs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const datasetRequestLimit = 64 * 1024

var datasetUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// DatasetConfig configures the public API independently of telemetry initialization.
type DatasetConfig struct {
	Endpoint   string
	Token      string
	ProjectID  string
	HTTPClient *http.Client
}

// DatasetClient creates immutable dataset versions through the public API.
type DatasetClient struct {
	endpoint  string
	token     string
	projectID string
	http      *http.Client
}

func NewDatasetClient(cfg DatasetConfig) (*DatasetClient, error) {
	base, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("neatlogs: dataset endpoint must be an HTTP(S) base URL")
	}
	if base.Scheme == "http" && !strings.EqualFold(base.Hostname(), "localhost") {
		ip := net.ParseIP(base.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("neatlogs: dataset endpoint requires HTTPS except on loopback")
		}
	}
	path := strings.TrimRight(base.Path, "/")
	if path != "" && path != "/api/v1" {
		return nil, errors.New("neatlogs: dataset endpoint must be a base URL or end in /api/v1")
	}
	if strings.TrimSpace(cfg.Token) == "" || strings.ContainsAny(cfg.Token, "\r\n") {
		return nil, errors.New("neatlogs: dataset token is required")
	}
	if !datasetUUID.MatchString(cfg.ProjectID) {
		return nil, errors.New("neatlogs: dataset project ID must be a UUID")
	}
	base.Path = "/api/v1/public"
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Public API credentials must never follow a redirect to another authority.
	isolated := *client
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &DatasetClient{endpoint: strings.TrimRight(base.String(), "/"), token: cfg.Token, projectID: strings.ToLower(cfg.ProjectID), http: &isolated}, nil
}

// DatasetPublication identifies a dataset and its immutable published version.
type DatasetPublication struct {
	DatasetID string `json:"datasetId"`
	VersionID string `json:"versionId"`
}

// DatasetOutcome records a human supplied draft outcome for one case.
type DatasetOutcome struct {
	Value  any    `json:"value"`
	Status string `json:"status"`
	Source string `json:"source"`
}

func NewDatasetOutcome(value any) *DatasetOutcome {
	return &DatasetOutcome{Value: value, Status: "draft", Source: "human"}
}

// DatasetCase is an imported example. A nil Outcome leaves the case unlabeled.
type DatasetCase struct {
	Input          any             `json:"input"`
	ObservedOutput any             `json:"observedOutput"`
	Outcome        *DatasetOutcome `json:"expected"`
}

type CreateDatasetOptions struct {
	Name        string        `json:"name"`
	Description *string       `json:"description,omitempty"`
	Cases       []DatasetCase `json:"cases"`
	// Reuse this UUID when retrying the same logical operation after an uncertain result.
	IdempotencyKey string `json:"idempotencyKey"`
}

// Create works without Init or NewClient, including when Cases is empty.
func (c *DatasetClient) Create(ctx context.Context, options CreateDatasetOptions) (DatasetPublication, error) {
	var result DatasetPublication
	options.Name = strings.TrimSpace(options.Name)
	if n := utf8.RuneCountInString(options.Name); n < 1 || n > 255 {
		return result, errors.New("neatlogs: dataset name must contain 1 to 255 characters")
	}
	if options.Description != nil && utf8.RuneCountInString(*options.Description) > 2000 {
		return result, errors.New("neatlogs: dataset description exceeds 2000 characters")
	}
	if len(options.Cases) > 500 {
		return result, errors.New("neatlogs: dataset creation accepts at most 500 cases")
	}
	if options.Cases == nil {
		options.Cases = []DatasetCase{}
	}
	for _, item := range options.Cases {
		if err := validateDatasetOutcome(item.Outcome); err != nil {
			return result, err
		}
	}
	key, err := datasetIdempotencyKey(options.IdempotencyKey)
	if err != nil {
		return result, err
	}
	options.IdempotencyKey = key
	err = c.request(ctx, http.MethodPost, "/datasets", key, options, &result)
	result.DatasetID = strings.ToLower(result.DatasetID)
	result.VersionID = strings.ToLower(result.VersionID)
	if err == nil && (!datasetUUID.MatchString(result.DatasetID) || !datasetUUID.MatchString(result.VersionID)) {
		err = errors.New("neatlogs: invalid dataset publication response")
	}
	return result, err
}

// DatasetOutcomeChange pins an edit to a case revision. A nil Outcome clears it.
// Cases absent from Changes preserve their existing outcome.
type DatasetOutcomeChange struct {
	CaseID     string          `json:"caseId"`
	RevisionID string          `json:"revisionId"`
	Outcome    *DatasetOutcome `json:"expected"`
}

type UpdateDatasetOutcomesOptions struct {
	BaseVersionID  string                 `json:"baseVersionId"`
	Changes        []DatasetOutcomeChange `json:"expectedChanges"`
	IdempotencyKey string                 `json:"idempotencyKey"`
}

func (c *DatasetClient) UpdateOutcomes(ctx context.Context, datasetID string, options UpdateDatasetOutcomesOptions) (DatasetPublication, error) {
	var result DatasetPublication
	datasetID = strings.ToLower(datasetID)
	options.BaseVersionID = strings.ToLower(options.BaseVersionID)
	options.Changes = append([]DatasetOutcomeChange(nil), options.Changes...)
	if !datasetUUID.MatchString(datasetID) || !datasetUUID.MatchString(options.BaseVersionID) {
		return result, errors.New("neatlogs: dataset and base version IDs must be UUIDs")
	}
	if len(options.Changes) == 0 || len(options.Changes) > 2000 {
		return result, errors.New("neatlogs: provide 1 to 2000 outcome changes")
	}
	seen := make(map[string]bool, len(options.Changes))
	for i := range options.Changes {
		options.Changes[i].CaseID = strings.ToLower(options.Changes[i].CaseID)
		options.Changes[i].RevisionID = strings.ToLower(options.Changes[i].RevisionID)
		change := options.Changes[i]
		if !datasetUUID.MatchString(change.CaseID) || !datasetUUID.MatchString(change.RevisionID) || seen[change.CaseID] {
			return result, errors.New("neatlogs: outcome changes require unique case UUIDs and revision UUIDs")
		}
		seen[change.CaseID] = true
		if err := validateDatasetOutcome(change.Outcome); err != nil {
			return result, err
		}
	}
	key, err := datasetIdempotencyKey(options.IdempotencyKey)
	if err != nil {
		return result, err
	}
	options.IdempotencyKey = key
	err = c.request(ctx, http.MethodPost, "/datasets/"+datasetID+"/versions", key, options, &result)
	result.DatasetID = strings.ToLower(result.DatasetID)
	result.VersionID = strings.ToLower(result.VersionID)
	if err == nil && (result.DatasetID != datasetID || !datasetUUID.MatchString(result.VersionID)) {
		err = errors.New("neatlogs: invalid dataset publication response")
	}
	return result, err
}

func validateDatasetOutcome(outcome *DatasetOutcome) error {
	if outcome != nil && (outcome.Status != "draft" || outcome.Source != "human") {
		return errors.New("neatlogs: SDK outcomes must be human supplied drafts")
	}
	return nil
}

// DatasetAPIError contains safe public API failure metadata, never response bodies or credentials.
type DatasetAPIError struct {
	StatusCode     int
	RequestID      string
	IdempotencyKey string
}

func (e *DatasetAPIError) Error() string {
	return fmt.Sprintf("neatlogs: dataset API returned HTTP %d (request %s)", e.StatusCode, e.RequestID)
}

func datasetIdempotencyKey(key string) (string, error) {
	if key != "" {
		if !datasetUUID.MatchString(key) {
			return "", errors.New("neatlogs: idempotency key must be a UUID")
		}
		return strings.ToLower(key), nil
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("neatlogs: generate idempotency key: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func (c *DatasetClient) request(ctx context.Context, method, path, key string, body any, result any) error {
	if c == nil {
		return errors.New("neatlogs: nil dataset client")
	}
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("neatlogs: encode dataset request: %w", err)
		}
		if len(encoded) > datasetRequestLimit {
			return errors.New("neatlogs: dataset request exceeds 64 KiB")
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(encoded))
		if err != nil {
			return errors.New("neatlogs: invalid dataset request")
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("x-project-id", c.projectID)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Transport errors can contain URLs or credentials from a custom transport.
			return &DatasetAPIError{IdempotencyKey: key}
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
		resp.Body.Close()
		if readErr != nil || len(data) > 1024*1024 {
			return errors.New("neatlogs: invalid dataset API response")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := &DatasetAPIError{StatusCode: resp.StatusCode, RequestID: resp.Header.Get("x-request-id"), IdempotencyKey: key}
			if attempt == 2 || (resp.StatusCode != 429 && resp.StatusCode != 502 && resp.StatusCode != 503 && resp.StatusCode != 504) {
				return apiErr
			}
			timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		var envelope struct {
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &envelope) != nil || !envelope.Success || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return errors.New("neatlogs: invalid dataset API response")
		}
		if err := json.Unmarshal(envelope.Data, result); err != nil {
			return errors.New("neatlogs: invalid dataset API response")
		}
		if path == "/dataset-capture-jobs" && resp.Header.Get("Idempotency-Replayed") == "true" {
			if receipt, ok := result.(*DatasetCaptureReceipt); ok {
				receipt.replayed = true
			}
		}
		return nil
	}
	return errors.New("neatlogs: dataset API request failed")
}
