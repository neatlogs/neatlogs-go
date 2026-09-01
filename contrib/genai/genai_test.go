package genai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	google "google.golang.org/genai"

	neatlogs "github.com/neatlogs/neatlogs-go"
	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

func TestResponseAccumulatorPreservesChoicesAndSemanticStreamEvidence(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "genai-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "stream", attrs.KindLLM)
	acc := newResponseAccumulator()
	recordStreamChunk(span, acc, &google.GenerateContentResponse{Candidates: []*google.Candidate{
		{Index: 0, Content: &google.Content{Role: "model", Parts: []*google.Part{{Text: "A"}}}},
		{Index: 1, Content: &google.Content{Role: "model", Parts: []*google.Part{
			{Text: "X"},
			{FunctionCall: &google.FunctionCall{ID: "call-1", Name: "weather", Args: map[string]any{"city": "Paris"}}},
		}}},
	}})
	recordStreamChunk(span, acc, &google.GenerateContentResponse{Candidates: []*google.Candidate{
		{Index: 0, FinishReason: google.FinishReasonStop, Content: &google.Content{Role: "model", Parts: []*google.Part{{Text: "B"}}}},
		{Index: 1, FinishReason: google.FinishReasonMaxTokens, Content: &google.Content{Role: "model", Parts: []*google.Part{
			{Text: "Y"},
			{FunctionCall: &google.FunctionCall{ID: "call-1", Name: "weather", Args: map[string]any{"unit": "C"}}},
		}}},
	}})
	finalizeStream(span, acc, true, false)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	var got *tracetest.SpanStub
	for i := range sink.GetSpans() {
		candidate := sink.GetSpans()[i]
		if candidate.Name == "stream" {
			got = &candidate
			break
		}
	}
	if got == nil {
		t.Fatal("stream span not exported")
	}
	assertStringAttribute(t, got.Attributes, attrs.LLMOutputMessagePrefix+"0.content", "AB")
	assertStringAttribute(t, got.Attributes, attrs.LLMOutputMessagePrefix+"1.content", "XY")
	assertStringAttribute(t, got.Attributes, attrs.LLMChoicePrefix+"0.finish_reason", string(google.FinishReasonStop))
	assertStringAttribute(t, got.Attributes, attrs.LLMChoicePrefix+"1.finish_reason", string(google.FinishReasonMaxTokens))
	assertStringAttribute(t, got.Attributes, attrs.LLMToolCallPrefix+"0.id", "call-1")
	assertIntAttribute(t, got.Attributes, attrs.LLMToolCallPrefix+"0.choice_index", 1)
	assertStringAttribute(t, got.Attributes, attrs.LLMToolCallPrefix+"0.arguments", `{"city":"Paris","unit":"C"}`)
	if _, ok := findAttribute(got.Attributes, attrs.LLMToolCallPrefix+"1.name"); ok {
		t.Fatal("streamed fragments of one tool call were emitted as duplicate calls")
	}
	assertIntAttribute(t, got.Attributes, attrs.StreamChunkCount, 2)
	assertBoolAttribute(t, got.Attributes, attrs.StreamCancelled, true)
	if got.Status.Code != codes.Unset {
		t.Fatalf("cancelled stream status = %v, want UNSET", got.Status.Code)
	}
	if len(got.Events) != 2 {
		t.Fatalf("stream events = %d, want one per chunk", len(got.Events))
	}
	for index, event := range got.Events {
		if event.Name != attrs.StreamChunkEvent {
			t.Fatalf("event %d name = %q", index, event.Name)
		}
		assertIntAttribute(t, event.Attributes, attrs.StreamChunkIndex, index)
		if value, ok := findAttribute(event.Attributes, attrs.StreamChunkSummary); !ok || value.AsString() == "" {
			t.Fatalf("event %d did not preserve its semantic summary", index)
		} else if value.AsString() == "A" || value.AsString() == "X" {
			t.Fatalf("event %d contains raw chunk content", index)
		}
	}
}

