package main

import (
	"strings"
	"testing"
)

func TestSlackReleaseMessage(t *testing.T) {
	message := slackMessage(
		"success",
		releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", PreviouslyAnalyzed: "v1", Latest: "v2"}}},
		evidenceReport{Modules: []struct {
			Module               string `json:"module"`
			ToolchainRequirement string `json:"toolchainRequirement"`
		}{{Module: "example.com/sdk", ToolchainRequirement: "example.com/sdk requires go >= 1.26.0 (running go 1.25.0)"}}},
		analysisReport{RiskLevel: "high"},
		upstreamIssue{Title: "empty tool arguments disappear", URL: "https://github.com/example/sdk/issues/3"},
		"https://example.test/issues/7",
		"https://example.test/run",
		"",
	)
	for _, expected := range []string{"1 upstream release", "example.com/sdk v1 → v2", "Go ≥1.26.0", "no confirmed regression", "Gemini advisory risk", "high", "empty tool arguments disappear", "github.com/example/sdk/issues/3", "https://example.test/issues/7", "https://example.test/run"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
}

func TestSlackFailureMessage(t *testing.T) {
	message := slackMessage("failure", releaseReport{}, evidenceReport{}, analysisReport{}, upstreamIssue{}, "", "https://example.test/run", "deterministic evidence collection")
	if !strings.Contains(message, "failed during deterministic evidence collection") || !strings.Contains(message, "https://example.test/run") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}
