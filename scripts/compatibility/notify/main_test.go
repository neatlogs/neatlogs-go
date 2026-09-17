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
		"https://example.test/run",
	)
	for _, expected := range []string{"1 upstream release", "example.com/sdk v1 → v2", "high", "https://example.test/run"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
}

func TestSlackFailureMessage(t *testing.T) {
	if message := slackMessage("failure", releaseReport{}, analysisReport{}, ""); !strings.Contains(message, "workflow failed") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}
