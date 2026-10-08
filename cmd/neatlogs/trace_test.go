package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	traceData = `{"traceId":"t1","status":"success","finalizationStatus":"finalized","spansCount":2,"totalTokens":5}`
	spanA     = `{"spanId":"a","parentSpanId":null,"spanName":"root","spanType":"workflow"}`
	spanB     = `{"spanId":"b","parentSpanId":"a","spanName":"chat","spanType":"llm"}`
	lastPage  = `"page":{"hasMore":false,"limit":50,"nextCursor":null}`
)

func envelope(data string) string {
	return `{"success":true,"data":` + data + `,"requestId":"r1"}`
}

type traceCall struct{ path, query, auth, project string }

// route answers trace and spans requests; status applies to both unless the handler overrides.
func traceRun(t *testing.T, handler func(r *http.Request) (int, string), env map[string]string, args ...string) (int, string, string, []traceCall) {
	t.Helper()
	var calls []traceCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, traceCall{r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("x-project-id")})
		status, body := handler(r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	var out, errOut bytes.Buffer
	values := map[string]string{"NEATLOGS_TOKEN": "tok-secret", "NEATLOGS_PROJECT_ID": "proj-1", "NEATLOGS_HOST": server.URL}
	for k, v := range env {
		values[k] = v
	}
	code := runTrace(args, traceIO{stdout: &out, stderr: &errOut, env: func(k string) string { return values[k] }})
	return code, out.String(), errOut.String(), calls
}

func healthy(r *http.Request) (int, string) {
	if strings.HasSuffix(r.URL.Path, "/spans") {
		return 200, envelope(`{"spans":[` + spanA + `,` + spanB + `],` + lastPage + `}`)
	}
	return 200, envelope(traceData)
}

func TestTraceGetUsesPublicAPIWithBearerAndProjectID(t *testing.T) {
	code, out, _, calls := traceRun(t, healthy, nil, "trace", "get", "t1", "--json")
	if code != 0 || len(calls) != 2 {
		t.Fatalf("code=%d calls=%v", code, calls)
	}
	if calls[0].path != "/api/v1/public/traces/t1" || calls[1].path != "/api/v1/public/traces/t1/spans" || calls[1].query != "limit=50" {
		t.Fatalf("calls=%v", calls)
	}
	if calls[0].auth != "Bearer tok-secret" || calls[0].project != "proj-1" {
		t.Fatalf("headers=%v", calls[0])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || parsed["result"] != "pass" {
		t.Fatalf("bad output %q", out)
	}
}

func TestTraceGetFollowsSpanPages(t *testing.T) {
	handler := func(r *http.Request) (int, string) {
		if !strings.HasSuffix(r.URL.Path, "/spans") {
			return 200, envelope(traceData)
		}
		if r.URL.Query().Get("cursor") == "c2" {
			return 200, envelope(`{"spans":[` + spanB + `],` + lastPage + `}`)
		}
		return 200, envelope(`{"spans":[` + spanA + `],"page":{"hasMore":true,"limit":50,"nextCursor":"c2"}}`)
	}
	code, out, _, calls := traceRun(t, handler, nil, "trace", "get", "t1", "--json")
	if code != 0 || len(calls) != 3 || !strings.Contains(calls[2].query, "cursor=c2") || !strings.Contains(out, `"span_count": 2`) {
		t.Fatalf("code=%d calls=%v out=%s", code, calls, out)
	}
}

func TestTraceGetFailsMissingParentAndUnnamedSpan(t *testing.T) {
	handler := func(r *http.Request) (int, string) {
		if !strings.HasSuffix(r.URL.Path, "/spans") {
			return 200, envelope(traceData)
		}
		return 200, envelope(`{"spans":[{"spanId":"a","parentSpanId":"zzz","spanName":""},` + spanB + `],` + lastPage + `}`)
	}
	code, out, _, _ := traceRun(t, handler, nil, "trace", "get", "t1", "--json")
	if code != 1 || !strings.Contains(out, "parents_resolve") || !strings.Contains(out, "spans_named") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestTraceGetPendingFailsFinalizedAndZeroTokensPasses(t *testing.T) {
	handler := func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/spans") {
			return healthy(r)
		}
		return 200, envelope(`{"traceId":"t1","status":"failed","finalizationStatus":"pending","spansCount":2,"totalTokens":0}`)
	}
	code, out, _, _ := traceRun(t, handler, nil, "trace", "get", "t1", "--json")
	if code != 1 || !strings.Contains(out, `"finalized"`) {
		t.Fatalf("code=%d out=%s", code, out)
	}
	var parsed struct {
		Checks []traceCheck `json:"checks"`
	}
	_ = json.Unmarshal([]byte(out), &parsed)
	got := map[string]string{}
	for _, c := range parsed.Checks {
		got[c.Name] = c.Status
	}
	if got["finalized"] != "fail" || got["llm_token_usage"] != "pass" {
		t.Fatalf("checks=%v", got)
	}
}

