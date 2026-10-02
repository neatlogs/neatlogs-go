package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type releaseChange struct {
	Module             string   `json:"module"`
	PreviouslyAnalyzed string   `json:"previouslyAnalyzed"`
	Latest             string   `json:"latest"`
	Integrations       []string `json:"integrations"`
}

type releaseReport struct {
	Changes []releaseChange `json:"changes"`
}

type evidenceReport struct {
	Modules []struct {
		Module               string `json:"module"`
		ToolchainRequirement string `json:"toolchainRequirement"`
	} `json:"modules"`
}

type testRun struct {
	Status          string `json:"status"`
	Detail          string `json:"detail,omitempty"`
	ResolvedVersion string `json:"resolvedVersion,omitempty"`
	Output          string `json:"output,omitempty"`
	Seconds         int    `json:"seconds,omitempty"`
}

type suiteResult struct {
	Integration string  `json:"integration"`
	Package     string  `json:"package"`
	Status      string  `json:"status"`
	Detail      string  `json:"detail,omitempty"`
	Baseline    testRun `json:"baseline"`
	Latest      testRun `json:"latest"`
}

type moduleResult struct {
	Module             string        `json:"module"`
	PreviouslyAnalyzed string        `json:"previouslyAnalyzed"`
	Latest             string        `json:"latest"`
	Status             string        `json:"status"`
	Detail             string        `json:"detail,omitempty"`
	Suites             []suiteResult `json:"suites"`
}

type verificationReport struct {
	SchemaVersion int            `json:"schemaVersion"`
	Ecosystem     string         `json:"ecosystem"`
	GeneratedAt   string         `json:"generatedAt"`
	RunnerGo      string         `json:"runnerGo"`
	Modules       []moduleResult `json:"modules"`
}

