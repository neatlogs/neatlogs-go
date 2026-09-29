package main

import (
	"strings"
	"testing"
)

func TestSlackReleaseMessage(t *testing.T) {
	message := slackMessage(
		"success",
		releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", PreviouslyAnalyzed: "v1", Latest: "v2"}}},
		analysisReport{RiskLevel: "high"},
		upstreamIssue{Title: "empty tool arguments disappear", URL: "https://github.com/example/sdk/issues/3"},
		"https://example.test/run",
	)
	for _, expected := range []string{"1 upstream release", "example.com/sdk v1 → v2", "high", "empty tool arguments disappear", "github.com/example/sdk/issues/3", "https://example.test/run"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
}

func TestSlackFailureMessage(t *testing.T) {
	if message := slackMessage("failure", releaseReport{}, analysisReport{}, upstreamIssue{}, ""); !strings.Contains(message, "workflow failed") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}
