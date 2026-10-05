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
		LatestVersion        string `json:"latestVersion"`
		ToolchainRequirement string `json:"toolchainRequirement"`
	} `json:"modules"`
}

type verificationSuite struct {
	Status   string `json:"status"`
	Baseline struct {
		Status string `json:"status"`
	} `json:"baseline"`
	Latest struct {
		Status string `json:"status"`
	} `json:"latest"`
}

type verificationModule struct {
	Module string              `json:"module"`
	Latest string              `json:"latest"`
	Status string              `json:"status"`
	Suites []verificationSuite `json:"suites"`
}

type verificationReport struct {
	Modules []verificationModule `json:"modules"`
}

type upstreamIssue struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type alertContext struct {
	ReviewIssueURL      string
	FixPRURL            string
	FixPROutcome        string
	PublishCheckOutcome string
	PatchJobStatus      string
	ProposalOutcome     string
	RetestOutcome       string
	GeminiOutcome       string
	IssueOutcome        string
	FixIssueOutcome     string
	RunURL              string
	FailureStage        string
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type slackBlock struct {
	Type     string      `json:"type"`
	Text     *slackText  `json:"text,omitempty"`
	Elements []slackText `json:"elements,omitempty"`
}

type slackPayload struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks"`
}

func optionalJSON(path string, value any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, value) == nil
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
		{"COMPAT_PROPOSAL_OUTCOME", "Gemini patch proposal"},
		{"COMPAT_RETEST_OUTCOME", "SDK patch retest"},
		{"COMPAT_PUBLISH_CHECK_OUTCOME", "fix publication validation"},
		{"COMPAT_FIX_PR_OUTCOME", "fix PR creation"},
		{"COMPAT_FIX_ISSUE_OUTCOME", "fix status issue update"},
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
// reserved for a candidate regression, a fix PR, or failed automation.
func shouldSendSlack(status string, report releaseReport, evidence evidenceReport, verification verificationReport, analysis analysisReport, proposal proposalReport, alert alertContext) bool {
	if status != "success" || alert.IssueOutcome == "failure" || alert.FixIssueOutcome == "failure" || alert.GeminiOutcome == "failure" ||
		alert.PatchJobStatus == "failure" || alert.ProposalOutcome == "failure" || alert.RetestOutcome == "failure" ||
		alert.PublishCheckOutcome == "failure" || alert.FixPROutcome == "failure" {
		return true
	}
	if len(report.Changes) == 0 {
		return false
	}
	if len(verification.Modules) != len(report.Changes) {
		return true
	}
	for _, change := range report.Changes {
		found := false
		for _, module := range verification.Modules {
			if module.Module == change.Module && module.Latest == change.Latest {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	if alert.FixPRURL != "" || proposal.Status == "validated" {
		return true
	}
	if analysis.Skipped && analysis.Reason != "No uncovered release is testable with this runner" {
		return true
	}
	for _, module := range verification.Modules {
		if len(module.Suites) == 0 && module.Status == "blocked" && hasToolchainBlock(evidence, module) {
			continue
		}
		knownSuites, preexistingFailure := classifySuites(module.Suites)
		if !knownSuites {
			return true
		}
		switch module.Status {
		case "pass", "blocked":
			// Documented in the issue and artifact; no SDK regression established.
		case "fail":
			return true
		case "not_tested":
			if !preexistingFailure {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func hasToolchainBlock(evidence evidenceReport, module verificationModule) bool {
	for _, item := range evidence.Modules {
		if item.Module == module.Module && item.LatestVersion == module.Latest && item.ToolchainRequirement != "" {
			return true
		}
	}
	return false
}

// The verifier marks baseline-fail/latest-fail suites as not_tested because
// the failure cannot be attributed to the newly published version. Other
// not_tested results lack enough evidence and still warrant an alert.
func classifySuites(suites []verificationSuite) (known, preexistingFailure bool) {
	if len(suites) == 0 {
		return false, false
	}
	for _, suite := range suites {
		if suite.Status == "pass" || suite.Status == "blocked" {
			continue
		}
		if suite.Status != "not_tested" || suite.Baseline.Status != "fail" || suite.Latest.Status != "fail" {
			return false, preexistingFailure
		}
		preexistingFailure = true
	}
	return true, preexistingFailure
}

func slackMessage(status string, report releaseReport, evidence evidenceReport, verification verificationReport, analysis analysisReport, proposal proposalReport, issue upstreamIssue, alert alertContext) string {
	links := make([]string, 0, 3)
	if alert.ReviewIssueURL != "" {
		links = append(links, fmt.Sprintf("<%s|Review issue>", alert.ReviewIssueURL))
	}
	if issue.URL != "" {
		title := issue.Title
		if title == "" {
			title = "Upstream issue"
		}
		links = append(links, fmt.Sprintf("<%s|%s>", issue.URL, conciseSlackText(title)))
	}
	if alert.RunURL != "" {
		links = append(links, fmt.Sprintf("<%s|Workflow evidence>", alert.RunURL))
	}
	linkText := ""
	if len(links) > 0 {
		linkText = "\nDetails: " + strings.Join(links, " · ")
	}
	if status != "success" && len(verification.Modules) == 0 {
		fixText := "none"
		if alert.FixPRURL != "" {
			fixText = fmt.Sprintf("<%s|open for review>", alert.FixPRURL)
		}
		return fmt.Sprintf(":red_circle: *Go SDK: compatibility monitor failed — inspect workflow.*\nChecked: incomplete\nRegression: no verdict\nFix PR: %s\nAction: Investigate failure during %s.%s", fixText, alert.FailureStage, linkText)
	}
	toolchainBlocks := make([]string, 0)
	for _, item := range evidence.Modules {
		if match := requiredGoPattern.FindStringSubmatch(item.ToolchainRequirement); len(match) == 2 {
			toolchainBlocks = append(toolchainBlocks, fmt.Sprintf("%s requires Go ≥%s", item.Module, match[1]))
		}
	}
	passed, failed, blocked, notTested := 0, 0, 0, 0
	for _, item := range verification.Modules {
		switch item.Status {
		case "pass":
			passed++
		case "fail":
			failed++
		case "blocked":
			blocked++
		default:
			notTested++
		}
	}
	releaseLabel := "release"
	if len(report.Changes) != 1 {
		releaseLabel += "s"
	}
	checked := fmt.Sprintf("%d newer %s; mapped adapter checks: %d passed, %d failed, %d blocked, %d not tested", len(report.Changes), releaseLabel, passed, failed, blocked, notTested)
	if len(verification.Modules) == 0 {
		checked = fmt.Sprintf("%d newer %s; mapped adapter checks unavailable", len(report.Changes), releaseLabel)
	}
	regression := "none found in tested scope"
	if failed > 0 {
		candidateLabel := "candidate"
		if failed != 1 {
			candidateLabel += "s"
		}
		regression = fmt.Sprintf("%d %s (baseline passed, latest failed)", failed, candidateLabel)
	} else if len(verification.Modules) == 0 {
		regression = "no verdict"
	} else if blocked+notTested > 0 {
		regression += "; some releases untested"
	}
	fixPR := "none — no validated SDK fix"
	if alert.FixPRURL != "" {
		fixPR = fmt.Sprintf("<%s|open for human review>", alert.FixPRURL)
	} else if proposal.Status == "rejected" {
		fixPR = "none — proposed fix rejected by validation"
	} else if proposal.Status == "validated" && alert.FixPROutcome == "failure" {
		fixPR = "none — PR creation failed"
	} else if proposal.Status == "validated" && alert.PublishCheckOutcome == "failure" {
		fixPR = "none — publication check failed"
	} else if proposal.Status == "validated" {
		fixPR = "none — validated fix was not published"
	} else if alert.PatchJobStatus == "failure" {
		fixPR = "none — patch validation failed"
	} else if alert.ProposalOutcome == "failure" {
		fixPR = "none — proposal tooling failed"
	} else if alert.RetestOutcome == "failure" {
		fixPR = "none — patch retest failed"
	} else if proposal.Status == "skipped" {
		fixPR = "none — no actionable SDK fix"
	}
	heading := ":warning: *Go SDK: compatibility check needs attention — inspect results.*"
	action := "Review the incomplete compatibility check."
	switch {
	case failed > 0:
		heading = ":rotating_light: *Go SDK: candidate regression in mapped adapter tests — review fix.*"
		action = "Review the baseline-pass/latest-fail evidence and SDK fix."
	case alert.FixPRURL != "":
		heading = ":large_green_circle: *Go SDK: fix PR opened — review the code.*"
		action = "Review and approve the PR manually."
		if proposal.Validation == "test_regression_resolved" {
			action += " The mapped regression passed after the patch."
		}
	case alert.GeminiOutcome == "failure" || analysis.Skipped && analysis.Reason != "No uncovered release is testable with this runner":
		heading = ":warning: *Go SDK: Gemini analysis failed — inspect workflow.*"
		action = "Inspect Gemini analysis; mapped adapter checks passed."
	case status != "success" || alert.IssueOutcome == "failure" || alert.FixIssueOutcome == "failure" || alert.PatchJobStatus == "failure" || alert.ProposalOutcome == "failure" || alert.RetestOutcome == "failure" || alert.PublishCheckOutcome == "failure" || alert.FixPROutcome == "failure":
		heading = ":red_circle: *Go SDK: compatibility automation failed — inspect workflow.*"
		action = "Inspect workflow failure and the mapped adapter results."
	case blocked > 0 && len(toolchainBlocks) > 0:
		heading = ":warning: *Go SDK: release blocked by Go toolchain — review Go version support.*"
		action = strings.Join(toolchainBlocks, "; ") + ". Decide whether to upgrade SDK CI to test it."
	case blocked > 0 || notTested > 0 || len(verification.Modules) == 0:
		heading = ":warning: *Go SDK: verification incomplete — investigate untested releases.*"
		action = "Investigate blocked or untested adapter checks."
	}
	if proposal.Status == "rejected" && proposal.Reason != "" {
		action += " Proposed fix rejected: " + conciseSlackText(proposal.Reason) + "."
	} else if proposal.Status == "skipped" && proposal.Reason != "" && failed > 0 {
		action += " No fix proposal: " + conciseSlackText(proposal.Reason) + "."
	}
	if alert.IssueOutcome == "failure" {
		action += " Review issue update failed."
	} else if alert.FixIssueOutcome == "failure" {
		action += " Fix status update to the review issue failed."
	}
	if alert.GeminiOutcome == "failure" && (blocked > 0 || failed > 0 || notTested > 0) {
		action += " Gemini advisory analysis also failed."
	}
	if alert.PatchJobStatus == "failure" || alert.ProposalOutcome == "failure" || alert.RetestOutcome == "failure" || alert.PublishCheckOutcome == "failure" || alert.FixPROutcome == "failure" {
		action += " Inspect failed fix automation in the workflow."
	}
	if status != "success" {
		action += fmt.Sprintf(" Monitor also failed during %s.", alert.FailureStage)
	}
	return fmt.Sprintf("%s\nChecked: %s\nRegression: %s\nFix PR: %s\nAction: %s%s", heading, checked, regression, fixPR, action, linkText)
}

// Keep a plain-text fallback for notifications, and show a small number of
// separate Block Kit sections so the verdict and next action are scannable.
func buildSlackPayload(message string) slackPayload {
	lines := strings.Split(message, "\n")
	result := slackPayload{Text: message}
	if len(lines) == 0 {
		return result
	}
	heading := slackText{Type: "mrkdwn", Text: lines[0]}
	result.Blocks = append(result.Blocks, slackBlock{Type: "section", Text: &heading})
	verdict := make([]string, 0, 3)
	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "Checked: "):
			verdict = append(verdict, "*Checked:* "+strings.TrimPrefix(line, "Checked: "))
		case strings.HasPrefix(line, "Regression: "):
			verdict = append(verdict, "*Regression:* "+strings.TrimPrefix(line, "Regression: "))
		case strings.HasPrefix(line, "Fix PR: "):
			verdict = append(verdict, "*Fix PR:* "+strings.TrimPrefix(line, "Fix PR: "))
		case strings.HasPrefix(line, "Action: "):
			if len(verdict) > 0 {
				status := slackText{Type: "mrkdwn", Text: strings.Join(verdict, "\n")}
				result.Blocks = append(result.Blocks, slackBlock{Type: "section", Text: &status})
				verdict = nil
			}
			action := slackText{Type: "mrkdwn", Text: "*Action:* " + strings.TrimPrefix(line, "Action: ")}
			result.Blocks = append(result.Blocks, slackBlock{Type: "section", Text: &action})
		case strings.HasPrefix(line, "Details: "):
			links := slackText{Type: "mrkdwn", Text: strings.TrimPrefix(line, "Details: ")}
			result.Blocks = append(result.Blocks, slackBlock{Type: "context", Elements: []slackText{links}})
		}
	}
	return result
}

func main() {
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
	reportLoaded := optionalJSON("compatibility-release-report.json", &report)
	evidenceLoaded := optionalJSON("compatibility-evidence.json", &evidence)
	verificationLoaded := optionalJSON("compatibility-verification.json", &verification)
	analysisLoaded := optionalJSON("compatibility-llm-analysis.json", &analysis)
	optionalJSON("compatibility-proposal.json", &proposal)
	issuePath := os.Getenv("COMPAT_UPSTREAM_ISSUE_FILE")
	if issuePath == "" {
		issuePath = "compatibility-upstream-issue.json"
	}
	optionalJSON(issuePath, &issue)
	alert := alertContext{
		ReviewIssueURL: os.Getenv("COMPAT_REVIEW_ISSUE_URL"), FixPRURL: os.Getenv("COMPAT_FIX_PR_URL"),
		FixPROutcome: os.Getenv("COMPAT_FIX_PR_OUTCOME"), PublishCheckOutcome: os.Getenv("COMPAT_PUBLISH_CHECK_OUTCOME"),
		PatchJobStatus: os.Getenv("COMPAT_PATCH_JOB_STATUS"), IssueOutcome: os.Getenv("COMPAT_ISSUE_OUTCOME"),
		ProposalOutcome: os.Getenv("COMPAT_PROPOSAL_OUTCOME"), RetestOutcome: os.Getenv("COMPAT_RETEST_OUTCOME"),
		GeminiOutcome:   os.Getenv("COMPAT_GEMINI_OUTCOME"),
		FixIssueOutcome: os.Getenv("COMPAT_FIX_ISSUE_OUTCOME"),
		RunURL:          workflowURL(), FailureStage: failedStage(),
	}
	if status == "success" && os.Getenv("COMPAT_CHANGES_FOUND") == "true" && (!reportLoaded || !evidenceLoaded || !verificationLoaded || !analysisLoaded) {
		status = "failure"
		alert.FailureStage = "compatibility report or analysis artifact loading"
	}
	if !shouldSendSlack(status, report, evidence, verification, analysis, proposal, alert) {
		fmt.Println("Slack compatibility alert skipped: no actionable outcome; results are in the issue and run artifact")
		return
	}
	webhook := os.Getenv("COMPAT_SLACK_WEBHOOK_URL")
	payload, err := json.Marshal(buildSlackPayload(slackMessage(status, report, evidence, verification, analysis, proposal, issue, alert)))
	if err != nil {
		panic(err)
	}
	if err := postSlack(webhook, payload); err != nil {
		panic(err)
	}
	fmt.Println("Slack compatibility alert sent")
}

func postSlack(webhook string, payload []byte) error {
	if webhook == "" {
		return fmt.Errorf("actionable compatibility alert cannot be delivered: COMPAT_SLACK_WEBHOOK_URL is not configured")
	}
	request, err := http.NewRequest(http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Slack webhook returned %d", response.StatusCode)
	}
	return nil
}
