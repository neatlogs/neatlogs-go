package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
)

type candidateChange struct {
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type analysis struct {
	ScopeModule       string            `json:"scopeModule"`
	Decision          string            `json:"decision"`
	TargetModule      string            `json:"targetModule"`
	TargetVersion     string            `json:"targetVersion"`
	EvidenceReference string            `json:"evidenceReference"`
	EvidenceRationale string            `json:"evidenceRationale"`
	ProposedChanges   []candidateChange `json:"proposedChanges"`
	Skipped           bool              `json:"skipped"`
}

type releaseReport struct {
	Changes []struct {
		Module string `json:"module"`
		Latest string `json:"latest"`
	} `json:"changes"`
}

type coveredRelease struct {
	Module string `json:"module"`
	Latest string `json:"latest"`
	URL    string `json:"url"`
}

type evidenceReport struct {
	Modules []struct {
		Module               string `json:"module"`
		ToolchainRequirement string `json:"toolchainRequirement"`
		SourceContentChanges []struct {
			Path string `json:"path"`
		} `json:"sourceContentChanges"`
		Integrations []struct {
			AdapterSource []struct {
				Path string `json:"path"`
			} `json:"adapterSource"`
		} `json:"integrations"`
	} `json:"modules"`
}

type suiteResult struct {
	Status   string `json:"status"`
	Baseline struct {
		Status string `json:"status"`
	} `json:"baseline"`
	Latest struct {
		Status string `json:"status"`
	} `json:"latest"`
}

type verificationReport struct {
	Modules []struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	} `json:"modules"`
}

type proposalReport struct {
	Status            string   `json:"status"`
	Reason            string   `json:"reason,omitempty"`
	TargetModule      string   `json:"targetModule,omitempty"`
	TargetVersion     string   `json:"targetVersion,omitempty"`
	EvidenceReference string   `json:"evidenceReference,omitempty"`
	EvidenceRationale string   `json:"evidenceRationale,omitempty"`
	Validation        string   `json:"validation,omitempty"`
	Paths             []string `json:"paths,omitempty"`
}

func readJSON(path string, value any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(content, value)
}

func writeJSON(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o600)
}

func allowedPath(module, path string) bool {
	switch module {
	case "github.com/a2aproject/a2a-go/v2":
		return path == "contrib/adk/a2a.go"
	case "google.golang.org/adk":
		return path == "contrib/adk/adk.go" || path == "contrib/adk/run.go" || path == "contrib/adk/tools.go"
	case "google.golang.org/genai":
		return path == "contrib/genai/genai.go" || path == "contrib/adk/adk.go" || path == "contrib/adk/run.go" || path == "contrib/adk/tools.go"
	}
	return false
}

func validateCandidate(candidate analysis, releases releaseReport, evidence evidenceReport, covered []coveredRelease, root string) (proposalReport, map[string][]byte, error) {
	result := proposalReport{Status: "rejected", TargetModule: candidate.TargetModule, TargetVersion: candidate.TargetVersion, EvidenceReference: candidate.EvidenceReference, EvidenceRationale: candidate.EvidenceRationale}
	if candidate.Decision != "propose_fix" || candidate.Skipped {
		result.Status = "skipped"
		result.Reason = "Gemini did not produce an actionable SDK fix"
		return result, nil, nil
	}
	matchedRelease := false
	for _, release := range releases.Changes {
		matchedRelease = matchedRelease || release.Module == candidate.TargetModule && release.Latest == candidate.TargetVersion
	}
	if !matchedRelease {
		return result, nil, errors.New("proposal target is not a detected module release")
	}
	if candidate.TargetModule != candidate.ScopeModule {
		return result, nil, errors.New("proposal target differs from the module selected for Gemini review")
	}
	for _, prior := range covered {
		if prior.Module == candidate.TargetModule && prior.Latest == candidate.TargetVersion {
			result.Status = "skipped"
			result.Reason = "An automated fix PR already covers this module release: " + prior.URL
			return result, nil, nil
		}
	}
	matchedEvidence := false
	for _, module := range evidence.Modules {
		if module.Module != candidate.TargetModule {
			continue
		}
		if module.ToolchainRequirement != "" {
			return result, nil, errors.New("target release is blocked by a newer Go requirement")
		}
		for _, source := range module.SourceContentChanges {
			matchedEvidence = matchedEvidence || source.Path == candidate.EvidenceReference
		}
		for _, integration := range module.Integrations {
			for _, source := range integration.AdapterSource {
				matchedEvidence = matchedEvidence || source.Path == candidate.EvidenceReference
			}
		}
	}
	if !matchedEvidence || len(candidate.EvidenceRationale) < 20 || len(candidate.EvidenceRationale) > 2000 {
		return result, nil, errors.New("proposal lacks a cited source path and concise evidence rationale")
	}
	if len(candidate.ProposedChanges) == 0 || len(candidate.ProposedChanges) > 8 {
		return result, nil, errors.New("proposal must contain one to eight bounded source replacements")
	}
	modified := make(map[string][]byte)
	originals := make(map[string][]byte)
	totalChangeBytes := 0
	for _, change := range candidate.ProposedChanges {
		if !allowedPath(candidate.TargetModule, change.Path) {
			return result, nil, errors.New("proposal contains a disallowed source path")
		}
		if change.OldText == "" || change.NewText == "" || change.OldText == change.NewText || len(change.OldText) > 8000 || len(change.NewText) > 8000 {
			return result, nil, errors.New("proposal replacement is empty, unchanged, or too large")
		}
		totalChangeBytes += len(change.OldText) + len(change.NewText)
		if totalChangeBytes > 16000 {
			return result, nil, errors.New("proposal exceeds the 16 KB total replacement limit")
		}
		current, seen := modified[change.Path]
		if !seen {
			if len(modified) >= 2 {
				return result, nil, errors.New("proposal may change at most two allowlisted SDK source files")
			}
			path := filepath.Join(root, change.Path)
			file, err := os.Lstat(path)
			if err != nil || !file.Mode().IsRegular() || file.Mode()&os.ModeSymlink != 0 {
				return result, nil, fmt.Errorf("proposal source %q is not a regular file", change.Path)
			}
			current, err = os.ReadFile(path)
			if err != nil {
				return result, nil, err
			}
			originals[change.Path] = current
			result.Paths = append(result.Paths, change.Path)
		}
		if bytes.Count(current, []byte(change.OldText)) != 1 {
			return result, nil, fmt.Errorf("oldText must occur exactly once in %s", change.Path)
		}
		modified[change.Path] = bytes.Replace(current, []byte(change.OldText), []byte(change.NewText), 1)
	}
	for path, patched := range modified {
		formatted, err := format.Source(patched)
		if err != nil {
			return result, nil, fmt.Errorf("proposal does not parse as Go in %s: %w", path, err)
		}
		if bytes.Equal(originals[path], formatted) || len(formatted) > len(originals[path])+12000 {
			return result, nil, fmt.Errorf("proposal has no bounded source change in %s", path)
		}
		modified[path] = formatted
	}
	result.Status = "proposed"
	result.Reason = "Allowlisted SDK source patch awaits isolated adapter tests"
	return result, modified, nil
}