func TestTypedMediaPreservesInlineDigestAndFileReference(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "media-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	raw := []byte("full-image-bytes")
	content := &google.Content{Role: "user", Parts: []*google.Part{
		{Text: "inspect both"},
		{InlineData: &google.Blob{Data: raw, MIMEType: "image/png"}},
		{FileData: &google.FileData{FileURI: "gs://bucket/report.pdf", MIMEType: "application/pdf"}},
		{FileData: &google.FileData{
			FileURI:  "https://user:password@bucket.example/private.png?X-Amz-Signature=secret#fragment",
			MIMEType: "image/png",
		}},
	}}
	_, span, end := neatlogs.StartProviderSpan(ctx, "media", attrs.KindLLM)
	setInputMessages(span, []*google.Content{content}, nil)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	var got *tracetest.SpanStub
	for _, candidate := range sink.GetSpans() {
		if candidate.Name == "media" {
			copy := candidate
			got = &copy
		}
	}
	if got == nil {
		t.Fatal("media span not exported")
	}
	digest := sha256.Sum256(raw)
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.0.type", "image")
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.0.sha256", fmt.Sprintf("%x", digest))
	assertIntAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.0.byte_length", len(raw))
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.1.type", "document")
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.1.reference", "gs://bucket/report.pdf")
	assertStringAttribute(t, got.Attributes, attrs.LLMInputMessagePrefix+"0.media.2.reference", "https://bucket.example/private.png")
}

func TestLargeTypedMediaUploadsMaskedBytesAndExportsCanonicalReference(t *testing.T) {
	original := []byte(strings.Repeat("private-image", 8_000))
	masked := []byte(strings.Repeat("masked-image", 8_000))
	maskedDigest := sha256.Sum256(masked)
	uploadID := "0198f1ea-70ce-7c6d-8bbc-b08a19c58280"
	referenceID := uploadID
	var uploaded []byte
	var prepare map[string]any
	var mu sync.Mutex
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/telemetry/uploads":
			if request.Header.Get("x-api-key") != "project-key" {
				t.Errorf("prepare auth = %q", request.Header.Get("x-api-key"))
			}
			var decoded map[string]any
			if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
				t.Errorf("decode prepare: %v", err)
			}
			mu.Lock()
			prepare = decoded
			mu.Unlock()
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(map[string]any{
				"upload_id": uploadID, "state": "prepared",
				"expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
				"upload": map[string]any{
					"method": "PUT", "url": server.URL + "/object?signature=must-not-export",
					"headers": map[string]string{"x-upload-secret": "must-not-export"},
				},
				"reference": map[string]any{
					"id": referenceID, "purpose": "typed_media", "sha256": fmt.Sprintf("%x", maskedDigest),
					"byte_length": len(masked), "mime_type": "image/png", "content_encoding": "identity", "state": "prepared",
				},
			})
		case "/object":
			if request.Header.Get("x-api-key") != "" {
				t.Error("project API key leaked to object PUT")
			}
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read upload: %v", err)
			}
			mu.Lock()
			uploaded = body
			mu.Unlock()
			response.WriteHeader(http.StatusNoContent)
		case "/v1/telemetry/uploads/" + uploadID + "/complete":
			_ = json.NewEncoder(response).Encode(map[string]any{
				"upload_id": uploadID, "state": "ready",
				"reference": map[string]any{
					"id": referenceID, "purpose": "typed_media", "sha256": fmt.Sprintf("%x", maskedDigest),
					"byte_length": len(masked), "mime_type": "image/png", "content_encoding": "identity", "state": "ready",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previousTransport }()

	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{
		APIKey: "project-key", Endpoint: server.URL, WorkflowName: "media-upload-test", EnableUploads: true,
		Mask: func(_ context.Context, data neatlogs.SpanData) (*neatlogs.SpanData, error) {
			for index := range data.Attributes {
				if strings.HasPrefix(string(data.Attributes[index].Key), "neatlogs.internal.media_payload.") {
					data.Attributes[index] = attribute.ByteSlice(string(data.Attributes[index].Key), masked)
				}
			}
			return &data, nil
		},
	}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	content := &google.Content{Role: "user", Parts: []*google.Part{{InlineData: &google.Blob{Data: original, MIMEType: "image/png"}}}}
	_, span, end := neatlogs.StartProviderSpan(ctx, "large-media", attrs.KindLLM)
	setInputMessages(span, []*google.Content{content}, nil)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotUploaded := append([]byte(nil), uploaded...)
	gotPrepare := make(map[string]any, len(prepare))
	for key, value := range prepare {
		gotPrepare[key] = value
	}
	mu.Unlock()
	if string(gotUploaded) != string(masked) {
		t.Fatalf("uploaded bytes were not the masked bytes: got=%d want=%d", len(gotUploaded), len(masked))
	}
	if gotPrepare["purpose"] != "typed_media" || gotPrepare["sha256"] != fmt.Sprintf("%x", maskedDigest) || gotPrepare["payload_schema"] != "neatlogs.media.v1" {
		t.Fatalf("prepare metadata = %#v", gotPrepare)
	}
	var got *tracetest.SpanStub
	for _, candidate := range sink.GetSpans() {
		if candidate.Name == "large-media" {
			copy := candidate
			got = &copy
		}
	}
	if got == nil {
		t.Fatal("large media span not exported")
	}
	prefix := attrs.LLMInputMessagePrefix + "0.media.0."
	assertStringAttribute(t, got.Attributes, prefix+"id", referenceID)
	assertStringAttribute(t, got.Attributes, prefix+"source", "uploaded")
	assertStringAttribute(t, got.Attributes, prefix+"state", "available")
	for _, value := range got.Attributes {
		rendered := value.Value.Emit()
		if strings.HasPrefix(string(value.Key), "neatlogs.internal.media_payload.") ||
			strings.Contains(rendered, "must-not-export") || strings.Contains(rendered, "private-image") {
			t.Fatalf("telemetry leaked private upload material: %s=%s", value.Key, rendered)
		}
	}
}

func TestMissingToolCallIDGetsStableExplicitlyMarkedIdentity(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "tool-id-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "tool-id", attrs.KindLLM)
	acc := newResponseAccumulator()
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			Name: "lookup", Args: map[string]any{"query": "weather"},
		}}}},
	}}})
	acc.apply(span)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "tool-id" {
			continue
		}
		id, ok := findAttribute(candidate.Attributes, attrs.LLMToolCallPrefix+"0.id")
		if !ok || !strings.HasPrefix(id.AsString(), "nl_") {
			t.Fatalf("synthetic tool ID = %q, %v", id.AsString(), ok)
		}
		assertBoolAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.id_synthetic", true)
		assertIntAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.tool_call_index", 0)
		return
	}
	t.Fatal("tool span not exported")
}

