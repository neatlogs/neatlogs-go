package neatlogs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const datasetTestID = "11111111-1111-4111-8111-111111111111"
const datasetTestVersion = "22222222-2222-4222-8222-222222222222"

func datasetTestClient(t *testing.T, handler http.HandlerFunc) *DatasetClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewDatasetClient(DatasetConfig{Endpoint: server.URL, Token: "test-secret", ProjectID: datasetTestID})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func datasetTestPublication(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"success":true,"data":{"datasetId":"` + datasetTestID + `","versionId":"` + datasetTestVersion + `"},"requestId":"request"}`))
}

func TestDatasetCreateStandaloneEmpty(t *testing.T) {
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/public/datasets" {
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("x-project-id") != datasetTestID {
			t.Error("missing authentication")
		}
		var body CreateDatasetOptions
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "empty" || body.Cases == nil || len(body.Cases) != 0 || body.IdempotencyKey != r.Header.Get("Idempotency-Key") || !datasetUUID.MatchString(body.IdempotencyKey) {
			t.Errorf("unexpected body: %+v", body)
		}
		datasetTestPublication(w)
	})
	publication, err := client.Create(context.Background(), CreateDatasetOptions{Name: " empty "})
	if err != nil || publication.DatasetID != datasetTestID || publication.VersionID != datasetTestVersion {
		t.Fatalf("%+v %v", publication, err)
	}
}

func TestDatasetCreateRetriesSameRequest(t *testing.T) {
	attempts := 0
	var key string
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if key == "" {
			key = r.Header.Get("Idempotency-Key")
		}
		if key != r.Header.Get("Idempotency-Key") {
			t.Error("retry changed idempotency key")
		}
		if attempts == 1 {
			w.WriteHeader(503)
			return
		}
		datasetTestPublication(w)
	})
	if _, err := client.Create(context.Background(), CreateDatasetOptions{Name: "retry"}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestDatasetOutcomeUpdatesPinRevisionsAndClear(t *testing.T) {
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/public/datasets/"+datasetTestID+"/versions" {
			t.Error(r.URL.Path)
		}
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		var changes []map[string]json.RawMessage
		json.Unmarshal(body["expectedChanges"], &changes)
		if string(changes[0]["expected"]) != "null" || string(body["baseVersionId"]) != `"`+datasetTestVersion+`"` {
			t.Errorf("unexpected clear %s", body)
		}
		var outcome DatasetOutcome
		json.Unmarshal(changes[1]["expected"], &outcome)
		if outcome.Value != "done" || outcome.Status != "draft" || outcome.Source != "human" {
			t.Errorf("unexpected outcome %+v", outcome)
		}
		datasetTestPublication(w)
	})
	_, err := client.UpdateOutcomes(context.Background(), datasetTestID, UpdateDatasetOutcomesOptions{BaseVersionID: datasetTestVersion, Changes: []DatasetOutcomeChange{
		{CaseID: datasetTestID, RevisionID: datasetTestVersion},
		{CaseID: datasetTestVersion, RevisionID: datasetTestVersion, Outcome: NewDatasetOutcome("done")},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDatasetRejectsOversizeAndInvalidInputBeforeHTTP(t *testing.T) {
	client := datasetTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached server") })
	for _, options := range []CreateDatasetOptions{
		{}, {Name: "test", IdempotencyKey: "bad"}, {Name: "test", Cases: []DatasetCase{{Input: strings.Repeat("x", datasetRequestLimit)}}},
		{Name: "test", Cases: []DatasetCase{{Input: make(chan int)}}}, {Name: "test", Cases: []DatasetCase{{Outcome: &DatasetOutcome{Value: "bad"}}}},
	} {
		if _, err := client.Create(context.Background(), options); err == nil {
			t.Fatal("expected validation error")
		}
	}
}

func TestDatasetErrorsDoNotExposeResponseBody(t *testing.T) {
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("test-secret private body"))
	})
	_, err := client.Create(context.Background(), CreateDatasetOptions{Name: "denied", IdempotencyKey: datasetTestID})
	var apiErr *DatasetAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || apiErr.IdempotencyKey != datasetTestID || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestDatasetCancellationAndRedirect(t *testing.T) {
	client := datasetTestClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://example.invalid/", 302) })
	if _, err := client.Create(context.Background(), CreateDatasetOptions{Name: "redirect"}); err == nil {
		t.Fatal("redirect accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Create(ctx, CreateDatasetOptions{Name: "cancel"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
