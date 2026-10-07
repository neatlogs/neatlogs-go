package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodTrace = `{"_id":"t1","status":"success","finalizationStatus":"finalized","spanCount":2,"totalTokensUsed":5,
"spans":[{"span_id":"a","span_name":"root","node_type":"workflow"},{"span_id":"b","parent_span_id":"a","span_name":"chat","node_type":"llm"}]}`

func traceRun(t *testing.T, status int, body string, env map[string]string, args ...string) (int, string, string, string) {
	t.Helper()
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	var out, errOut bytes.Buffer
	values := map[string]string{"NEATLOGS_API_KEY": "secret-key", "NEATLOGS_ENDPOINT": server.URL}
	for k, v := range env {
		values[k] = v
	}
	code := runTrace(args, traceIO{stdout: &out, stderr: &errOut, env: func(k string) string { return values[k] }})
	return code, out.String(), errOut.String(), gotPath
}

func TestTraceGetPassesHealthyTrace(t *testing.T) {
	code, out, _, path := traceRun(t, 200, goodTrace, nil, "trace", "get", "t1", "--json")
	if code != 0 || path != "/api/traces/v3/t1" {
		t.Fatalf("code=%d path=%s", code, path)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || parsed["result"] != "pass" {
		t.Fatalf("bad output %q", out)
	}
}

func TestTraceGetFailsMissingParentAndUnnamedSpan(t *testing.T) {
	bad := `{"spanCount":2,"spans":[{"span_id":"a","parent_span_id":"zzz"},{"span_id":"b","span_name":"x"}]}`
	code, out, _, _ := traceRun(t, 200, bad, nil, "trace", "get", "t1", "--json")
	if code != 1 || !strings.Contains(out, "parents_resolve") || !strings.Contains(out, "spans_named") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestTraceGetExitCodesAndKeySafety(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{{404, 2}, {202, 2}, {401, 3}, {403, 3}, {500, 5}}
	for _, c := range cases {
		code, _, errOut, _ := traceRun(t, c.status, "{}", nil, "trace", "get", "t1")
		if code != c.want || strings.Contains(errOut, "secret-key") {
			t.Fatalf("status %d: code=%d err=%q", c.status, code, errOut)
		}
	}
	if code, _, _, _ := traceRun(t, 200, "not json", nil, "trace", "get", "t1"); code != 5 {
		t.Fatalf("invalid json code=%d", code)
	}
	if code, _, _, _ := traceRun(t, 200, goodTrace, map[string]string{"NEATLOGS_API_KEY": ""}, "trace", "get", "t1"); code != 3 {
		t.Fatalf("missing key code=%d", code)
	}
	if code, _, _, _ := traceRun(t, 200, goodTrace, nil, "trace", "get"); code != 4 {
		t.Fatalf("usage code=%d", code)
	}
}

func TestTraceGetEncodesTraceID(t *testing.T) {
	_, _, _, path := traceRun(t, 200, goodTrace, nil, "trace", "get", "a/b")
	if path != "/api/traces/v3/a%2Fb" {
		t.Fatalf("path=%s", path)
	}
}