func TestMissingToolCallIDsNeverMergeByPositionAlone(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "tool-separation-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "tool-separation", attrs.KindLLM)
	acc := newResponseAccumulator()
	for _, name := range []string{"first", "second"} {
		acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
			Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
				Name: name, Args: map[string]any{"value": name},
			}}}},
		}}})
	}
	acc.apply(span)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "tool-separation" {
			continue
		}
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.name", "first")
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"1.name", "second")
		assertBoolAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.id_synthetic", true)
		assertBoolAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"1.id_synthetic", true)
		return
	}
	t.Fatal("tool separation span not exported")
}

func TestIDLessStreamedToolCallsMergePartialArgsByChoiceAndPosition(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "partial-tool-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	continued, complete := true, false
	_, span, end := neatlogs.StartProviderSpan(ctx, "partial-tools", attrs.KindLLM)
	acc := newResponseAccumulator()
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{
		{Index: 0, Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			Name: "weather", WillContinue: &continued, PartialArgs: []*google.PartialArg{
				{JsonPath: "$.location.city", StringValue: "San", WillContinue: &continued},
				{JsonPath: "$.items[0].value", StringValue: "A", WillContinue: &continued},
			},
		}}}}},
		{Index: 1, Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			Name: "search", WillContinue: &continued, PartialArgs: []*google.PartialArg{
				{JsonPath: "$['query']", StringValue: "Par", WillContinue: &continued},
			},
		}}}}},
	}})
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{
		{Index: 0, Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			WillContinue: &complete, PartialArgs: []*google.PartialArg{
				{JsonPath: "$.location.city", StringValue: " Francisco", WillContinue: &complete},
				{JsonPath: "$.items[0].value", StringValue: "B", WillContinue: &complete},
			},
		}}}}},
		{Index: 1, Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			WillContinue: &complete, PartialArgs: []*google.PartialArg{
				{JsonPath: "$['query']", StringValue: "is", WillContinue: &complete},
			},
		}}}}},
	}})
	acc.apply(span)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "partial-tools" {
			continue
		}
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.name", "weather")
		assertIntAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.choice_index", 0)
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"0.arguments",
			`{"items":[{"value":"AB"}],"location":{"city":"San Francisco"}}`)
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"1.name", "search")
		assertIntAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"1.choice_index", 1)
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolCallPrefix+"1.arguments", `{"query":"Paris"}`)
		if _, ok := findAttribute(candidate.Attributes, attrs.LLMToolCallPrefix+"2.name"); ok {
			t.Fatal("continued ID-less calls were emitted as duplicate tool calls")
		}
		return
	}
	t.Fatal("partial tool span not exported")
}

