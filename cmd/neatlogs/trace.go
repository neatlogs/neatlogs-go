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
	return "Usage: neatlogs trace get <trace_id> [--json]\nReads a trace back with NEATLOGS_API_KEY (and optional NEATLOGS_ENDPOINT) and checks it."
}

func text(value any) string {
	s, _ := value.(string)
	return s
}

// checkTrace checks a trace as the backend returns it.
func checkTrace(trace map[string]any) []traceCheck {
	var spans []map[string]any
	if raw, ok := trace["spans"].([]any); ok {
		for _, item := range raw {
			if span, ok := item.(map[string]any); ok {
				spans = append(spans, span)
			}
		}
	}
	var checks []traceCheck
	add := func(name string, ok bool, message string) {
		status := "fail"
		if ok {
			status = "pass"
		}
		checks = append(checks, traceCheck{name, status, message})
	}
	add("has_spans", len(spans) > 0, fmt.Sprintf("%d span(s) returned", len(spans)))
	if count, ok := trace["spanCount"].(float64); ok {
		add("span_count_matches", int(count) == len(spans), fmt.Sprintf("spanCount=%d, returned=%d", int(count), len(spans)))
	}
	ids := map[string]bool{}
	for _, span := range spans {
		if id := text(span["span_id"]); id != "" {
			ids[id] = true
		}
	}
	orphans, unnamed, llm := 0, 0, 0
	for _, span := range spans {
		if parent := text(span["parent_span_id"]); parent != "" && !ids[parent] {
			orphans++
		}
		if text(span["span_name"]) == "" && text(span["node_name"]) == "" {
			unnamed++
		}
		kind := text(span["node_type"])
		if kind == "" {
			kind = text(span["span_type"])
		}
		if strings.Contains(strings.ToLower(kind), "llm") {
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
	if total, ok := trace["totalTokensUsed"].(float64); ok && llm > 0 {
		add("llm_token_usage", total > 0, fmt.Sprintf("LLM span(s): %d, totalTokensUsed=%d", llm, int(total)))
	}
	if status, ok := trace["finalizationStatus"]; ok {
		add("finalized", status == "finalized", fmt.Sprintf("finalizationStatus=%v", status))
	}
	return checks
}

// runTrace handles `neatlogs trace get <trace_id>`.
// exit: 0 pass, 1 check failed, 2 not ready or not found, 3 key, 4 usage, 5 error
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
	key := strings.TrimSpace(in.env("NEATLOGS_API_KEY"))
	if key == "" {
		fmt.Fprintln(in.stderr, "NEATLOGS_API_KEY is not set")
		return 3
	}
	endpoint := strings.TrimSpace(in.env("NEATLOGS_ENDPOINT"))
	if endpoint == "" {
		endpoint = "https://ingest.neatlogs.com"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		fmt.Fprintln(in.stderr, "NEATLOGS_ENDPOINT must be an absolute http(s) URL")
		return 4
	}
	target := url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	target.Path = "/api/traces/v3/" + traceID
	target.RawPath = "/api/traces/v3/" + url.PathEscape(traceID)
	client := in.client
	if client == nil {
		client = &http.Client{Timeout: in.timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	request, err := http.NewRequest(http.MethodGet, target.String(), nil)
	if err != nil {
		fmt.Fprintln(in.stderr, "Could not build the trace read request")
		return 5
	}
	request.Header.Set("x-api-key", key)
	response, err := client.Do(request)
	if err != nil {
		fmt.Fprintln(in.stderr, "Could not reach the trace read API")
		return 5
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == 401 || response.StatusCode == 403:
		fmt.Fprintln(in.stderr, "Trace read rejected the API key")
		return 3
	case response.StatusCode == 202 || response.StatusCode == 404 || response.StatusCode == 409:
		fmt.Fprintf(in.stderr, "Trace not ready or not found (HTTP %d); retry after the app flushes\n", response.StatusCode)
		return 2
	case response.StatusCode < 200 || response.StatusCode > 299:
		fmt.Fprintf(in.stderr, "Trace read failed (HTTP %d)\n", response.StatusCode)
		return 5
	}
	var trace map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&trace); err != nil || trace == nil {
		fmt.Fprintln(in.stderr, "Trace read returned invalid JSON")
		return 5
	}
	checks := checkTrace(trace)
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
	spans, _ := trace["spans"].([]any)
	id := text(trace["_id"])
	if id == "" {
		id = traceID
	}
	if jsonOutput {
		listing := make([]map[string]any, 0, len(spans))
		for _, item := range spans {
			if span, ok := item.(map[string]any); ok {
				name, kind := span["span_name"], span["node_type"]
				if name == nil {
					name = span["node_name"]
				}
				if kind == nil {
					kind = span["span_type"]
				}
				listing = append(listing, map[string]any{"span_id": span["span_id"], "parent_span_id": span["parent_span_id"], "name": name, "type": kind})
			}
		}
		encoded, _ := json.MarshalIndent(map[string]any{"trace_id": id, "status": trace["status"], "span_count": len(listing), "total_tokens": trace["totalTokensUsed"], "spans": listing, "checks": checks, "result": result}, "", "  ")
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
