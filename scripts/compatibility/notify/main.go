package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type releaseChange struct {
	Module             string `json:"module"`
	PreviouslyAnalyzed string `json:"previouslyAnalyzed"`
	Latest             string `json:"latest"`
}

type releaseReport struct {
	Changes []releaseChange `json:"changes"`
}

type analysisReport struct {
	RiskLevel   string `json:"riskLevel"`
	Skipped     bool   `json:"skipped"`
	Reason      string `json:"reason"`
	ScopeModule string `json:"scopeModule"`
}

type proposalReport struct {
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Validation string `json:"validation"`
}

type evidenceReport struct {
	Modules []struct {
		Module               string `json:"module"`
		ToolchainRequirement string `json:"toolchainRequirement"`
	} `json:"modules"`
}

type verificationReport struct {
	Modules []struct {
		Module string `json:"module"`
		Latest string `json:"latest"`
		Status string `json:"status"`
	} `json:"modules"`
}

type upstreamIssue struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type alertContext struct {
	ReviewIssueURL          string
	UnchangedToolchainBlock bool
	FixPRURL                string
	FixPROutcome            string
	PublishCheckOutcome     string
	PatchJobStatus          string
	ProposalOutcome         string
	RetestOutcome           string
	GeminiOutcome           string
	IssueOutcome            string
	FixIssueOutcome         string
	RunURL                  string
	FailureStage            string
}

func optionalJSON(path string, value any) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, value)
}

func workflowURL() string {
	server := os.Getenv("GITHUB_SERVER_URL")
	repository := os.Getenv("GITHUB_REPOSITORY")
	runID := os.Getenv("GITHUB_RUN_ID")
	if server == "" || repository == "" || runID == "" {
		return ""
	}
	return server + "/" + repository + "/actions/runs/" + runID
}

var requiredGoPattern = regexp.MustCompile(`requires go >= ([0-9]+(?:\.[0-9]+){1,2})`)

func failedStage() string {
	for _, step := range []struct{ env, label string }{
		{"COMPAT_DISCOVER_OUTCOME", "release discovery"},
		{"COMPAT_EVIDENCE_OUTCOME", "deterministic evidence collection"},
		{"COMPAT_VERIFY_OUTCOME", "adapter version tests"},
		{"COMPAT_GEMINI_OUTCOME", "Gemini advisory analysis"},
		{"COMPAT_ISSUE_OUTCOME", "review issue update"},
	} {
		if os.Getenv(step.env) == "failure" {
			return step.label
		}
	}
	if os.Getenv("COMPAT_PATCH_JOB_STATUS") == "failure" {
		return "isolated SDK patch validation"
	}
	return "an unknown step"
}

func conciseSlackText(value string) string {
	value = strings.NewReplacer("<", "", ">", "", "\n", " ", "\r", " ").Replace(value)
	characters := []rune(value)
	if len(characters) > 240 {
		return string(characters[:240]) + "…"
	}
	return value
}

// A newly published version is recorded in the issue and artifact. Slack is
// reserved for an outcome that requires a person to act or investigate.
func shouldSendSlack(status string, report releaseReport, evidence evidenceReport, verification verificationReport, analysis analysisReport, proposal proposalReport, alert alertContext) bool {
	if status != "success" || alert.IssueOutcome == "failure" || alert.FixIssueOutcome == "failure" || alert.GeminiOutcome == "failure" ||
		alert.PatchJobStatus == "failure" || alert.ProposalOutcome == "failure" || alert.RetestOutcome == "failure" ||
		alert.PublishCheckOutcome == "failure" || alert.FixPROutcome == "failure" {
		return true
	}
	if len(report.Changes) == 0 {
		return false
	}
	if len(verification.Modules) == 0 {
		return true
	}
	if alert.FixPRURL != "" || proposal.Status == "validated" {
		return true
	}
	// The review issue retains unchanged toolchain-only findings. A repeated
	// schedule should not page Slack again for the same blocked release.
	if alert.UnchangedToolchainBlock {
		toolchainBlocked := make(map[string]bool)
		for _, module := range evidence.Modules {
			toolchainBlocked[module.Module] = module.ToolchainRequirement != ""
		}
		unchangedToolchainOnly := false
		for _, module := range verification.Modules {
			if module.Status == "blocked" && toolchainBlocked[module.Module] {
				unchangedToolchainOnly = true
				continue
			}
			if module.Status != "pass" {
				unchangedToolchainOnly = false
				break
			}
		}
		if unchangedToolchainOnly {
			return false
		}
	}
	for _, module := range verification.Modules {
		if module.Status != "pass" {
			return true
		}
	}
	return analysis.Skipped && analysis.Reason != "No uncovered release is testable with this runner"
}