func TestIDLessMultiCallContinuationCompactsAfterEarlierCallCompletes(t *testing.T) {
	continued, complete := true, false
	acc := newResponseAccumulator()
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{
			{FunctionCall: &google.FunctionCall{Name: "first", WillContinue: &continued, PartialArgs: []*google.PartialArg{
				{JsonPath: "$.value", StringValue: "A1", WillContinue: &continued},
			}}},
			{FunctionCall: &google.FunctionCall{Name: "second", WillContinue: &continued, PartialArgs: []*google.PartialArg{
				{JsonPath: "$.value", StringValue: "B1", WillContinue: &continued},
			}}},
		}},
	}}})
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{
			{FunctionCall: &google.FunctionCall{WillContinue: &complete, PartialArgs: []*google.PartialArg{
				{JsonPath: "$.value", StringValue: "A2", WillContinue: &complete},
			}}},
			{FunctionCall: &google.FunctionCall{WillContinue: &continued, PartialArgs: []*google.PartialArg{
				{JsonPath: "$.value", StringValue: "B2", WillContinue: &continued},
			}}},
		}},
	}}})
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			WillContinue: &complete, PartialArgs: []*google.PartialArg{
				{JsonPath: "$.value", StringValue: "B3", WillContinue: &complete},
			},
		}}}},
	}}})

	choice := acc.choices[0]
	if choice == nil || len(choice.toolCalls) != 2 {
		t.Fatalf("tool calls = %#v, want exactly two logical calls", choice)
	}
	if got := mustJSON(choice.toolCalls[0].arguments); got != `{"value":"A1A2"}` {
		t.Fatalf("first arguments = %s", got)
	}
	if got := mustJSON(choice.toolCalls[1].arguments); got != `{"value":"B1B2B3"}` {
		t.Fatalf("second arguments = %s", got)
	}
	if got := len(choice.activeIDlessCalls); got != 0 {
		t.Fatalf("active calls after completion = %d, want 0", got)
	}
}

func TestInterleavedIDBearingCallsNeverMergeByActivePosition(t *testing.T) {
	continued, complete := true, false
	acc := newResponseAccumulator()
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			ID: "call-a", Name: "first", Args: map[string]any{"first": "A1"}, WillContinue: &continued,
		}}}},
	}}})
	choice := acc.choices[0]
	if choice == nil || len(choice.activeIDlessCalls) != 0 {
		t.Fatalf("active ID-less calls = %#v after identified call", choice)
	}
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			ID: "call-b", Name: "second", Args: map[string]any{"second": "B1"}, WillContinue: &complete,
		}}}},
	}}})
	acc.add(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
		Content: &google.Content{Parts: []*google.Part{{FunctionCall: &google.FunctionCall{
			ID: "call-a", Args: map[string]any{"first_tail": "A2"}, WillContinue: &complete,
		}}}},
	}}})

	if choice == nil || len(choice.toolCalls) != 2 {
		t.Fatalf("tool calls = %#v, want two calls with distinct provider IDs", choice)
	}
	first := choice.toolCalls[choice.toolPositionsByID["call-a"]]
	second := choice.toolCalls[choice.toolPositionsByID["call-b"]]
	if first == nil || first.id != "call-a" || first.name != "first" ||
		mustJSON(first.arguments) != `{"first":"A1","first_tail":"A2"}` {
		t.Fatalf("first call = %#v", first)
	}
	if second == nil || second.id != "call-b" || second.name != "second" ||
		mustJSON(second.arguments) != `{"second":"B1"}` {
		t.Fatalf("second call = %#v", second)
	}
	if first == second {
		t.Fatal("different provider IDs resolved to the same accumulated call")
	}
}

