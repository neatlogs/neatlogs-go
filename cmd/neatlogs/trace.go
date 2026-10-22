package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	traceDefaultHost = "https://app.neatlogs.com"
	tracePageLimit   = 50
	traceMaxPages    = 200
)

type traceCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type traceIO struct {
	stdout  io.Writer
	stderr  io.Writer
	env     func(string) string
	client  *http.Client
	timeout time.Duration
}

func defaultTraceIO() traceIO {
	return traceIO{stdout: os.Stdout, stderr: os.Stderr, env: os.Getenv, timeout: 5 * time.Second}
}

func traceUsage() string {
	return "Usage: neatlogs trace get <trace_id> [--json]\n" +
		"Reads a trace from the public API and checks it.\n" +
		"Needs NEATLOGS_TOKEN (service-account token with observability:read) and NEATLOGS_PROJECT_ID.\n" +
		"NEATLOGS_HOST sets the app origin (default https://app.neatlogs.com, EU: https://eu.app.neatlogs.com)."
}

func text(value any) string {
	s, _ := value.(string)
	return s
}

// checkTrace checks a trace and its spans as the public API returns them.
func checkTrace(trace map[string]any, spans []map[string]any) []traceCheck {
	var checks []traceCheck
	add := func(name string, ok bool, message string) {
		status := "fail"
		if ok {
			status = "pass"
		}
		checks = append(checks, traceCheck{name, status, message})
	}
	add("has_spans", len(spans) > 0, fmt.Sprintf("%d span(s) returned", len(spans)))
	if count, ok := trace["spansCount"].(float64); ok {
		add("span_count_matches", int(count) == len(spans), fmt.Sprintf("spansCount=%d, returned=%d", int(count), len(spans)))
	}
	ids := map[string]bool{}
	for _, span := range spans {
		if id := text(span["spanId"]); id != "" {
			ids[id] = true
		}
	}
	orphans, unnamed, llm := 0, 0, 0
	for _, span := range spans {
		if parent := text(span["parentSpanId"]); parent != "" && !ids[parent] {
			orphans++
		}
		if text(span["spanName"]) == "" {
			unnamed++
		}
		if strings.Contains(strings.ToLower(text(span["spanType"])), "llm") {
			llm++
		}
	}
	if orphans == 0 {
		add("parents_resolve", true, "every parent span is present")
	} else {
		add("parents_resolve", false, fmt.Sprintf("%d span(s) point at a missing parent", orphans))
	}
	if unnamed == 0 {
		add("spans_named", true, "every span has a name")
	} else {
		add("spans_named", false, fmt.Sprintf("%d span(s) have no name", unnamed))
	}
	if total, ok := trace["totalTokens"].(float64); ok && llm > 0 {
		add("llm_token_usage", total >= 0, fmt.Sprintf("LLM span(s): %d, totalTokens=%d (0 can mean the provider sent no usage)", llm, int(total)))
	}
	if status, ok := trace["finalizationStatus"]; ok {
		add("finalized", status == "finalized", fmt.Sprintf("finalizationStatus=%v", status))
	}
	return checks
}