func TestTraceGetExitCodesAndTokenSafety(t *testing.T) {
	for _, c := range []struct{ status, want int }{{404, 2}, {401, 3}, {403, 3}, {500, 5}, {429, 5}, {409, 5}} {
		status := c.status
		code, _, errOut, _ := traceRun(t, func(*http.Request) (int, string) { return status, "{}" }, nil, "trace", "get", "t1")
		if code != c.want || strings.Contains(errOut, "tok-secret") {
			t.Fatalf("status %d: code=%d err=%q", c.status, code, errOut)
		}
	}
	if code, _, _, _ := traceRun(t, func(*http.Request) (int, string) { return 200, "not json" }, nil, "trace", "get", "t1"); code != 5 {
		t.Fatalf("invalid json code=%d", code)
	}
}

func TestTraceGetSpans409IsNotReady(t *testing.T) {
	handler := func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/spans") {
			return 409, "{}"
		}
		return 200, envelope(traceData)
	}
	if code, _, _, _ := traceRun(t, handler, nil, "trace", "get", "t1"); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

func TestTraceGetNeedsTokenProjectAndTraceID(t *testing.T) {
	if code, _, _, _ := traceRun(t, healthy, map[string]string{"NEATLOGS_TOKEN": ""}, "trace", "get", "t1"); code != 3 {
		t.Fatalf("missing token code=%d", code)
	}
	if code, _, _, _ := traceRun(t, healthy, map[string]string{"NEATLOGS_PROJECT_ID": ""}, "trace", "get", "t1"); code != 3 {
		t.Fatalf("missing project code=%d", code)
	}
	if code, _, _, _ := traceRun(t, healthy, nil, "trace", "get"); code != 4 {
		t.Fatalf("usage code=%d", code)
	}
}

func TestTraceGetEncodesTraceID(t *testing.T) {
	_, _, _, calls := traceRun(t, healthy, nil, "trace", "get", "a/b")
	if len(calls) == 0 || calls[0].path != "/api/v1/public/traces/a%2Fb" {
		t.Fatalf("calls=%v", calls)
	}
}

func TestTraceGetDLQIsTerminalBeforeReadingSpans(t *testing.T) {
	handler := func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/spans") {
			return 409, "{}"
		}
		return 200, envelope(`{"traceId":"t1","status":"failed","finalizationStatus":"dlq","spansCount":0,"totalTokens":0}`)
	}
	code, _, errOut, calls := traceRun(t, handler, nil, "trace", "get", "t1")
	if code != 5 || len(calls) != 1 || !strings.Contains(errOut, "dlq") {
		t.Fatalf("code=%d calls=%d err=%q", code, len(calls), errOut)
	}
}

func TestTraceGetPendingWith409OnSpansIsNotReady(t *testing.T) {
	handler := func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/spans") {
			return 409, "{}"
		}
		return 200, envelope(`{"traceId":"t1","status":"success","finalizationStatus":"pending","spansCount":2,"totalTokens":0}`)
	}
	code, _, errOut, _ := traceRun(t, handler, nil, "trace", "get", "t1")
	if code != 2 || !strings.Contains(errOut, "not ready") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
}

func TestTraceGetPaginationCapIsIncompleteNotACheck(t *testing.T) {
	handler := func(r *http.Request) (int, string) {
		if !strings.HasSuffix(r.URL.Path, "/spans") {
			return 200, envelope(traceData)
		}
		return 200, envelope(`{"spans":[` + spanA + `],"page":{"hasMore":true,"limit":50,"nextCursor":"more"}}`)
	}
	code, out, errOut, calls := traceRun(t, handler, nil, "trace", "get", "t1", "--json")
	if code != 5 || out != "" || len(calls) != 201 || !strings.Contains(errOut, "pagination incomplete") {
		t.Fatalf("code=%d out=%q calls=%d err=%q", code, out, len(calls), errOut)
	}
}