func TestPartialArgJSONPathSupportsQuotedEscapesAndEmbeddedBrackets(t *testing.T) {
	call := &accumulatedToolCall{
		arguments:            make(map[string]any),
		partialContinuations: make(map[string]bool),
	}
	call.applyPartialArg(&google.PartialArg{
		JsonPath:    `$["double\"quote]"]`,
		StringValue: "double",
	})
	call.applyPartialArg(&google.PartialArg{
		JsonPath:    `$[ "array]key" ][0]['single\'quote']`,
		StringValue: "nested",
	})

	if got := mustJSON(call.arguments); got != `{"array]key":[{"single'quote":"nested"}],"double\"quote]":"double"}` {
		t.Fatalf("partial arguments = %s", got)
	}
}

func TestPartialArgJSONPathValidatesUTF16SurrogateEscapes(t *testing.T) {
	segments, ok := parseJSONPath(`$["emoji-\uD83D\uDE00"]`)
	if !ok || len(segments) != 1 || !segments[0].isKey || segments[0].key != "emoji-\U0001F600" {
		t.Fatalf("valid surrogate pair parsed as %#v, %v", segments, ok)
	}

	invalid := []string{
		`$["\uD800"]`,
		`$["\uDC00"]`,
		`$["\uD800\u0041"]`,
		`$["\uDC00\uD800"]`,
	}
	for _, path := range invalid {
		if segments, ok := parseJSONPath(path); ok {
			t.Fatalf("parseJSONPath(%q) = %#v, true; want invalid UTF-16 rejected", path, segments)
		}
	}
}

func TestPartialArgJSONPathParserIsBounded(t *testing.T) {
	tooDeep := "$" + strings.Repeat(".a", maxJSONPathSegments+1)
	invalid := []string{
		"$." + strings.Repeat("a", maxJSONPathLength),
		tooDeep,
		fmt.Sprintf("$.items[%d]", maxJSONPathArrayIndex+1),
		"$.items[01]",
		"$.items[*]",
		"$..name",
		`$["unterminated]`,
	}
	for _, path := range invalid {
		if segments, ok := parseJSONPath(path); ok {
			t.Fatalf("parseJSONPath(%q) = %#v, true; want rejected", path, segments)
		}
	}

	call := &accumulatedToolCall{
		arguments:            map[string]any{"retained": true},
		partialContinuations: make(map[string]bool),
	}
	for _, path := range invalid {
		call.applyPartialArg(&google.PartialArg{JsonPath: path, StringValue: "must-not-apply"})
	}
	if got := mustJSON(call.arguments); got != `{"retained":true}` {
		t.Fatalf("rejected selectors partially mutated arguments: %s", got)
	}
}

func TestToolDefinitionsPreserveTypeAndCompleteConfiguration(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "tool-definition-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	_, span, end := neatlogs.StartProviderSpan(ctx, "tool-definitions", attrs.KindLLM)
	setToolDefinitions(span, &google.GenerateContentConfig{Tools: []*google.Tool{{
		FunctionDeclarations: []*google.FunctionDeclaration{{
			Name: "lookup", Description: "Find a record",
			ParametersJsonSchema: map[string]any{"type": "object"},
			ResponseJsonSchema:   map[string]any{"description": "response-sensitive-marker"},
		}},
		GoogleSearch: &google.GoogleSearch{ExcludeDomains: []string{"private.example"}},
	}}})
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "tool-definitions" {
			continue
		}
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"0.type", "function")
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"0.name", "lookup")
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"0.description", "Find a record")
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"0.input_schema", `{"type":"object"}`)
		assertStringAttribute(t, candidate.Attributes, attrs.LLMToolPrefix+"1.type", "google_search")
		for index, marker := range []string{"response-sensitive-marker", "private.example"} {
			configuration, ok := findAttribute(candidate.Attributes, attrs.LLMToolPrefix+fmt.Sprintf("%d.configuration", index))
			if !ok || !strings.Contains(configuration.AsString(), marker) {
				t.Fatalf("tool %d configuration = %q, %v; want marker %q", index, configuration.AsString(), ok, marker)
			}
		}
		for index := range 2 {
			if _, ok := findAttribute(candidate.Attributes, attrs.LLMToolPrefix+fmt.Sprintf("%d.definition", index)); ok {
				t.Fatalf("legacy unclassified definition attribute was emitted for tool %d", index)
			}
		}
		return
	}
	t.Fatal("tool definition span not exported")
}