func slackMessage(status string, report releaseReport, evidence evidenceReport, verification verificationReport, analysis analysisReport, proposal proposalReport, issue upstreamIssue, alert alertContext) string {
	link := ""
	if alert.RunURL != "" {
		link = fmt.Sprintf(" <%s|Evidence and workflow run>.", alert.RunURL)
	}
	if status != "success" && len(verification.Modules) == 0 {
		reviewLink := ""
		if alert.ReviewIssueURL != "" {
			reviewLink = fmt.Sprintf(" <%s|Review issue>.", alert.ReviewIssueURL)
		}
		fixLink := ""
		if alert.FixPRURL != "" {
			fixLink = fmt.Sprintf(" <%s|Fix PR>.", alert.FixPRURL)
		}
		return fmt.Sprintf(":red_circle: *Go SDK compatibility monitor failed during %s.* No regression verdict is available; inspect the workflow logs for the error.%s%s%s", alert.FailureStage, reviewLink, fixLink, link)
	}
	items := make([]string, 0)
	limit := len(report.Changes)
	if limit > 8 {
		limit = 8
	}
	for _, item := range report.Changes[:limit] {
		previous := item.PreviouslyAnalyzed
		if previous == "" {
			previous = "untracked"
		}
		items = append(items, fmt.Sprintf("%s %s → %s", item.Module, previous, item.Latest))
	}
	remaining := ""
	if len(report.Changes) > limit {
		remaining = fmt.Sprintf(", +%d more", len(report.Changes)-limit)
	}
	risk := ""
	if alert.GeminiOutcome == "failure" {
		risk = " Gemini advisory analysis failed; inspect the run."
	} else if analysis.RiskLevel != "" {
		scope := ""
		if analysis.ScopeModule != "" {
			scope = " for " + conciseSlackText(analysis.ScopeModule)
		}
		risk = fmt.Sprintf(" Gemini advisory risk%s: *%s* (unverified).", scope, conciseSlackText(analysis.RiskLevel))
	} else if analysis.Skipped {
		risk = " Gemini advisory unavailable; deterministic evidence and tests remain available."
	}
	toolchain := ""
	for _, item := range evidence.Modules {
		if match := requiredGoPattern.FindStringSubmatch(item.ToolchainRequirement); len(match) == 2 {
			toolchain += fmt.Sprintf(" %s requires Go ≥%s; the current SDK CI toolchain cannot use it.", item.Module, match[1])
		}
	}
	verificationItems := make([]string, 0, len(verification.Modules))
	confirmedFailure := false
	blocked := false
	notTested := false
	for index, item := range verification.Modules {
		if index < 8 {
			verificationItems = append(verificationItems, fmt.Sprintf("%s %s: *%s*", item.Module, item.Latest, item.Status))
		}
		if item.Status == "fail" {
			confirmedFailure = true
		} else if item.Status == "blocked" {
			blocked = true
		} else if item.Status != "pass" {
			notTested = true
		}
	}
	verificationText := " Mapped adapter tests unavailable; inspect the run."
	if len(verificationItems) > 0 {
		verificationText = " Mapped adapter tests (recorded baseline vs latest; tested scope only): " + strings.Join(verificationItems, "; ")
		if len(verification.Modules) > len(verificationItems) {
			verificationText += fmt.Sprintf("; +%d more", len(verification.Modules)-len(verificationItems))
		}
		verificationText += "."
	}
	heading := ":warning: *Go SDK compatibility automation needs attention:*"
	switch {
	case confirmedFailure:
		heading = ":rotating_light: *Go SDK regression in mapped adapter tests (baseline passed, latest failed):*"
	case blocked && toolchain != "":
		heading = ":warning: *Go SDK verification incomplete: toolchain blocked; no regression established:*"
	case blocked || notTested || len(verification.Modules) == 0:
		heading = ":warning: *Go SDK verification incomplete; no regression established:*"
	case alert.FixPRURL != "":
		heading = ":large_green_circle: *Go SDK fix PR opened for human review:*"
	}
	issueText := ""
	if issue.URL != "" {
		title := issue.Title
		if title == "" {
			title = issue.URL
		}
		issueText = fmt.Sprintf(" Referenced upstream issue: <%s|%s>.", issue.URL, title)
	}
	reviewIssue := ""
	if alert.ReviewIssueURL != "" {
		reviewIssue = fmt.Sprintf(" <%s|Review issue>.", alert.ReviewIssueURL)
	}
	fix := " No validated SDK fix proposal; no PR opened."
	switch {
	case alert.FixPRURL != "":
		validation := "review validation evidence in the PR"
		if proposal.Validation == "test_regression_resolved" {
			validation = "mapped regression passed after the patch"
		} else if proposal.Validation == "advisory_only_no_reproduction" {
			validation = "suspected behavior unverified by mapped tests"
		}
		fix = fmt.Sprintf(" Gemini-proposed fix: <%s|PR opened for human code review> (%s); no automated approval or merge.", alert.FixPRURL, validation)
	case proposal.Status == "validated" && alert.PublishCheckOutcome == "failure":
		fix = " SDK patch passed isolated tests, but the publication check failed; no PR opened. Inspect the run."
	case proposal.Status == "validated" && alert.FixPROutcome == "failure":
		fix = " SDK patch passed validation, but PR creation failed; no PR opened. Inspect Actions permissions and the run."
	case proposal.Status == "validated":
		fix = " SDK patch passed validation, but no PR was opened; inspect the run."
	case alert.PatchJobStatus == "failure":
		fix = " Isolated SDK patch validation failed; no PR opened. Inspect the run."
	case alert.ProposalOutcome == "failure":
		fix = " SDK proposal validation tooling failed; no PR opened. Inspect the run."
	case alert.RetestOutcome == "failure":
		fix = " SDK patch retest tooling failed; no PR opened. Inspect the run."
	case proposal.Status == "rejected":
		fix = fmt.Sprintf(" Gemini fix rejected by validation; no PR opened: %s.", conciseSlackText(proposal.Reason))
	case proposal.Status == "skipped" && proposal.Reason != "":
		fix = fmt.Sprintf(" No SDK fix proposal: %s.", conciseSlackText(proposal.Reason))
	}
	issueFailure := ""
	if alert.IssueOutcome == "failure" {
		issueFailure = " Review issue update failed; inspect the run."
	} else if alert.FixIssueOutcome == "failure" {
		issueFailure = " Fix status update to the review issue failed; inspect the run."
	}
	monitorFailure := ""
	if status != "success" {
		monitorFailure = fmt.Sprintf(" Monitor also failed during %s; inspect the run.", alert.FailureStage)
	}
	return fmt.Sprintf("%s %d upstream release(s) newer than the analyzed lock. %s%s.%s%s%s%s%s%s%s%s%s", heading, len(report.Changes), strings.Join(items, ", "), remaining, verificationText, toolchain, risk, fix, issueText, reviewIssue, issueFailure, monitorFailure, link)
}

