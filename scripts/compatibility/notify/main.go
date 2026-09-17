package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

func slackMessage(status string, report releaseReport, analysis analysisReport, runURL string) string {
	link := ""
	if runURL != "" {
		link = fmt.Sprintf(" <%s|Open workflow run>.", runURL)
	}
	if status != "success" {
		return ":red_circle: *Go SDK compatibility workflow failed.*" + link
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
		risk = fmt.Sprintf(" Advisory risk: *%s*.", analysis.RiskLevel)
	}
	return fmt.Sprintf(":warning: *Go SDK compatibility review required:* %d upstream release(s). %s%s.%s%s", len(report.Changes), strings.Join(items, ", "), remaining, risk, link)
}

func main() {
	webhook := os.Getenv("COMPAT_SLACK_WEBHOOK_URL")
	if webhook == "" {
		fmt.Println("Slack notification skipped: COMPAT_SLACK_WEBHOOK_URL is not configured")
		return
	}
	status := os.Getenv("COMPAT_JOB_STATUS")
	if status == "success" && os.Getenv("COMPAT_CHANGES_FOUND") != "true" {
		return
	}
	var report releaseReport
	var analysis analysisReport
	optionalJSON("compatibility-release-report.json", &report)
	optionalJSON("compatibility-llm-analysis.json", &analysis)
	payload, err := json.Marshal(map[string]string{"text": slackMessage(status, report, analysis, workflowURL())})
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