func TestFunctionToolConfigurationUsesCanonicalSensitiveFieldsOnce(t *testing.T) {
	definitions, truncated := collectToolDefinitions(&google.GenerateContentConfig{Tools: []*google.Tool{{
		FunctionDeclarations: []*google.FunctionDeclaration{{
			Name:        "lookup-sensitive-marker",
			Description: "description-sensitive-marker",
			ParametersJsonSchema: map[string]any{
				"type":        "object",
				"description": "schema-sensitive-marker",
			},
		}},
	}}})
	if truncated != 0 || len(definitions) != 1 {
		t.Fatalf("definitions, truncated = %#v, %d; want one definition", definitions, truncated)
	}
	definition := definitions[0]
	if definition.name != "lookup-sensitive-marker" || definition.description != "description-sensitive-marker" {
		t.Fatalf("canonical name/description = %q/%q", definition.name, definition.description)
	}
	if !strings.Contains(definition.inputSchema, "schema-sensitive-marker") {
		t.Fatalf("canonical input schema = %s", definition.inputSchema)
	}
	for _, duplicate := range []string{
		"lookup-sensitive-marker", "description-sensitive-marker", "schema-sensitive-marker",
	} {
		if strings.Contains(definition.configurationJSON, duplicate) {
			t.Fatalf("configuration duplicates canonical sensitive field %q: %s", duplicate, definition.configurationJSON)
		}
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(definition.configurationJSON), &metadata); err != nil {
		t.Fatal(err)
	}
	for _, duplicateKey := range []string{"name", "description", "parameters", "parametersJsonSchema"} {
		if _, exists := metadata[duplicateKey]; exists {
			t.Fatalf("configuration contains duplicate key %q: %s", duplicateKey, definition.configurationJSON)
		}
	}
}

func TestToolDefinitionsExcludeAllProviderAuthenticationShapes(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "safe-tool-test"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	config := &google.GenerateContentConfig{Tools: []*google.Tool{{
		Retrieval: &google.Retrieval{ExternalAPI: &google.ExternalAPI{
			Endpoint: "https://url-user:url-password@example.com/search?access_token=url-token#fragment",
			APIAuth: &google.APIAuth{APIKeyConfig: &google.APIAuthAPIKeyConfig{
				APIKeySecretVersion: "deprecated-secret-ref", APIKeyString: "deprecated-api-key",
			}},
			AuthConfig: &google.AuthConfig{
				APIKey:              "direct-api-key",
				APIKeyConfig:        &google.APIKeyConfig{APIKeySecret: "api-key-secret-ref", APIKeyString: "nested-api-key"},
				HTTPBasicAuthConfig: &google.AuthConfigHTTPBasicAuthConfig{CredentialSecret: "basic-secret-ref"},
				OauthConfig:         &google.AuthConfigOauthConfig{AccessToken: "oauth-access-token", ServiceAccount: "oauth-service-account"},
				OidcConfig:          &google.AuthConfigOidcConfig{IDToken: "oidc-id-token", ServiceAccount: "oidc-service-account"},
			},
		}},
		GoogleMaps: &google.GoogleMaps{AuthConfig: &google.AuthConfig{
			APIKey:      "maps-api-key",
			OauthConfig: &google.AuthConfigOauthConfig{AccessToken: "maps-oauth-token"},
		}},
		ParallelAISearch: &google.ToolParallelAISearch{
			APIKey: "parallel-api-key", CustomConfigs: map[string]any{"authorization": "custom-bearer-token"},
		},
		MCPServers: []*google.MCPServer{{Name: "safe-server", StreamableHTTPTransport: &google.StreamableHTTPTransport{
			URL:     "https://mcp-user:mcp-password@example.com/mcp?token=mcp-query-token",
			Headers: map[string]string{"Authorization": "Bearer mcp-header-token", "Cookie": "session=mcp-cookie"},
			Timeout: 2 * time.Second,
		}}},
	}}}

	_, span, end := neatlogs.StartProviderSpan(ctx, "safe-tools", attrs.KindLLM)
	setToolDefinitions(span, config)
	end()
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range sink.GetSpans() {
		if candidate.Name != "safe-tools" {
			continue
		}
		var telemetry strings.Builder
		for _, value := range candidate.Attributes {
			telemetry.WriteString(value.Value.Emit())
		}
		captured := telemetry.String()
		for _, secret := range []string{
			"url-user", "url-password", "url-token", "deprecated-secret-ref", "deprecated-api-key",
			"direct-api-key", "api-key-secret-ref", "nested-api-key", "basic-secret-ref",
			"oauth-access-token", "oauth-service-account", "oidc-id-token", "oidc-service-account",
			"maps-api-key", "maps-oauth-token", "parallel-api-key", "custom-bearer-token",
			"mcp-user", "mcp-password", "mcp-query-token", "mcp-header-token", "mcp-cookie",
		} {
			if strings.Contains(captured, secret) {
				t.Fatalf("tool telemetry leaked %q: %s", secret, captured)
			}
		}
		if !strings.Contains(captured, "https://example.com/search") ||
			!strings.Contains(captured, "https://example.com/mcp") || !strings.Contains(captured, "safe-server") {
			t.Fatalf("safe tool configuration was not retained: %s", captured)
		}
		return
	}
	t.Fatal("safe tool span not exported")
}

