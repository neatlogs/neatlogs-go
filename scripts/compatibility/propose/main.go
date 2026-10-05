package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

type candidateChange struct {
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type candidateTest struct {
	Package string `json:"package"`
	Content string `json:"content"`
}

type analysis struct {
	ScopeModule       string            `json:"scopeModule"`
	Decision          string            `json:"decision"`
	TargetModule      string            `json:"targetModule"`
	TargetVersion     string            `json:"targetVersion"`
	EvidenceReference string            `json:"evidenceReference"`
	EvidenceRationale string            `json:"evidenceRationale"`
	ProposedChanges   []candidateChange `json:"proposedChanges"`
	ProposedTest      *candidateTest    `json:"proposedTest,omitempty"`
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
	Integration string `json:"integration"`
	Package     string `json:"package"`
	Status      string `json:"status"`
	Baseline    struct {
		Status string `json:"status"`
	} `json:"baseline"`
	Latest struct {
		Status string `json:"status"`
		Output string `json:"output"`
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
	TestPath          string   `json:"testPath,omitempty"`
	TestName          string   `json:"testName,omitempty"`
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

func validateFocusedTest(candidate analysis) (string, []byte, string, error) {
	if candidate.ProposedTest == nil {
		return "", nil, "", nil
	}
	test := candidate.ProposedTest
	if len(test.Content) == 0 || len(test.Content) > 12000 {
		return "", nil, "", errors.New("focused regression test must contain at most 12 KB of Go source")
	}
	if test.Package != "adk" && test.Package != "genai" {
		return "", nil, "", errors.New("focused regression test targets an unknown adapter package")
	}
	if test.Package == "genai" && candidate.TargetModule != "google.golang.org/genai" {
		return "", nil, "", errors.New("focused regression test targets an unrelated adapter package")
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "focused_test.go", test.Content, 0)
	if err != nil || file.Name.Name != test.Package {
		return "", nil, "", errors.New("focused regression test must parse in its adapter package")
	}
	functionCount := 0
	testName := ""
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.GenDecl:
			if typed.Tok != token.IMPORT {
				return "", nil, "", errors.New("focused regression test may contain only imports and one TestCompatibility function")
			}
		case *ast.FuncDecl:
			if typed.Recv != nil || !strings.HasPrefix(typed.Name.Name, "TestCompatibility") || typed.Body == nil || len(typed.Body.List) == 0 {
				return "", nil, "", errors.New("focused regression test must contain one nonempty TestCompatibility function")
			}
			functionCount++
			testName = typed.Name.Name
		default:
			return "", nil, "", errors.New("focused regression test contains an unsupported declaration")
		}
	}
	if functionCount != 1 {
		return "", nil, "", errors.New("focused regression test must contain exactly one TestCompatibility function")
	}
	formatted, err := format.Source([]byte(test.Content))
	if err != nil {
		return "", nil, "", err
	}
	hash := sha256.Sum256([]byte(candidate.TargetModule + "@" + candidate.TargetVersion))
	path := fmt.Sprintf("contrib/%s/compatibility_%x_test.go", test.Package, hash[:6])
	return path, formatted, testName, nil
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
	if candidate.ProposedTest != nil {
		testPath, testContent, testName, err := validateFocusedTest(candidate)
		if err != nil {
			return result, nil, err
		}
		fullPath := filepath.Join(root, testPath)
		if existing, err := os.Lstat(fullPath); err == nil {
			if !existing.Mode().IsRegular() || existing.Mode()&os.ModeSymlink != 0 {
				return result, nil, errors.New("focused regression test path is not a regular file")
			}
			content, err := os.ReadFile(fullPath)
			if err != nil || !bytes.Equal(content, testContent) {
				return result, nil, errors.New("focused regression test path already contains different source")
			}
		} else if !os.IsNotExist(err) {
			return result, nil, err
		}
		modified[testPath] = testContent
		result.TestPath = testPath
		result.TestName = testName
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
	// A passing mapped suite cannot prove that an advisory-only patch fixes an
	// SDK regression. Keep the finding in the review issue until a failing
	// baseline-pass/latest-fail case is reproduced.
	if original.Status != "fail" {
		proposal.Status, proposal.Reason = "rejected", "No reproduced baseline-pass/latest-fail adapter regression; advisory findings remain review-only"
		return proposal
	}
	reproduced := make([]suiteResult, 0)
	for _, suite := range original.Suites {
		if suite.Integration != "" && suite.Package != "" && suite.Status == "fail" && suite.Baseline.Status == "pass" && suite.Latest.Status == "fail" {
			reproduced = append(reproduced, suite)
		}
	}
	if len(reproduced) == 0 {
		proposal.Status, proposal.Reason = "rejected", "Target module has no baseline-pass/latest-fail adapter suite evidence"
		return proposal
	}
	if updated.Status != "pass" {
		proposal.Status, proposal.Reason = "rejected", "Patched target module did not pass mapped adapter tests"
		return proposal
	}
	for _, suite := range updated.Suites {
		if suite.Status != "pass" || suite.Baseline.Status != "pass" || suite.Latest.Status != "pass" {
			proposal.Status, proposal.Reason = "rejected", "Patched adapter must pass tests at both baseline and new module versions"
			return proposal
		}
	}
	for _, prior := range reproduced {
		resolved := false
		for _, suite := range updated.Suites {
			if suite.Integration == prior.Integration && suite.Package == prior.Package && suite.Status == "pass" && suite.Baseline.Status == "pass" && suite.Latest.Status == "pass" {
				resolved = true
				break
			}
		}
		if !resolved {
			proposal.Status, proposal.Reason = "rejected", "Patched adapter did not pass the same suite that failed at the latest version"
			return proposal
		}
	}
	// A shared adapter source file can affect another watched module. Preserve
	// every suite that passed before the patch, including suites in a module
	// whose overall result was blocked or inconclusive because of other suites.
	for _, beforeModule := range before.Modules {
		for _, beforeSuite := range beforeModule.Suites {
			if beforeSuite.Status != "pass" || beforeSuite.Latest.Status != "pass" {
				continue
			}
			preserved := false
			for _, afterModule := range after.Modules {
				if afterModule.Module != beforeModule.Module || afterModule.Latest != beforeModule.Latest {
					continue
				}
				for _, afterSuite := range afterModule.Suites {
					if afterSuite.Integration == beforeSuite.Integration && afterSuite.Package == beforeSuite.Package &&
						afterSuite.Status == "pass" && afterSuite.Latest.Status == "pass" &&
						(beforeSuite.Baseline.Status != "pass" || afterSuite.Baseline.Status == "pass") {
						preserved = true
						break
					}
				}
			}
			if !preserved {
				proposal.Status, proposal.Reason = "rejected", "Patch caused a previously passing mapped adapter suite to stop passing"
				return proposal
			}
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
	proposal.Reason = "Baseline-pass/latest-fail mapped adapter tests passed after the patch"
	proposal.Validation = "test_regression_resolved"
	return proposal
}

func validateFocusedRetest(proposal proposalReport, original, before, after verificationReport) proposalReport {
	if proposal.TestPath == "" {
		return validateRetest(proposal, before, after)
	}
	if proposal.TestName == "" || !strings.HasPrefix(proposal.TestName, "TestCompatibility") {
		proposal.Status, proposal.Reason = "rejected", "Focused regression proof is missing its generated test identity"
		return proposal
	}
	testPackage := filepath.Dir(proposal.TestPath)
	matched := false
	for _, module := range original.Modules {
		if module.Module != proposal.TargetModule || module.Latest != proposal.TargetVersion || module.Status != "pass" {
			continue
		}
		for _, originalSuite := range module.Suites {
			if originalSuite.Package != testPackage || originalSuite.Status != "pass" || originalSuite.Baseline.Status != "pass" || originalSuite.Latest.Status != "pass" {
				continue
			}
			for _, focusedModule := range before.Modules {
				if focusedModule.Module != module.Module || focusedModule.Latest != module.Latest || focusedModule.Status != "fail" {
					continue
				}
				for _, focusedSuite := range focusedModule.Suites {
					if focusedSuite.Integration == originalSuite.Integration && focusedSuite.Package == testPackage &&
						focusedSuite.Status == "fail" && focusedSuite.Baseline.Status == "pass" && focusedSuite.Latest.Status == "fail" &&
						strings.Contains(focusedSuite.Latest.Output, "--- FAIL: "+proposal.TestName+" ") {
						matched = true
					}
				}
			}
		}
	}
	if !matched {
		proposal.Status, proposal.Reason = "rejected", "Focused regression test did not cause a baseline-pass/latest-fail result in its own adapter suite"
		return proposal
	}
	proposal = validateRetest(proposal, before, after)
	if proposal.Status != "validated" {
		return proposal
	}
	for _, originalModule := range original.Modules {
		for _, originalSuite := range originalModule.Suites {
			if originalSuite.Status != "pass" || originalSuite.Latest.Status != "pass" {
				continue
			}
			preserved := false
			for _, afterModule := range after.Modules {
				if afterModule.Module != originalModule.Module || afterModule.Latest != originalModule.Latest {
					continue
				}
				for _, afterSuite := range afterModule.Suites {
					if afterSuite.Integration == originalSuite.Integration && afterSuite.Package == originalSuite.Package &&
						afterSuite.Status == "pass" && afterSuite.Latest.Status == "pass" &&
						(originalSuite.Baseline.Status != "pass" || afterSuite.Baseline.Status == "pass") {
						preserved = true
						break
					}
				}
			}
			if !preserved {
				proposal.Status, proposal.Reason, proposal.Validation = "rejected", "Generated test or SDK patch broke a previously passing adapter suite", ""
				return proposal
			}
		}
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
	testOnly := flag.Bool("test-only", false, "stage only a new focused regression test before the SDK source patch")
	originalPath := flag.String("original", "", "original adapter test report before the focused regression test")
	flag.Parse()
	if *checkTests {
		var proposal proposalReport
		var before, after verificationReport
		var original verificationReport
		for _, item := range []struct {
			path  string
			value any
		}{{*outputPath, &proposal}, {*beforePath, &before}, {*afterPath, &after}} {
			if err := readJSON(item.path, item.value); err != nil {
				panic(err)
			}
		}
		if proposal.TestPath != "" {
			if *originalPath == "" || readJSON(*originalPath, &original) != nil {
				panic("focused regression proof requires the original adapter verification report")
			}
		}
		if err := writeJSON(*outputPath, validateFocusedRetest(proposal, original, before, after)); err != nil {
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
		if *testOnly && proposal.TestPath != "" {
			if _, err := os.Lstat(proposal.TestPath); err == nil || !os.IsNotExist(err) {
				proposal.Status, proposal.Reason = "rejected", "Focused regression test path already exists on this branch"
			}
		}
		if proposal.Status == "proposed" {
			for path, content := range patches {
				if *testOnly && proposal.TestPath != "" && path != proposal.TestPath {
					continue
				}
				if err := os.WriteFile(path, content, 0o644); err != nil {
					panic(err)
				}
			}
		}
	}
	if err := writeJSON(*outputPath, proposal); err != nil {
		panic(err)
	}
	fmt.Printf("SDK fix proposal: %s — %s\n", proposal.Status, strings.TrimSpace(proposal.Reason))
}