var integrationPackages = map[string]string{
	"a2a":          "contrib/adk",
	"google-adk":   "contrib/adk",
	"google-genai": "contrib/genai",
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func boundedOutput(output []byte) string {
	const limit = 12000
	if len(output) <= limit {
		return string(output)
	}
	return "[earlier output omitted]\n" + string(output[len(output)-limit:])
}

func infrastructureFailure(output string) bool {
	text := strings.ToLower(output)
	for _, pattern := range []string{"requires go >=", "network is unreachable", "no such host", "i/o timeout", "tls handshake timeout", "connection reset by peer", "proxy.golang.org: 429", "proxy.golang.org: 502", "proxy.golang.org: 503"} {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

func testModuleVersion(repositoryRoot, packagePath, module, version string, timeout time.Duration) testRun {
	if version == "" {
		return testRun{Status: "not_tested", Detail: "No analyzed baseline version is recorded"}
	}
	started := time.Now()
	moduleDirectory := filepath.Join(repositoryRoot, packagePath)
	temporaryDirectory, err := os.MkdirTemp("", "neatlogs-compat-go-*")
	if err != nil {
		return testRun{Status: "blocked", Detail: fmt.Sprintf("create temporary module file: %v", err)}
	}
	defer os.RemoveAll(temporaryDirectory)
	modFile := filepath.Join(temporaryDirectory, "compat.mod")
	for _, extension := range []string{"mod", "sum"} {
		data, err := os.ReadFile(filepath.Join(moduleDirectory, "go."+extension))
		if err != nil {
			return testRun{Status: "blocked", Detail: fmt.Sprintf("read adapter go.%s: %v", extension, err)}
		}
		if err := os.WriteFile(filepath.Join(temporaryDirectory, "compat."+extension), data, 0o600); err != nil {
			return testRun{Status: "blocked", Detail: fmt.Sprintf("create temporary module file: %v", err)}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	environment := append(os.Environ(), "GOWORK=off")
	commands := [][]string{{"mod", "edit", "-modfile=" + modFile, "-require=" + module + "@" + version, "-replace=github.com/neatlogs/neatlogs-go=" + repositoryRoot},
		{"list", "-m", "-modfile=" + modFile, "-mod=mod", "-f={{.Version}}", module},
		{"test", "-modfile=" + modFile, "-mod=mod", "-count=1", "./..."}}
	resolvedVersion := ""
	for _, arguments := range commands {
		command := exec.CommandContext(ctx, "go", arguments...)
		command.Dir = moduleDirectory
		command.Env = environment
		output, err := command.CombinedOutput()
		if err != nil {
			result := testRun{Status: "fail", Detail: fmt.Sprintf("go %s failed: %v", arguments[0], err), ResolvedVersion: resolvedVersion, Output: boundedOutput(output), Seconds: int(time.Since(started).Seconds())}
			if arguments[0] != "test" || errors.Is(ctx.Err(), context.DeadlineExceeded) || infrastructureFailure(string(output)) {
				result.Status = "blocked"
			}
			return result
		}
		if arguments[0] == "list" {
			resolvedVersion = strings.TrimSpace(string(output))
			if resolvedVersion != version {
				return testRun{Status: "blocked", Detail: fmt.Sprintf("Dependency resolution selected %s instead of requested %s", resolvedVersion, version), ResolvedVersion: resolvedVersion, Seconds: int(time.Since(started).Seconds())}
			}
		}
	}
	return testRun{Status: "pass", Detail: "Adapter package tests passed against this published module version", ResolvedVersion: resolvedVersion, Seconds: int(time.Since(started).Seconds())}
}

func suiteVerdict(baseline, latest testRun) (string, string) {
	switch {
	case latest.Status == "pass":
		return "pass", "Latest version passed the adapter test suite"
	case latest.Status == "blocked":
		return "blocked", "Latest version could not be tested in this runner"
	case baseline.Status == "pass" && latest.Status == "fail":
		return "fail", "Baseline passed but latest failed in the same adapter test suite"
	default:
		return "not_tested", "Latest failed, but the baseline did not pass; a new regression cannot be attributed"
	}
}

func moduleVerdict(suites []suiteResult) string {
	if len(suites) == 0 {
		return "not_tested"
	}
	status := "pass"
	for _, suite := range suites {
		switch suite.Status {
		case "fail":
			return "fail"
		case "blocked":
			status = "blocked"
		case "not_tested":
			if status == "pass" {
				status = "not_tested"
			}
		}
	}
	return status
}

func verify(report releaseReport, evidence evidenceReport, repositoryRoot string, timeout time.Duration) verificationReport {
	result := verificationReport{SchemaVersion: 1, Ecosystem: "go", GeneratedAt: time.Now().UTC().Format(time.RFC3339), RunnerGo: runtime.Version(), Modules: []moduleResult{}}
	requirements := make(map[string]string)
	for _, module := range evidence.Modules {
		requirements[module.Module] = module.ToolchainRequirement
	}
	for _, change := range report.Changes {
		module := moduleResult{Module: change.Module, PreviouslyAnalyzed: change.PreviouslyAnalyzed, Latest: change.Latest, Suites: []suiteResult{}}
		if requirement := requirements[change.Module]; requirement != "" {
			module.Status = "blocked"
			module.Detail = "The published module requires a newer Go version than this runner: " + requirement
			result.Modules = append(result.Modules, module)
			continue
		}
		integrations := append([]string(nil), change.Integrations...)
		sort.Strings(integrations)
		for _, integration := range integrations {
			packagePath := integrationPackages[integration]
			if packagePath == "" {
				module.Suites = append(module.Suites, suiteResult{Integration: integration, Status: "not_tested", Detail: "No adapter test suite is mapped to this integration"})
				continue
			}
			baseline := testModuleVersion(repositoryRoot, packagePath, change.Module, change.PreviouslyAnalyzed, timeout)
			latest := testModuleVersion(repositoryRoot, packagePath, change.Module, change.Latest, timeout)
			status, detail := suiteVerdict(baseline, latest)
			module.Suites = append(module.Suites, suiteResult{Integration: integration, Package: packagePath, Status: status, Detail: detail, Baseline: baseline, Latest: latest})
		}
		module.Status = moduleVerdict(module.Suites)
		result.Modules = append(result.Modules, module)
	}
	return result
}

func main() {
	releasePath := flag.String("release-report", "compatibility-release-report.json", "release report path")
	evidencePath := flag.String("evidence", "compatibility-evidence.json", "evidence report path")
	outputPath := flag.String("output", "compatibility-verification.json", "verification report path")
	timeout := flag.Duration("suite-timeout", 4*time.Minute, "maximum time per version and adapter suite")
	flag.Parse()
	var releases releaseReport
	var evidence evidenceReport
	if err := readJSON(*releasePath, &releases); err != nil {
		panic(err)
	}
	if err := readJSON(*evidencePath, &evidence); err != nil {
		panic(err)
	}
	repositoryRoot, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	result := verify(releases, evidence, repositoryRoot, *timeout)
	if err := writeJSON(*outputPath, result); err != nil {
		panic(err)
	}
	for _, module := range result.Modules {
		fmt.Printf("%s %s: %s\n", module.Module, module.Latest, module.Status)
	}
}