func TestToolDefinitionCapPreservesResponseAndUsageUnderDefaultAttributeLimit(t *testing.T) {
	limits := sdktrace.NewSpanLimits()
	limits.AttributeCountLimit = 128
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithRawSpanLimits(limits),
		sdktrace.WithSpanProcessor(recorder),
	)
	_, span := provider.Tracer("tool-budget-test").Start(context.Background(), "tool-budget")

	declarations := make([]*google.FunctionDeclaration, 0, 32)
	for index := range 32 {
		declarations = append(declarations, &google.FunctionDeclaration{
			Name: fmt.Sprintf("tool_%d", index), Description: "described tool",
			ParametersJsonSchema: map[string]any{"type": "object"},
		})
	}
	config := &google.GenerateContentConfig{Tools: []*google.Tool{{FunctionDeclarations: declarations}}}
	finalizeResponse(span, &google.GenerateContentResponse{
		ResponseID: "response-kept",
		Candidates: []*google.Candidate{{Index: 0, FinishReason: google.FinishReasonStop,
			Content: &google.Content{Parts: []*google.Part{{Text: "authoritative output"}}}}},
		UsageMetadata: &google.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 11, CandidatesTokenCount: 7, TotalTokenCount: 18,
		},
	})
	setToolDefinitions(span, config)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	got := ended[0]
	assertStringAttribute(t, got.Attributes(), attrs.LLMOutputMessagePrefix+"0.content", "authoritative output")
	assertStringAttribute(t, got.Attributes(), attrs.LLMResponseID, "response-kept")
	assertIntAttribute(t, got.Attributes(), attrs.LLMTokenPrompt, 11)
	assertIntAttribute(t, got.Attributes(), attrs.LLMTokenCompletion, 7)
	assertIntAttribute(t, got.Attributes(), attrs.LLMTokenTotal, 18)
	assertIntAttribute(t, got.Attributes(), attrs.LLMToolsTruncated, len(declarations)-maxCapturedToolDefinitions)
	if _, ok := findAttribute(got.Attributes(), attrs.LLMToolPrefix+fmt.Sprintf("%d.type", maxCapturedToolDefinitions)); ok {
		t.Fatal("tool definition capture exceeded its configured cap")
	}
}

