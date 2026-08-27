package neatlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type DoctorV2Stage struct {
	Stage            string   `json:"stage"`
	Status           string   `json:"status"`
	ReasonCode       string   `json:"reason_code"`
	At               string   `json:"at"`
	SpanCount        *int     `json:"span_count,omitempty"`
	MissingIDs       []string `json:"missing_ids,omitempty"`
	MissingFields    []string `json:"missing_fields,omitempty"`
	ParentMismatches *int     `json:"parent_mismatches,omitempty"`
	SemanticDigest   string   `json:"semantic_digest,omitempty"`
}
type DoctorV2Probe struct {
	DiagnosticID  string          `json:"diagnostic_id"`
	ReceiptStatus string          `json:"receipt_status"`
	ExpiresAt     string          `json:"expires_at"`
	Stages        []DoctorV2Stage `json:"stages"`
}
type DoctorProbeOptions struct {
	Endpoint     string
	APIKey       string
	Timeout      time.Duration
	PollInterval time.Duration
	HTTPClient   *http.Client
}
type diagnosticSession struct {
	FormatVersion  string `json:"format_version"`
	DiagnosticID   string `json:"diagnostic_id"`
	ProbeToken     string `json:"probe_token"`
	CreatedAt      string `json:"created_at"`
	ExpiresAt      string `json:"expires_at"`
	FixtureVersion string `json:"fixture_version"`
}
type diagnosticReceipt struct {
	FormatVersion string          `json:"format_version"`
	DiagnosticID  string          `json:"diagnostic_id"`
	Status        string          `json:"status"`
	FirstFailure  *string         `json:"first_failure"`
	Stages        []DoctorV2Stage `json:"stages"`
	ExpiresAt     string          `json:"expires_at"`
	LocalDigest   string          `json:"local_semantic_digest"`
	BackendDigest string          `json:"backend_semantic_digest"`
}

// DoctorProbeV2 correlates an already-captured synthetic envelope with the
// authenticated backend receipt API. Polling is bounded and cancellation-safe;
// the secret probe token is used only as a request header and is never returned.
func DoctorProbeV2(ctx context.Context, local DoctorV2Result, options DoctorProbeOptions) DoctorV2Result {
	result := local
	result.Mode = "probe"
	if local.Capture == nil || local.Status == DoctorFail {
		return result
	}
	if strings.TrimSpace(options.APIKey) == "" {
		result.Checks = append(result.Checks, failV2("configuration", "CREDENTIAL_MISSING", "A project ingestion credential is required", "SET_CREDENTIAL"))
		return finishDoctorV2(result)
	}
	base, err := url.Parse(options.Endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		result.Checks = append(result.Checks, failV2("configuration", "AUTH_FAILED", "The diagnostic endpoint is invalid", "CHECK_INGEST_CREDENTIAL"))
		return finishDoctorV2(result)
	}
	base.Path = "/api/diagnostics/v2/sessions"
	base.RawQuery = ""
	base.Fragment = ""
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	interval := options.PollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	body, _ := json.Marshal(map[string]string{"envelope_digest": local.Capture.SemanticDigest, "fixture_version": "doctor-v2", "trace_id": local.Capture.TraceID})
	request, _ := http.NewRequestWithContext(probeCtx, http.MethodPost, base.String(), bytes.NewReader(body))
	request.Header.Set("content-type", "application/json")
	request.Header.Set("x-api-key", options.APIKey)
	response, err := client.Do(request)
	if err != nil {
		return probeTransportFailure(result)
	}
	var session diagnosticSession
	err = decodeLimited(response, &session)
	response.Body.Close()
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 || !strings.HasPrefix(session.DiagnosticID, "diag_") {
		return probeTransportFailure(result)
	}
	receiptURL := strings.TrimSuffix(base.String(), "/") + "/" + url.PathEscape(session.DiagnosticID)
	defer func() {
		cleanupCtx, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		req, _ := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, receiptURL, nil)
		req.Header.Set("x-api-key", options.APIKey)
		if resp, e := client.Do(req); e == nil {
			resp.Body.Close()
		}
	}()
	current := diagnosticReceipt{DiagnosticID: session.DiagnosticID, Status: "pending", ExpiresAt: session.ExpiresAt}
	for {
		req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, receiptURL, nil)
		req.Header.Set("x-api-key", options.APIKey)
		if session.ProbeToken != "" {
			req.Header.Set("x-neatlogs-diagnostic-token", session.ProbeToken)
		}
		resp, e := client.Do(req)
		if e != nil {
			break
		}
		var next diagnosticReceipt
		e = decodeLimited(resp, &next)
		resp.Body.Close()
		if e != nil || resp.StatusCode != http.StatusOK {
			break
		}
		current = next
		if current.Status == "pass" || current.Status == "fail" || current.Status == "expired" {
			break
		}
		select {
		case <-probeCtx.Done():
			goto complete
		case <-time.After(interval):
		}
	}
complete:
	result.Probe = &DoctorV2Probe{DiagnosticID: session.DiagnosticID, ReceiptStatus: current.Status, ExpiresAt: current.ExpiresAt, Stages: current.Stages}
	if current.Status == "pass" && probeStagesComplete(current.Stages) && (current.BackendDigest == "" || current.BackendDigest == local.Capture.SemanticDigest) {
		result.Checks = append(result.Checks, DoctorV2Check{Name: "probe_visibility", Status: DoctorPass, ReasonCode: "DIAGNOSTIC_VISIBLE", Message: "The diagnostic trace reached the authenticated read path", RemediationCode: "NONE"})
		return finishDoctorV2(result)
	}
	code := "STAGE_PENDING"
	remediation := "WAIT_FOR_RECEIPT"
	if current.FirstFailure != nil {
		code = *current.FirstFailure
		remediation = "CONTACT_SUPPORT"
	} else if current.Status == "expired" {
		code = "DIAGNOSTIC_EXPIRED"
		remediation = "CREATE_NEW_SESSION"
	} else if current.BackendDigest != "" && current.BackendDigest != local.Capture.SemanticDigest {
		code = "DIGEST_MISMATCH"
		remediation = "CONTACT_SUPPORT"
	}
	result.Checks = append(result.Checks, failV2("probe_visibility", code, "The backend diagnostic did not reach confirmed visibility", remediation))
	return finishDoctorV2(result)
}

func probeStagesComplete(stages []DoctorV2Stage) bool {
	expected := []string{"auth", "schema_decode", "pii", "kafka", "raw_durable", "root_resolution", "simplified_durable", "visibility"}
	if len(stages) != len(expected) {
		return false
	}
	for i, v := range expected {
		if stages[i].Stage != v || stages[i].Status != "accepted" {
			return false
		}
	}
	return true
}
func probeTransportFailure(result DoctorV2Result) DoctorV2Result {
	result.Checks = append(result.Checks, failV2("probe_transport", "AUTH_FAILED", "The authenticated diagnostic session could not be reached", "CHECK_INGEST_CREDENTIAL"))
	return finishDoctorV2(result)
}
func decodeLimited(response *http.Response, destination any) error {
	if response == nil || response.Body == nil {
		return errors.New("empty response")
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(destination)
}
