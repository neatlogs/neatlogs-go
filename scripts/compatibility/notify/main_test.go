package main

import (
	"strings"
	"testing"
)

func TestWorkflowURLUsesCurrentActionsRun(t *testing.T) {
	t.Setenv("GITHUB_SERVER_URL", "https://github.com")
	t.Setenv("GITHUB_REPOSITORY", "neatlogs/neatlogs-go")
	t.Setenv("GITHUB_RUN_ID", "12345")
	if got := workflowURL(); got != "https://github.com/neatlogs/neatlogs-go/actions/runs/12345" {
		t.Fatalf("workflowURL() = %q", got)
	}
}

func TestSlackReleaseMessage(t *testing.T) {
	message := slackMessage(
		"success",
		releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", PreviouslyAnalyzed: "v1", Latest: "v2"}}},
		evidenceReport{Modules: []struct {
			Module               string `json:"module"`
			ToolchainRequirement string `json:"toolchainRequirement"`
		}{{Module: "example.com/sdk", ToolchainRequirement: "example.com/sdk requires go >= 1.26.0 (running go 1.25.0)"}}},
		verificationReport{Modules: []struct {
			Module string `json:"module"`
			Latest string `json:"latest"`
			Status string `json:"status"`
		}{{Module: "example.com/sdk", Latest: "v2", Status: "blocked"}}},
		analysisReport{RiskLevel: "high", ScopeModule: "example.com/sdk"},
		proposalReport{Status: "skipped", Reason: "Gemini did not produce an actionable SDK fix"},
		upstreamIssue{Title: "empty tool arguments disappear", URL: "https://github.com/example/sdk/issues/3"},
		alertContext{ReviewIssueURL: "https://example.test/issues/7", RunURL: "https://example.test/run"},
	)
	for _, expected := range []string{"1 upstream release", "example.com/sdk v1 → v2", "Go ≥1.26.0", "no confirmed regression", "blocked", "Gemini advisory risk", "high", "No SDK fix proposal", "empty tool arguments disappear", "github.com/example/sdk/issues/3", "https://example.test/issues/7", "https://example.test/run"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
}

func TestSlackFailureMessage(t *testing.T) {
	message := slackMessage("failure", releaseReport{}, evidenceReport{}, verificationReport{}, analysisReport{}, proposalReport{}, upstreamIssue{}, alertContext{RunURL: "https://example.test/run", FailureStage: "deterministic evidence collection"})
	if !strings.Contains(message, "failed during deterministic evidence collection") || !strings.Contains(message, "https://example.test/run") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}

func TestSlackRetainsTestVerdictWhenLaterAutomationFails(t *testing.T) {
	verification := verificationReport{Modules: []struct {
		Module string `json:"module"`
		Latest string `json:"latest"`
		Status string `json:"status"`
	}{{Module: "example.com/sdk", Latest: "v2", Status: "fail"}}}
	message := slackMessage("failure", releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2"}}}, evidenceReport{}, verification, analysisReport{}, proposalReport{}, upstreamIssue{}, alertContext{RunURL: "https://example.test/run", FailureStage: "review issue update"})
	if !strings.Contains(message, "regression in mapped adapter tests") || !strings.Contains(message, "Monitor also failed during review issue update") || strings.Contains(message, "No regression verdict is available") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}

func TestSlackReportsDeterministicFailureSeparatelyFromGemini(t *testing.T) {
	verification := verificationReport{Modules: []struct {
		Module string `json:"module"`
		Latest string `json:"latest"`
		Status string `json:"status"`
	}{{Module: "example.com/sdk", Latest: "v2", Status: "fail"}}}
	message := slackMessage("success", releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2"}}}, evidenceReport{}, verification, analysisReport{RiskLevel: "low", ScopeModule: "example.com/sdk"}, proposalReport{}, upstreamIssue{}, alertContext{})
	if !strings.Contains(message, "regression in mapped adapter tests") || !strings.Contains(message, "Gemini advisory risk for example.com/sdk: *low* (unverified)") {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestSlackReportsFixPublicationOutcomes(t *testing.T) {
	releases := releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", PreviouslyAnalyzed: "v1", Latest: "v2"}}}
	for _, tc := range []struct {
		name     string
		proposal proposalReport
		alert    alertContext
		want     []string
	}{
		{"unverified fix PR", proposalReport{Status: "validated", Validation: "advisory_only_no_reproduction"}, alertContext{FixPRURL: "https://example.test/pr/8", RunURL: "https://example.test/run", ReviewIssueURL: "https://example.test/issues/7"}, []string{"PR opened for human code review", "suspected behavior unverified", "no automated approval or merge", "https://example.test/pr/8", "https://example.test/issues/7", "https://example.test/run"}},
		{"reproduced fix PR", proposalReport{Status: "validated", Validation: "test_regression_resolved"}, alertContext{FixPRURL: "https://example.test/pr/8"}, []string{"PR opened for human code review", "mapped regression passed after the patch"}},
		{"patch rejected", proposalReport{Status: "rejected", Reason: "adapter tests failed"}, alertContext{}, []string{"fix rejected by validation", "adapter tests failed", "no PR opened"}},
		{"publication check failed", proposalReport{Status: "validated"}, alertContext{PublishCheckOutcome: "failure"}, []string{"publication check failed", "no PR opened"}},
		{"PR creation failed", proposalReport{Status: "validated"}, alertContext{FixPROutcome: "failure"}, []string{"PR creation failed", "no PR opened"}},
		{"patch job failed", proposalReport{}, alertContext{PatchJobStatus: "failure"}, []string{"patch validation failed", "no PR opened"}},
		{"proposal tooling failed", proposalReport{}, alertContext{ProposalOutcome: "failure"}, []string{"proposal validation tooling failed", "no PR opened"}},
		{"retest tooling failed", proposalReport{}, alertContext{RetestOutcome: "failure"}, []string{"patch retest tooling failed", "no PR opened"}},
		{"issue update failed", proposalReport{}, alertContext{IssueOutcome: "failure"}, []string{"Review issue update failed"}},
		{"fix issue update failed", proposalReport{}, alertContext{FixIssueOutcome: "failure"}, []string{"Fix status update to the review issue failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := slackMessage("success", releases, evidenceReport{}, verificationReport{}, analysisReport{}, tc.proposal, upstreamIssue{}, tc.alert)
			for _, expected := range tc.want {
				if !strings.Contains(message, expected) {
					t.Fatalf("message %q does not contain %q", message, expected)
				}
			}
		})
	}
}