func TestStreamContextCancellationIsUnsetBeforeFirstResponseAndMidStream(t *testing.T) {
	for _, test := range []struct {
		name        string
		beforeFirst bool
	}{
		{name: "before_first_response", beforeFirst: true},
		{name: "mid_stream", beforeFirst: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseCtx := context.Background()
			ctx, cancel := context.WithCancel(baseCtx)
			sink := tracetest.NewInMemoryExporter()
			shutdown, err := neatlogs.Init(baseCtx, neatlogs.Config{WorkflowName: test.name}, neatlogs.WithExporter(sink))
			if err != nil {
				t.Fatal(err)
			}
			defer shutdown(baseCtx)

			models := &GenAIModels{
				provider: geminiProvider,
				system:   geminiSystem,
				stream: func(streamCtx context.Context, _ string, _ []*google.Content, _ *google.GenerateContentConfig) iter.Seq2[*google.GenerateContentResponse, error] {
					return func(yield func(*google.GenerateContentResponse, error) bool) {
						if test.beforeFirst {
							cancel()
							yield(nil, streamCtx.Err())
							return
						}
						if !yield(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
							Content: &google.Content{Parts: []*google.Part{{Text: "partial"}}},
						}}}, nil) {
							return
						}
						cancel()
						yield(nil, streamCtx.Err())
					}
				},
			}

			var streamErr error
			for _, err := range models.GenerateContentStream(ctx, "gemini", nil, nil) {
				streamErr = err
			}
			if !errors.Is(streamErr, context.Canceled) {
				t.Fatalf("stream error = %v, want context.Canceled", streamErr)
			}
			if err := neatlogs.Flush(baseCtx); err != nil {
				t.Fatal(err)
			}
			for _, got := range sink.GetSpans() {
				if got.Name != "google_genai.models.generate_content" {
					continue
				}
				assertBoolAttribute(t, got.Attributes, attrs.StreamCancelled, true)
				if got.Status.Code != codes.Unset {
					t.Fatalf("cancelled stream status = %v, want UNSET", got.Status.Code)
				}
				return
			}
			t.Fatal("cancelled stream span not exported")
		})
	}
}

func TestGenerateContentStreamIsLazyUntilFirstConsumption(t *testing.T) {
	ctx := context.Background()
	sink := tracetest.NewInMemoryExporter()
	shutdown, err := neatlogs.Init(ctx, neatlogs.Config{WorkflowName: "lazy-stream"}, neatlogs.WithExporter(sink))
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(ctx)

	streamCalls := 0
	models := &GenAIModels{
		provider: geminiProvider,
		system:   geminiSystem,
		stream: func(context.Context, string, []*google.Content, *google.GenerateContentConfig) iter.Seq2[*google.GenerateContentResponse, error] {
			streamCalls++
			return func(yield func(*google.GenerateContentResponse, error) bool) {
				yield(&google.GenerateContentResponse{Candidates: []*google.Candidate{{
					Index: 0, Content: &google.Content{Parts: []*google.Part{{Text: "done"}}},
				}}}, nil)
			}
		},
	}

	sequence := models.GenerateContentStream(ctx, "gemini", nil, nil)
	if streamCalls != 0 {
		t.Fatalf("underlying stream started %d times before consumption", streamCalls)
	}
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.GetSpans()); got != 0 {
		t.Fatalf("never-consumed stream exported %d spans", got)
	}

	for _, streamErr := range sequence {
		if streamErr != nil {
			t.Fatal(streamErr)
		}
	}
	if streamCalls != 1 {
		t.Fatalf("underlying stream started %d times after consumption", streamCalls)
	}
	if err := neatlogs.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, span := range sink.GetSpans() {
		if span.Name == "google_genai.models.generate_content" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("consumed stream did not export its LLM span")
	}
}

func findAttribute(values []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, value := range values {
		if string(value.Key) == key {
			return value.Value, true
		}
	}
	return attribute.Value{}, false
}

func assertStringAttribute(t *testing.T, values []attribute.KeyValue, key, want string) {
	t.Helper()
	value, ok := findAttribute(values, key)
	if !ok || value.AsString() != want {
		t.Fatalf("attribute %s = %q, %v; want %q", key, value.AsString(), ok, want)
	}
}

func assertIntAttribute(t *testing.T, values []attribute.KeyValue, key string, want int) {
	t.Helper()
	value, ok := findAttribute(values, key)
	if !ok || value.AsInt64() != int64(want) {
		t.Fatalf("attribute %s = %d, %v; want %d", key, value.AsInt64(), ok, want)
	}
}

func assertBoolAttribute(t *testing.T, values []attribute.KeyValue, key string, want bool) {
	t.Helper()
	value, ok := findAttribute(values, key)
	if !ok || value.AsBool() != want {
		t.Fatalf("attribute %s = %v, %v; want %v", key, value.AsBool(), ok, want)
	}
}
