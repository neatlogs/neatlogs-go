package main

import (
	"encoding/json"
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
		verificationReport{Modules: []verificationModule{{Module: "example.com/sdk", Latest: "v2", Status: "blocked"}}},
		analysisReport{RiskLevel: "high", ScopeModule: "example.com/sdk"},
		proposalReport{Status: "skipped", Reason: "Gemini did not produce an actionable SDK fix"},
		upstreamIssue{Title: "empty tool arguments disappear", URL: "https://github.com/example/sdk/issues/3"},
		alertContext{ReviewIssueURL: "https://example.test/issues/7", RunURL: "https://example.test/run"},
	)
	for _, expected := range []string{"Go SDK: release blocked by Go toolchain", "Checked: 1 newer release", "1 blocked", "Regression: none found in tested scope; some releases untested", "Fix PR: none", "Action: example.com/sdk requires Go ≥1.26.0. Decide whether to upgrade SDK CI", "empty tool arguments disappear", "github.com/example/sdk/issues/3", "https://example.test/issues/7", "https://example.test/run"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
}

func verificationWithStatuses(statuses ...string) verificationReport {
	result := verificationReport{}
	for _, status := range statuses {
		result.Modules = append(result.Modules, verificationModule{Module: "example.com/sdk", Latest: "v2", Status: status})
	}
	return result
}

func evidenceWithToolchainBlock() evidenceReport {
	return evidenceReport{Modules: []struct {
		Module               string `json:"module"`
		ToolchainRequirement string `json:"toolchainRequirement"`
	}{{Module: "example.com/sdk", ToolchainRequirement: "requires go >= 1.26.0"}}}
}

