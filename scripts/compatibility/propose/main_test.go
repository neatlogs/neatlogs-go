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

func TestValidateRetestRequiresBothVersionSuitesPass(t *testing.T) {
	proposal := proposalReport{Status: "proposed", TargetModule: "example.com/sdk", TargetVersion: "v2"}
	before := verificationReport{}
	before.Modules = append(before.Modules, struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	}{Module: "example.com/sdk", Latest: "v2", Status: "fail"})
	after := before
	after.Modules = append([]struct {
		Module string        `json:"module"`
		Latest string        `json:"latest"`
		Status string        `json:"status"`
		Suites []suiteResult `json:"suites"`
	}(nil), before.Modules...)
	suite := suiteResult{}
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
