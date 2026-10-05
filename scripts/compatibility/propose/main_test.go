package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureCandidate(t *testing.T) (analysis, releaseReport, evidenceReport, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "contrib/genai/genai.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package genai\nfunc value() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := analysis{Decision: "propose_fix", ScopeModule: "google.golang.org/genai", TargetModule: "google.golang.org/genai", TargetVersion: "v1.71.0", EvidenceReference: "contrib/genai/genai.go", EvidenceRationale: "The adapter calls an upstream method whose behavior changed in this release.", ProposedChanges: []candidateChange{{Path: "contrib/genai/genai.go", OldText: "return 1", NewText: "return 2"}}}
	releases := releaseReport{}
	releases.Changes = append(releases.Changes, struct {
		Module string `json:"module"`
		Latest string `json:"latest"`
	}{Module: candidate.TargetModule, Latest: candidate.TargetVersion})
	evidence := evidenceReport{}
	evidence.Modules = append(evidence.Modules, struct {
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
	}{Module: candidate.TargetModule, Integrations: []struct {
		AdapterSource []struct {
			Path string `json:"path"`
		} `json:"adapterSource"`
	}{{AdapterSource: []struct {
		Path string `json:"path"`
	}{{Path: candidate.EvidenceReference}}}}})
	return candidate, releases, evidence, root
}

func TestValidateCandidateAllowsOnlySmallCitedAdapterPatch(t *testing.T) {
	candidate, releases, evidence, root := fixtureCandidate(t)
	proposal, files, err := validateCandidate(candidate, releases, evidence, nil, root)
	if err != nil || proposal.Status != "proposed" || !strings.Contains(string(files[candidate.ProposedChanges[0].Path]), "return 2") {
		t.Fatalf("validateCandidate() = %#v, %v", proposal, err)
	}
	candidate.ProposedChanges[0].Path = ".github/workflows/compatibility-scheduled.yml"
	if _, _, err := validateCandidate(candidate, releases, evidence, nil, root); err == nil {
		t.Fatal("workflow edits must be rejected")
	}
}

func TestCandidateAlreadyCoveredSkipsWithoutOverwriting(t *testing.T) {
	candidate, releases, evidence, root := fixtureCandidate(t)
	proposal, files, err := validateCandidate(candidate, releases, evidence, []coveredRelease{{Module: candidate.TargetModule, Latest: candidate.TargetVersion, URL: "https://example.test/pr/1"}}, root)
	if err != nil || proposal.Status != "skipped" || files != nil {
		t.Fatalf("validateCandidate() = %#v, %v", proposal, err)
	}
}

func TestValidateCandidateAppliesFourBoundedEditsToOneFile(t *testing.T) {
	candidate, releases, evidence, root := fixtureCandidate(t)
	path := candidate.ProposedChanges[0].Path
	content := "package genai\nfunc first() int { return 1 }\nfunc second() int { return 2 }\nfunc third() int { return 3 }\nfunc fourth() int { return 4 }\n"
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate.ProposedChanges = []candidateChange{
		{Path: path, OldText: "return 1", NewText: "return 11"},
		{Path: path, OldText: "return 2", NewText: "return 22"},
		{Path: path, OldText: "return 3", NewText: "return 33"},
		{Path: path, OldText: "return 4", NewText: "return 44"},
	}
	proposal, files, err := validateCandidate(candidate, releases, evidence, nil, root)
	if err != nil || proposal.Status != "proposed" || len(proposal.Paths) != 1 || proposal.Paths[0] != path {
		t.Fatalf("four-hunk proposal = %#v, %v", proposal, err)
	}
	for _, expected := range []string{"return 11", "return 22", "return 33", "return 44"} {
		if !strings.Contains(string(files[path]), expected) {
			t.Fatalf("patched source missing %q: %s", expected, files[path])
		}
	}
	for len(candidate.ProposedChanges) <= 8 {
		candidate.ProposedChanges = append(candidate.ProposedChanges, candidate.ProposedChanges[0])
	}
	if _, _, err := validateCandidate(candidate, releases, evidence, nil, root); err == nil || !strings.Contains(err.Error(), "one to eight") {
		t.Fatalf("more than eight hunks should be rejected: %v", err)
	}
}