func TestSlackSendsOnlyActionableResults(t *testing.T) {
	releases := releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2"}}}
	for _, tc := range []struct {
		name         string
		status       string
		evidence     evidenceReport
		verification verificationReport
		analysis     analysisReport
		proposal     proposalReport
		alert        alertContext
		want         bool
	}{
		{name: "routine pass", status: "success", verification: verificationWithStatuses("pass"), proposal: proposalReport{Status: "skipped"}},
		{name: "unverified rejected suggestion", status: "success", verification: verificationWithStatuses("pass"), analysis: analysisReport{RiskLevel: "high"}, proposal: proposalReport{Status: "rejected", Reason: "cosmetic patch"}},
		{name: "blocked release with rejected suggestion", status: "success", verification: verificationWithStatuses("blocked", "pass", "pass"), proposal: proposalReport{Status: "rejected"}},
		{name: "toolchain blocker", status: "success", evidence: evidenceWithToolchainBlock(), verification: verificationWithStatuses("blocked", "pass", "pass"), proposal: proposalReport{Status: "rejected"}},
		{name: "changed toolchain blocker", status: "success", verification: verificationWithStatuses("blocked", "pass", "pass")},
		{name: "new PR despite blocker", status: "success", evidence: evidenceWithToolchainBlock(), verification: verificationWithStatuses("blocked", "pass", "pass"), alert: alertContext{FixPRURL: "https://example.test/pr/8"}, want: true},
		{name: "workflow failure despite blocker", status: "failure", evidence: evidenceWithToolchainBlock(), verification: verificationWithStatuses("blocked", "pass", "pass"), want: true},
		{name: "automation failure despite blocker", status: "success", evidence: evidenceWithToolchainBlock(), verification: verificationWithStatuses("blocked", "pass", "pass"), alert: alertContext{FixIssueOutcome: "failure"}, want: true},
		{name: "non-toolchain block is recorded without Slack", status: "success", verification: verificationWithStatuses("blocked")},
		{name: "regression cannot be suppressed", status: "success", evidence: evidenceWithToolchainBlock(), verification: verificationWithStatuses("fail"), want: true},
		{name: "baseline-pass latest-fail", status: "success", verification: verificationWithStatuses("fail"), want: true},
		{name: "not tested", status: "success", verification: verificationWithStatuses("not_tested"), want: true},
		{name: "missing verification", status: "success", want: true},
		{name: "generated PR", status: "success", verification: verificationWithStatuses("pass"), alert: alertContext{FixPRURL: "https://example.test/pr/8"}, want: true},
		{name: "PR creation failed", status: "success", verification: verificationWithStatuses("pass"), alert: alertContext{FixPROutcome: "failure"}, want: true},
		{name: "patch validation tooling failed", status: "success", verification: verificationWithStatuses("pass"), alert: alertContext{ProposalOutcome: "failure"}, want: true},
		{name: "Gemini request failed", status: "success", verification: verificationWithStatuses("pass"), analysis: analysisReport{Skipped: true, Reason: "Gemini request failed; inspect the workflow run"}, want: true},
		{name: "Gemini step failed", status: "success", verification: verificationWithStatuses("pass"), alert: alertContext{GeminiOutcome: "failure"}, want: true},
		{name: "covered release", status: "success", verification: verificationWithStatuses("pass"), analysis: analysisReport{Skipped: true, Reason: "No uncovered release is testable with this runner"}},
		{name: "monitor failed", status: "failure", verification: verificationWithStatuses("pass"), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSendSlack(tc.status, releases, tc.evidence, tc.verification, tc.analysis, tc.proposal, tc.alert); got != tc.want {
				t.Fatalf("shouldSendSlack() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSlackPreexistingFailuresDoNotAlert(t *testing.T) {
	releases := releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2"}}}
	for _, tc := range []struct {
		name string
		json string
		want bool
	}{
		{"baseline and latest fail", `{"modules":[{"module":"example.com/sdk","latest":"v2","status":"not_tested","suites":[{"status":"not_tested","baseline":{"status":"fail"},"latest":{"status":"fail"}}]}]}`, false},
		{"preexisting failure with a passing suite", `{"modules":[{"module":"example.com/sdk","latest":"v2","status":"not_tested","suites":[{"status":"pass","baseline":{"status":"pass"},"latest":{"status":"pass"}},{"status":"not_tested","baseline":{"status":"fail"},"latest":{"status":"fail"}}]}]}`, false},
		{"baseline missing", `{"modules":[{"module":"example.com/sdk","latest":"v2","status":"not_tested","suites":[{"status":"not_tested","baseline":{"status":"not_tested"},"latest":{"status":"fail"}}]}]}`, true},
		{"blocked module with an unexplained suite", `{"modules":[{"module":"example.com/sdk","latest":"v2","status":"blocked","suites":[{"status":"blocked","baseline":{"status":"pass"},"latest":{"status":"blocked"}},{"status":"not_tested","baseline":{"status":"not_tested"},"latest":{"status":"fail"}}]}]}`, true},
		{"blocked module with preexisting failure", `{"modules":[{"module":"example.com/sdk","latest":"v2","status":"blocked","suites":[{"status":"blocked","baseline":{"status":"pass"},"latest":{"status":"blocked"}},{"status":"not_tested","baseline":{"status":"fail"},"latest":{"status":"fail"}}]}]}`, false},
		{"no mapped suite", `{"modules":[{"module":"example.com/sdk","latest":"v2","status":"not_tested","suites":[]}]}`, true},
		{"unknown suite result", `{"modules":[{"module":"example.com/sdk","latest":"v2","status":"not_tested","suites":[{"status":"unknown","baseline":{"status":"fail"},"latest":{"status":"fail"}}]}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var verification verificationReport
			if err := json.Unmarshal([]byte(tc.json), &verification); err != nil {
				t.Fatal(err)
			}
			got := shouldSendSlack("success", releases, evidenceReport{}, verification, analysisReport{}, proposalReport{Status: "rejected"}, alertContext{})
			if got != tc.want {
				t.Fatalf("shouldSendSlack() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMissingReleaseVerificationAlerts(t *testing.T) {
	releases := releaseReport{Changes: []releaseChange{
		{Module: "example.com/sdk", Latest: "v2"},
		{Module: "example.com/other", Latest: "v3"},
	}}
	verification := verificationWithStatuses("pass")
	if !shouldSendSlack("success", releases, evidenceReport{}, verification, analysisReport{}, proposalReport{}, alertContext{}) {
		t.Fatal("an omitted release result could hide a regression")
	}
}

func TestSlackNamesGeminiStepFailure(t *testing.T) {
	message := slackMessage("success", releaseReport{}, evidenceReport{}, verificationWithStatuses("pass"), analysisReport{}, proposalReport{}, upstreamIssue{}, alertContext{GeminiOutcome: "failure"})
	if !strings.Contains(message, "Go SDK: Gemini analysis failed") || !strings.Contains(message, "Regression: none found in tested scope") {
		t.Fatalf("Gemini failure missing from Slack message: %q", message)
	}
}

func TestSlackOct2GoResultRecordedWithoutSlack(t *testing.T) {
	var releases releaseReport
	var evidence evidenceReport
	var verification verificationReport
	for _, item := range []struct {
		text  string
		value any
	}{
		{`{"changes":[{"module":"github.com/a2aproject/a2a-go/v2","previouslyAnalyzed":"v2.3.1","latest":"v2.6.0"},{"module":"google.golang.org/adk","previouslyAnalyzed":"v1.4.0","latest":"v1.7.0"},{"module":"google.golang.org/genai","previouslyAnalyzed":"v1.61.0","latest":"v1.72.0"}]}`, &releases},
		{`{"modules":[{"module":"github.com/a2aproject/a2a-go/v2","toolchainRequirement":"github.com/a2aproject/a2a-go/v2@v2.6.0 requires go >= 1.26.0 (running go 1.25.0)"}]}`, &evidence},
		{`{"modules":[{"module":"github.com/a2aproject/a2a-go/v2","latest":"v2.6.0","status":"blocked"},{"module":"google.golang.org/adk","latest":"v1.7.0","status":"pass"},{"module":"google.golang.org/genai","latest":"v1.72.0","status":"pass"}]}`, &verification},
	} {
		if err := json.Unmarshal([]byte(item.text), item.value); err != nil {
			t.Fatal(err)
		}
	}
	analysis := analysisReport{RiskLevel: "high", ScopeModule: "google.golang.org/adk"}
	proposal := proposalReport{Status: "rejected", Reason: "proposal must change one or two allowlisted SDK source files"}
	alert := alertContext{ReviewIssueURL: "https://github.com/neatlogs/neatlogs-go/issues/29", RunURL: "https://github.com/neatlogs/neatlogs-go/actions/runs/36974183035"}
	if shouldSendSlack("success", releases, evidence, verification, analysis, proposal, alert) {
		t.Fatal("toolchain block without a candidate regression should remain in the issue and artifact")
	}
	message := slackMessage("success", releases, evidence, verification, analysis, proposal, upstreamIssue{}, alert)
	for _, expected := range []string{"Go SDK: release blocked by Go toolchain", "2 passed, 0 failed, 1 blocked", "Regression: none found in tested scope; some releases untested", "a2a-go/v2 requires Go ≥1.26.0", "proposed fix rejected by validation", "issues/29", "actions/runs/36974183035"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
}

func TestSlackOct3GoAlertIsScannable(t *testing.T) {
	releases := releaseReport{Changes: []releaseChange{
		{Module: "github.com/a2aproject/a2a-go/v2", PreviouslyAnalyzed: "v2.3.1", Latest: "v2.6.0"},
		{Module: "google.golang.org/adk", PreviouslyAnalyzed: "v1.4.0", Latest: "v1.7.0"},
		{Module: "google.golang.org/genai", PreviouslyAnalyzed: "v1.61.0", Latest: "v1.72.0"},
	}}
	evidence := evidenceReport{Modules: []struct {
		Module               string `json:"module"`
		ToolchainRequirement string `json:"toolchainRequirement"`
	}{{Module: "github.com/a2aproject/a2a-go/v2", ToolchainRequirement: "github.com/a2aproject/a2a-go/v2@v2.6.0 requires go >= 1.26.0 (running go 1.25.0)"}}}
	verification := verificationReport{Modules: []verificationModule{
		{Module: "github.com/a2aproject/a2a-go/v2", Latest: "v2.6.0", Status: "blocked"},
		{Module: "google.golang.org/adk", Latest: "v1.7.0", Status: "pass"},
		{Module: "google.golang.org/genai", Latest: "v1.72.0", Status: "pass"},
	}}
	alert := alertContext{ReviewIssueURL: "https://github.com/neatlogs/neatlogs-go/issues/29", RunURL: "https://github.com/neatlogs/neatlogs-go/actions/runs/37085365700"}
	got := slackMessage("success", releases, evidence, verification, analysisReport{RiskLevel: "low", ScopeModule: "google.golang.org/genai"}, proposalReport{Status: "skipped", Reason: "Gemini did not produce an actionable SDK fix"}, upstreamIssue{}, alert)
	want := ":warning: *Go SDK: release blocked by Go toolchain — review Go version support.*\n" +
		"Checked: 3 newer releases; mapped adapter checks: 2 passed, 0 failed, 1 blocked, 0 not tested\n" +
		"Regression: none found in tested scope; some releases untested\n" +
		"Fix PR: none — no actionable SDK fix\n" +
		"Action: github.com/a2aproject/a2a-go/v2 requires Go ≥1.26.0. Decide whether to upgrade SDK CI to test it.\n" +
		"Details: <https://github.com/neatlogs/neatlogs-go/issues/29|Review issue> · <https://github.com/neatlogs/neatlogs-go/actions/runs/37085365700|Workflow evidence>"
	if got != want {
		t.Fatalf("Oct 3 alert:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "Gemini advisory risk") || strings.Contains(got, "*low*") {
		t.Fatalf("unverified advisory should not distract from the toolchain blocker: %q", got)
	}
}

func TestActionableSlackPayloadUsesSeparateSections(t *testing.T) {
	message := slackMessage("success",
		releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2"}}},
		evidenceReport{}, verificationWithStatuses("fail"), analysisReport{}, proposalReport{}, upstreamIssue{},
		alertContext{ReviewIssueURL: "https://example.test/issues/7", RunURL: "https://example.test/runs/8"},
	)
	payload := buildSlackPayload(message)
	if payload.Text != message {
		t.Fatal("Block Kit payload must retain the complete plain-text fallback")
	}
	if len(payload.Blocks) != 4 {
		t.Fatalf("got %d blocks, want headline, verdict, action and links", len(payload.Blocks))
	}
	if payload.Blocks[0].Type != "section" || !strings.Contains(payload.Blocks[0].Text.Text, "candidate regression") {
		t.Fatalf("missing headline block: %#v", payload.Blocks[0])
	}
	for _, expected := range []string{"*Checked:*", "*Regression:* 1 candidate", "*Fix PR:*"} {
		if !strings.Contains(payload.Blocks[1].Text.Text, expected) {
			t.Fatalf("verdict block %q lacks %q", payload.Blocks[1].Text.Text, expected)
		}
	}
	if payload.Blocks[2].Type != "section" || !strings.HasPrefix(payload.Blocks[2].Text.Text, "*Action:*") {
		t.Fatalf("missing action block: %#v", payload.Blocks[2])
	}
	if payload.Blocks[3].Type != "context" || !strings.Contains(payload.Blocks[3].Elements[0].Text, "https://example.test/runs/8") {
		t.Fatalf("missing links block: %#v", payload.Blocks[3])
	}
	encoded, err := json.Marshal(payload)
	if err != nil || !strings.Contains(string(encoded), `"blocks"`) {
		t.Fatalf("invalid webhook JSON: %v, %s", err, encoded)
	}
}

func TestSlackFailureMessage(t *testing.T) {
	message := slackMessage("failure", releaseReport{}, evidenceReport{}, verificationReport{}, analysisReport{}, proposalReport{}, upstreamIssue{}, alertContext{RunURL: "https://example.test/run", FailureStage: "deterministic evidence collection"})
	if !strings.Contains(message, "Go SDK: compatibility monitor failed") || !strings.Contains(message, "Investigate failure during deterministic evidence collection") || !strings.Contains(message, "https://example.test/run") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}

func TestSlackRetainsTestVerdictWhenLaterAutomationFails(t *testing.T) {
	verification := verificationReport{Modules: []verificationModule{{Module: "example.com/sdk", Latest: "v2", Status: "fail"}}}
	message := slackMessage("failure", releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2"}}}, evidenceReport{}, verification, analysisReport{}, proposalReport{}, upstreamIssue{}, alertContext{RunURL: "https://example.test/run", FailureStage: "review issue update"})
	if !strings.Contains(message, "candidate regression in mapped adapter tests") || !strings.Contains(message, "Monitor also failed during review issue update") || strings.Contains(message, "Regression: no verdict") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}

func TestSlackReportsDeterministicFailureSeparatelyFromGemini(t *testing.T) {
	verification := verificationReport{Modules: []verificationModule{{Module: "example.com/sdk", Latest: "v2", Status: "fail"}}}
	message := slackMessage("success", releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2"}}}, evidenceReport{}, verification, analysisReport{RiskLevel: "low", ScopeModule: "example.com/sdk"}, proposalReport{}, upstreamIssue{}, alertContext{})
	if !strings.Contains(message, "candidate regression in mapped adapter tests") || !strings.Contains(message, "Regression: 1 candidate (baseline passed, latest failed)") || strings.Contains(message, "*low*") {
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
		{"reproduced fix PR", proposalReport{Status: "validated", Validation: "test_regression_resolved"}, alertContext{FixPRURL: "https://example.test/pr/8"}, []string{"fix PR opened", "mapped regression passed after the patch"}},
		{"patch rejected", proposalReport{Status: "rejected", Reason: "adapter tests failed"}, alertContext{}, []string{"proposed fix rejected by validation", "adapter tests failed", "Fix PR: none"}},
		{"publication check failed", proposalReport{Status: "validated"}, alertContext{PublishCheckOutcome: "failure"}, []string{"publication check failed", "Fix PR: none"}},
		{"PR creation failed", proposalReport{Status: "validated"}, alertContext{FixPROutcome: "failure"}, []string{"PR creation failed", "Fix PR: none"}},
		{"patch job failed", proposalReport{}, alertContext{PatchJobStatus: "failure"}, []string{"patch validation failed", "Fix PR: none"}},
		{"proposal tooling failed", proposalReport{}, alertContext{ProposalOutcome: "failure"}, []string{"proposal tooling failed", "Fix PR: none"}},
		{"retest tooling failed", proposalReport{}, alertContext{RetestOutcome: "failure"}, []string{"patch retest failed", "Fix PR: none"}},
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