// runTrace handles `neatlogs trace get <trace_id>`.
// exit: 0 pass, 1 check failed, 2 not ready or not found, 3 credentials, 4 usage, 5 error
func runTrace(arguments []string, in traceIO) int {
	jsonOutput := false
	var rest []string
	for _, argument := range arguments {
		switch argument {
		case "--json":
			jsonOutput = true
		case "--help", "-h":
			fmt.Fprintln(in.stdout, traceUsage())
			return 0
		default:
			rest = append(rest, argument)
		}
	}
	if len(rest) != 3 || rest[0] != "trace" || rest[1] != "get" || rest[2] == "" || strings.HasPrefix(rest[2], "-") {
		fmt.Fprintln(in.stderr, traceUsage())
		return 4
	}
	traceID := rest[2]
	token := strings.TrimSpace(in.env("NEATLOGS_TOKEN"))
	projectID := strings.TrimSpace(in.env("NEATLOGS_PROJECT_ID"))
	if token == "" {
		fmt.Fprintln(in.stderr, "NEATLOGS_TOKEN is not set")
		return 3
	}
	if projectID == "" {
		fmt.Fprintln(in.stderr, "NEATLOGS_PROJECT_ID is not set")
		return 3
	}
	host := strings.TrimSpace(in.env("NEATLOGS_HOST"))
	if host == "" {
		host = traceDefaultHost
	}
	parsed, err := url.Parse(host)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		fmt.Fprintln(in.stderr, "NEATLOGS_HOST must be an absolute http(s) URL")
		return 4
	}
	client := in.client
	if client == nil {
		client = &http.Client{Timeout: in.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	tracePath := "/api/v1/public/traces/" + traceID
	traceRawPath := "/api/v1/public/traces/" + url.PathEscape(traceID)

	// get does one GET and returns the data object, or an exit code
	get := func(path, rawPath, query string, spansCall bool) (map[string]any, int) {
		target := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: path, RawPath: rawPath, RawQuery: query}
		request, err := http.NewRequest(http.MethodGet, target.String(), nil)
		if err != nil {
			fmt.Fprintln(in.stderr, "Could not build the trace read request")
			return nil, 5
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("x-project-id", projectID)
		response, err := client.Do(request)
		if err != nil {
			fmt.Fprintln(in.stderr, "Could not reach the trace read API")
			return nil, 5
		}
		defer response.Body.Close()
		switch code := response.StatusCode; {
		case code == 401 || code == 403:
			fmt.Fprintf(in.stderr, "Trace read rejected the credentials (HTTP %d); check the token scope, project id and host\n", code)
			return nil, 3
		case code == 404 || (spansCall && code == 409):
			fmt.Fprintf(in.stderr, "Trace not ready or not found (HTTP %d); retry after the app flushes\n", code)
			return nil, 2
		case code == 429 || code == 503:
			fmt.Fprintf(in.stderr, "Trace read is rate limited or unavailable (HTTP %d); retry later\n", code)
			return nil, 5
		case code < 200 || code > 299:
			fmt.Fprintf(in.stderr, "Trace read failed (HTTP %d)\n", code)
			return nil, 5
		}
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&body); err != nil {
			fmt.Fprintln(in.stderr, "Trace read returned invalid JSON")
			return nil, 5
		}
		if body.Data == nil {
			fmt.Fprintln(in.stderr, "Trace read returned an unexpected response")
			return nil, 5
		}
		return body.Data, 0
	}

	trace, code := get(tracePath, traceRawPath, "", false)
	if code != 0 {
		return code
	}
	if trace["finalizationStatus"] == "dlq" {
		fmt.Fprintln(in.stderr, "Trace ingestion failed for good (finalizationStatus=dlq); retrying will not help")
		return 5
	}
	var spans []map[string]any
	cursor := ""
	for page := 0; page < traceMaxPages; page++ {
		query := fmt.Sprintf("limit=%d", tracePageLimit)
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		data, code := get(tracePath+"/spans", traceRawPath+"/spans", query, true)
		if code != 0 {
			return code
		}
		if raw, ok := data["spans"].([]any); ok {
			for _, item := range raw {
				if span, ok := item.(map[string]any); ok {
					spans = append(spans, span)
				}
			}
		}
		pageInfo, _ := data["page"].(map[string]any)
		cursor = text(pageInfo["nextCursor"])
		if cursor == "" {
			break
		}
		if page == traceMaxPages-1 {
			fmt.Fprintf(in.stderr, "Span pagination incomplete: still more spans after %d pages, so the trace was not checked\n", traceMaxPages)
			return 5
		}
	}

	checks := checkTrace(trace, spans)
	failed := false
	for _, check := range checks {
		if check.Status == "fail" {
			failed = true
		}
	}
	result := "pass"
	if failed {
		result = "fail"
	}
	id := text(trace["traceId"])
	if id == "" {
		id = traceID
	}
	if jsonOutput {
		listing := make([]map[string]any, 0, len(spans))
		for _, span := range spans {
			listing = append(listing, map[string]any{"span_id": span["spanId"], "parent_span_id": span["parentSpanId"], "name": span["spanName"], "type": span["spanType"]})
		}
		encoded, _ := json.MarshalIndent(map[string]any{"trace_id": id, "status": trace["status"], "span_count": len(listing), "total_tokens": trace["totalTokens"], "spans": listing, "checks": checks, "result": result}, "", "  ")
		fmt.Fprintln(in.stdout, string(encoded))
	} else {
		fmt.Fprintf(in.stdout, "trace %s: %s (%d spans)\n", id, result, len(spans))
		for _, check := range checks {
			label := "ok  "
			if check.Status == "fail" {
				label = "FAIL"
			}
			fmt.Fprintf(in.stdout, "  %s %s: %s\n", label, check.Name, check.Message)
		}
	}
	if failed {
		return 1
	}
	return 0
}