func validateRetest(proposal proposalReport, before, after verificationReport) proposalReport {
	if proposal.Status != "proposed" {
		return proposal
	}
	var original, updated *struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	}
	for i := range before.Modules {
		if before.Modules[i].Module == proposal.TargetModule && before.Modules[i].Latest == proposal.TargetVersion {
			original = &before.Modules[i]
		}
	}
	for i := range after.Modules {
		if after.Modules[i].Module == proposal.TargetModule && after.Modules[i].Latest == proposal.TargetVersion {
			updated = &after.Modules[i]
		}
	}
	if original == nil || updated == nil || len(updated.Suites) == 0 {
		proposal.Status, proposal.Reason = "rejected", "Target module has no completed post-patch adapter tests"
		return proposal
	}
	for _, suite := range updated.Suites {
		if suite.Baseline.Status != "pass" || suite.Latest.Status != "pass" {
			proposal.Status, proposal.Reason = "rejected", "Patched adapter must pass tests at both baseline and new module versions"
			return proposal
		}
	}
	for _, beforeModule := range before.Modules {
		if beforeModule.Status != "pass" {
			continue
		}
		found := false
		for _, afterModule := range after.Modules {
			if afterModule.Module == beforeModule.Module && afterModule.Latest == beforeModule.Latest {
				found = afterModule.Status == "pass"
			}
		}
		if !found {
			proposal.Status, proposal.Reason = "rejected", "Patch caused a previously passing release suite to stop passing"
			return proposal
		}
	}
	proposal.Status = "validated"
	proposal.Reason = "Patched adapter tests passed at baseline and new module versions"
	if original.Status == "fail" {
		proposal.Validation = "test_regression_resolved"
	} else {
		proposal.Validation = "advisory_only_no_reproduction"
	}
	return proposal
}

func main() {
	analysisPath := flag.String("analysis", "compatibility-llm-analysis.json", "Gemini analysis path")
	releasePath := flag.String("release-report", "compatibility-release-report.json", "release report path")
	evidencePath := flag.String("evidence", "compatibility-evidence.json", "evidence report path")
	beforePath := flag.String("verification", "compatibility-verification.json", "original adapter test report path")
	afterPath := flag.String("retest", "compatibility-proposal-verification.json", "post-patch adapter test report path")
	outputPath := flag.String("output", "compatibility-proposal.json", "proposal status path")
	coveredPath := flag.String("covered", "compatibility-covered.json", "automated fix PR coverage path")
	checkTests := flag.Bool("check-tests", false, "validate post-patch adapter tests")
	flag.Parse()
	if *checkTests {
		var proposal proposalReport
		var before, after verificationReport
		for _, item := range []struct {
			path  string
			value any
		}{{*outputPath, &proposal}, {*beforePath, &before}, {*afterPath, &after}} {
			if err := readJSON(item.path, item.value); err != nil {
				panic(err)
			}
		}
		if err := writeJSON(*outputPath, validateRetest(proposal, before, after)); err != nil {
			panic(err)
		}
		return
	}
	var candidate analysis
	var releases releaseReport
	var evidence evidenceReport
	var covered []coveredRelease
	if err := readJSON(*analysisPath, &candidate); err != nil {
		candidate = analysis{Skipped: true}
	}
	if err := readJSON(*releasePath, &releases); err != nil {
		panic(err)
	}
	if err := readJSON(*evidencePath, &evidence); err != nil {
		panic(err)
	}
	_ = readJSON(*coveredPath, &covered)
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	proposal, patches, err := validateCandidate(candidate, releases, evidence, covered, root)
	if err != nil {
		proposal.Status = "rejected"
		proposal.Reason = err.Error()
	}
	if proposal.Status == "proposed" {
		for path, content := range patches {
			if err := os.WriteFile(path, content, 0o644); err != nil {
				panic(err)
			}
		}
	}
	if err := writeJSON(*outputPath, proposal); err != nil {
		panic(err)
	}
	fmt.Printf("SDK fix proposal: %s — %s\n", proposal.Status, strings.TrimSpace(proposal.Reason))
}
