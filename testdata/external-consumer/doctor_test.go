package consumer

import (
	"context"
	"encoding/json"
	"testing"

	neatlogs "github.com/neatlogs/neatlogs-go"
)

func TestPublicDoctorFromFreshConsumer(t *testing.T) {
	result := neatlogs.Doctor(context.Background(), neatlogs.Config{
		DisableExport: true,
		Endpoint:      "https://ingest.neatlogs.com",
	})
	if result.FormatVersion != neatlogs.DoctorFormatVersion || !result.Ready {
		t.Fatalf("unexpected doctor result: %#v", result)
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatalf("doctor result is not JSON serializable: %v", err)
	}
}