func main() {
	webhook := os.Getenv("COMPAT_SLACK_WEBHOOK_URL")
	if webhook == "" {
		fmt.Println("Slack notification skipped: COMPAT_SLACK_WEBHOOK_URL is not configured")
		return
	}
	status := os.Getenv("COMPAT_JOB_STATUS")
	if os.Getenv("COMPAT_VERIFY_OUTCOME") == "failure" {
		status = "failure"
	}
	if status == "success" && os.Getenv("COMPAT_CHANGES_FOUND") != "true" {
		return
	}
	var report releaseReport
	var evidence evidenceReport
	var verification verificationReport
	var analysis analysisReport
	var proposal proposalReport
	var issue upstreamIssue
	optionalJSON("compatibility-release-report.json", &report)
	optionalJSON("compatibility-evidence.json", &evidence)
	optionalJSON("compatibility-verification.json", &verification)
	optionalJSON("compatibility-llm-analysis.json", &analysis)
	optionalJSON("compatibility-proposal.json", &proposal)
	issuePath := os.Getenv("COMPAT_UPSTREAM_ISSUE_FILE")
	if issuePath == "" {
		issuePath = "compatibility-upstream-issue.json"
	}
	optionalJSON(issuePath, &issue)
	alert := alertContext{
		ReviewIssueURL: os.Getenv("COMPAT_REVIEW_ISSUE_URL"), UnchangedToolchainBlock: os.Getenv("COMPAT_UNCHANGED_TOOLCHAIN_BLOCK") == "true", FixPRURL: os.Getenv("COMPAT_FIX_PR_URL"),
		FixPROutcome: os.Getenv("COMPAT_FIX_PR_OUTCOME"), PublishCheckOutcome: os.Getenv("COMPAT_PUBLISH_CHECK_OUTCOME"),
		PatchJobStatus: os.Getenv("COMPAT_PATCH_JOB_STATUS"), IssueOutcome: os.Getenv("COMPAT_ISSUE_OUTCOME"),
		ProposalOutcome: os.Getenv("COMPAT_PROPOSAL_OUTCOME"), RetestOutcome: os.Getenv("COMPAT_RETEST_OUTCOME"),
		GeminiOutcome:   os.Getenv("COMPAT_GEMINI_OUTCOME"),
		FixIssueOutcome: os.Getenv("COMPAT_FIX_ISSUE_OUTCOME"),
		RunURL:          workflowURL(), FailureStage: failedStage(),
	}
	if !shouldSendSlack(status, report, evidence, verification, analysis, proposal, alert) {
		fmt.Println("Slack compatibility alert skipped: no actionable outcome; results are in the issue and run artifact")
		return
	}
	payload, err := json.Marshal(map[string]string{"text": slackMessage(status, report, evidence, verification, analysis, proposal, issue, alert)})
	if err != nil {
		panic(err)
	}
	request, err := http.NewRequest(http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		panic(err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		panic(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		panic(fmt.Sprintf("Slack webhook returned %d", response.StatusCode))
	}
	fmt.Println("Slack compatibility alert sent")
}
