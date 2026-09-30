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
	RiskLevel string `json:"riskLevel"`
	Skipped   bool   `json:"skipped"`
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
	return "an unknown step"
}

func slackMessage(status string, report releaseReport, evidence evidenceReport, verification verificationReport, analysis analysisReport, issue upstreamIssue, reviewIssueURL, runURL, failureStage string) string {
	link := ""
	if runURL != "" {
		link = fmt.Sprintf(" <%s|Evidence and workflow run>.", runURL)
	}
	if status != "success" {
		reviewLink := ""
		if reviewIssueURL != "" {
			reviewLink = fmt.Sprintf(" <%s|Review issue>.", reviewIssueURL)
		}
		return fmt.Sprintf(":red_circle: *Go SDK compatibility monitor failed during %s.* No regression verdict is available; inspect the workflow logs for the error.%s%s", failureStage, reviewLink, link)
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
	if analysis.RiskLevel != "" {
		risk = fmt.Sprintf(" Gemini advisory risk: *%s* (unverified).", analysis.RiskLevel)
	} else if analysis.Skipped {
		risk = " Gemini analysis unavailable; deterministic evidence only."
	}
	toolchain := ""
	for _, item := range evidence.Modules {
		if match := requiredGoPattern.FindStringSubmatch(item.ToolchainRequirement); len(match) == 2 {
			toolchain += fmt.Sprintf(" %s requires Go ≥%s; the current SDK CI toolchain cannot use it.", item.Module, match[1])
		}
	}
	verificationItems := make([]string, 0, len(verification.Modules))
	confirmedFailure := false
	for _, item := range verification.Modules {
		verificationItems = append(verificationItems, fmt.Sprintf("%s %s: *%s*", item.Module, item.Latest, item.Status))
		if item.Status == "fail" {
			confirmedFailure = true
		}
	}
	verificationText := " Deterministic adapter tests: " + strings.Join(verificationItems, "; ") + "."
	heading := ":warning: *Go SDK release review needed; no confirmed regression:*"
	if confirmedFailure {
		heading = ":rotating_light: *Go SDK regression in mapped adapter tests (baseline passed, latest failed):*"
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
	if reviewIssueURL != "" {
		reviewIssue = fmt.Sprintf(" <%s|Review issue>.", reviewIssueURL)
	}
	return fmt.Sprintf("%s %d upstream release(s) newer than the analyzed lock. %s%s.%s%s%s%s%s%s", heading, len(report.Changes), strings.Join(items, ", "), remaining, verificationText, toolchain, risk, issueText, reviewIssue, link)
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
	var issue upstreamIssue
	optionalJSON("compatibility-release-report.json", &report)
	optionalJSON("compatibility-evidence.json", &evidence)
	optionalJSON("compatibility-verification.json", &verification)
	optionalJSON("compatibility-llm-analysis.json", &analysis)
	issuePath := os.Getenv("COMPAT_UPSTREAM_ISSUE_FILE")
	if issuePath == "" {
		issuePath = "compatibility-upstream-issue.json"
	}
	optionalJSON(issuePath, &issue)
	payload, err := json.Marshal(map[string]string{"text": slackMessage(status, report, evidence, verification, analysis, issue, os.Getenv("COMPAT_REVIEW_ISSUE_URL"), workflowURL(), failedStage())})
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