func TestValidateCandidateRejectsThirdSourceFile(t *testing.T) {
	candidate, releases, evidence, root := fixtureCandidate(t)
	for _, path := range []string{"contrib/adk/adk.go", "contrib/adk/run.go"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package adk\nfunc value() int { return 1 }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate.ProposedChanges = append(candidate.ProposedChanges, candidateChange{Path: path, OldText: "return 1", NewText: "return 2"})
	}
	if _, _, err := validateCandidate(candidate, releases, evidence, nil, root); err == nil || !strings.Contains(err.Error(), "at most two") {
		t.Fatalf("third source file should be rejected: %v", err)
	}
}

func TestValidateRetestRequiresBothVersionSuitesPass(t *testing.T) {
	proposal := proposalReport{Status: "proposed", TargetModule: "example.com/sdk", TargetVersion: "v2"}
	before := verificationReport{}
	before.Modules = append(before.Modules, struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	}{Module: "example.com/sdk", Latest: "v2", Status: "fail"})
	failingSuite := suiteResult{Integration: "sdk", Package: "contrib/genai", Status: "fail"}
	failingSuite.Baseline.Status, failingSuite.Latest.Status = "pass", "fail"
	before.Modules[0].Suites = []suiteResult{failingSuite}
	after := before
	after.Modules = append([]struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	}(nil), before.Modules...)
	suite := suiteResult{Integration: "sdk", Package: "contrib/genai", Status: "pass"}
	suite.Baseline.Status, suite.Latest.Status = "pass", "pass"
	after.Modules[0].Status, after.Modules[0].Suites = "pass", []suiteResult{suite}
	got := validateRetest(proposal, before, after)
	if got.Status != "validated" || got.Validation != "test_regression_resolved" {
		t.Fatalf("validateRetest() = %#v", got)
	}
	after.Modules[0].Suites[0].Latest.Status = "fail"
	if got := validateRetest(proposal, before, after); got.Status != "rejected" {
		t.Fatalf("validateRetest() = %#v", got)
	}
}

func TestValidateRetestRejectsAdvisoryOnlyPatchWithoutRedGreenProof(t *testing.T) {
	proposal := proposalReport{Status: "proposed", TargetModule: "example.com/sdk", TargetVersion: "v2"}
	before := verificationReport{}
	before.Modules = append(before.Modules, struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	}{Module: "example.com/sdk", Latest: "v2", Status: "pass"})
	passedSuite := suiteResult{Integration: "sdk", Package: "contrib/genai", Status: "pass"}
	passedSuite.Baseline.Status, passedSuite.Latest.Status = "pass", "pass"
	before.Modules[0].Suites = []suiteResult{passedSuite}
	after := before
	after.Modules = append([]struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	}(nil), before.Modules...)
	got := validateRetest(proposal, before, after)
	if got.Status != "rejected" || !strings.Contains(got.Reason, "No reproduced baseline-pass/latest-fail") || got.Validation != "" {
		t.Fatalf("advisory-only patch must not be publishable: %#v", got)
	}
	before.Modules[0].Status = "fail"
	failingSuite := passedSuite
	failingSuite.Status, failingSuite.Latest.Status = "fail", "fail"
	before.Modules[0].Suites = []suiteResult{failingSuite}
	got = validateRetest(proposal, before, after)
	if got.Status != "validated" {
		t.Fatalf("matching red/green suite should validate: %#v", got)
	}
	after.Modules[0].Suites = []suiteResult{{Integration: "different", Package: "contrib/genai", Status: "pass", Baseline: passedSuite.Baseline, Latest: passedSuite.Latest}}
	got = validateRetest(proposal, before, after)
	if got.Status != "rejected" || !strings.Contains(got.Reason, "same suite") {
		t.Fatalf("different suite must not count as green proof: %#v", got)
	}
}
