package consumer

import (
	"encoding/json"
	"strings"
	"testing"

	neatlogs "github.com/neatlogs/neatlogs-go"
)

func TestPublicDoctorFromFreshConsumer(t *testing.T) {
	digest, err := neatlogs.DoctorSemanticDigestV2(neatlogs.DoctorEnvelope{
		TraceID: "00000000000000000000000000000001",
		Spans: []neatlogs.DoctorSpan{{
			SpanID: "0000000000000001", Name: "consumer.workflow",
			Kind: "WORKFLOW", Status: "OK", Sampled: true, Ended: true,
		}},
	})
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("unexpected doctor digest %q: %v", digest, err)
	}
	result := neatlogs.DoctorV2Result{FormatVersion: neatlogs.DoctorV2FormatVersion}
	if _, err := json.Marshal(result); err != nil {
		t.Fatalf("doctor result is not JSON serializable: %v", err)
	}
}
