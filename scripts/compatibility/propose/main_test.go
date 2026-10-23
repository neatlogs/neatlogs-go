package main

import (
	"encoding/json"
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

func TestFocusedRegressionTestIsBoundedAndCannotReplaceExistingFile(t *testing.T) {
	candidate, releases, evidence, root := fixtureCandidate(t)
	candidate.ProposedTest = &candidateTest{Package: "genai", Content: "package genai\nimport \"testing\"\nfunc TestCompatibilitySemanticRegression(t *testing.T) { if false { t.Fatal(\"unreachable\") } }\n"}
	proposal, patches, err := validateCandidate(candidate, releases, evidence, nil, root)
	if err != nil || proposal.Status != "proposed" || proposal.TestPath == "" || proposal.TestName != "TestCompatibilitySemanticRegression" || !strings.HasPrefix(proposal.TestPath, "contrib/genai/compatibility_") || len(patches[proposal.TestPath]) == 0 {
		t.Fatalf("focused test proposal = %#v, %v", proposal, err)
	}
	fullPath := filepath.Join(root, proposal.TestPath)
	if err := os.WriteFile(fullPath, []byte("package genai\nfunc TestCompatibilityHijack() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateCandidate(candidate, releases, evidence, nil, root); err == nil || !strings.Contains(err.Error(), "already contains different source") {
		t.Fatalf("existing test source must not be overwritten: %v", err)
	}
	candidate.ProposedTest.Content = "package genai\nfunc init() {}\n"
	if _, _, err := validateCandidate(candidate, releases, evidence, nil, root); err == nil || !strings.Contains(err.Error(), "one nonempty TestCompatibility") {
		t.Fatalf("generated init function must be rejected: %v", err)
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

func TestValidateRetestPreservesPassingSuiteInMixedStatusModule(t *testing.T) {
	proposal := proposalReport{Status: "proposed", TargetModule: "example.com/target", TargetVersion: "v2"}
	var before, after verificationReport
	if err := json.Unmarshal([]byte(`{"modules":[
		{"module":"example.com/target","latest":"v2","status":"fail","suites":[{"integration":"target","package":"contrib/genai","status":"fail","baseline":{"status":"pass"},"latest":{"status":"fail"}}]},
		{"module":"example.com/other","latest":"v3","status":"blocked","suites":[{"integration":"shared","package":"contrib/adk","status":"pass","baseline":{"status":"pass"},"latest":{"status":"pass"}},{"integration":"blocked","package":"contrib/adk","status":"blocked","baseline":{"status":"pass"},"latest":{"status":"blocked"}}]}
	]}`), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"modules":[
		{"module":"example.com/target","latest":"v2","status":"pass","suites":[{"integration":"target","package":"contrib/genai","status":"pass","baseline":{"status":"pass"},"latest":{"status":"pass"}}]},
		{"module":"example.com/other","latest":"v3","status":"fail","suites":[{"integration":"shared","package":"contrib/adk","status":"fail","baseline":{"status":"pass"},"latest":{"status":"fail"}},{"integration":"blocked","package":"contrib/adk","status":"blocked","baseline":{"status":"pass"},"latest":{"status":"blocked"}}]}
	]}`), &after); err != nil {
		t.Fatal(err)
	}
	got := validateRetest(proposal, before, after)
	if got.Status != "rejected" || !strings.Contains(got.Reason, "previously passing mapped adapter suite") {
		t.Fatalf("collateral suite failure must reject the patch: %#v", got)
	}
}

func TestFocusedTestNeedsOriginalGreenRedGreenProof(t *testing.T) {
	proposal := proposalReport{Status: "proposed", TargetModule: "example.com/sdk", TargetVersion: "v2", TestPath: "contrib/genai/compatibility_123_test.go", TestName: "TestCompatibilitySemanticRegression"}
	var original, red, green verificationReport
	for _, item := range []struct {
		text  string
		value any
	}{
		{`{"modules":[{"module":"example.com/sdk","latest":"v2","status":"pass","suites":[{"integration":"genai","package":"contrib/genai","status":"pass","baseline":{"status":"pass"},"latest":{"status":"pass"}}]}]}`, &original},
		{`{"modules":[{"module":"example.com/sdk","latest":"v2","status":"fail","suites":[{"integration":"genai","package":"contrib/genai","status":"fail","baseline":{"status":"pass"},"latest":{"status":"fail","output":"--- FAIL: TestCompatibilitySemanticRegression (0.00s)"}}]}]}`, &red},
		{`{"modules":[{"module":"example.com/sdk","latest":"v2","status":"pass","suites":[{"integration":"genai","package":"contrib/genai","status":"pass","baseline":{"status":"pass"},"latest":{"status":"pass"}}]}]}`, &green},
	} {
		if err := json.Unmarshal([]byte(item.text), item.value); err != nil {
			t.Fatal(err)
		}
	}
	if got := validateFocusedRetest(proposal, original, red, green); got.Status != "validated" || got.Validation != "test_regression_resolved" {
		t.Fatalf("focused red-green proof should validate: %#v", got)
	}
	if got := validateFocusedRetest(proposal, original, original, green); got.Status != "rejected" {
		t.Fatalf("advisory-only focused test must not publish: %#v", got)
	}
	red.Modules[0].Suites[0].Latest.Output = "--- FAIL: TestUnrelatedFlake (0.00s)"
	if got := validateFocusedRetest(proposal, original, red, green); got.Status != "rejected" {
		t.Fatalf("another test failure must not prove the generated regression: %#v", got)
	}
	red.Modules[0].Suites[0].Latest.Output = "--- FAIL: TestCompatibilitySemanticRegression (0.00s)"
	otherOriginal := original.Modules[0]
	otherOriginal.Module, otherOriginal.Latest = "example.com/other", "v3"
	otherSuite := suiteResult{Integration: "adk", Package: "contrib/adk", Status: "pass"}
	otherSuite.Baseline.Status, otherSuite.Latest.Status = "pass", "pass"
	otherOriginal.Suites = []suiteResult{otherSuite}
	original.Modules = append(original.Modules, otherOriginal)
	otherAfter := otherOriginal
	otherAfter.Status = "fail"
	otherSuite.Status, otherSuite.Latest.Status = "fail", "fail"
	otherAfter.Suites = []suiteResult{otherSuite}
	red.Modules = append(red.Modules, otherAfter)
	green.Modules = append(green.Modules, otherAfter)
	if got := validateFocusedRetest(proposal, original, red, green); got.Status != "rejected" || !strings.Contains(got.Reason, "previously passing") {
		t.Fatalf("focused test must not break another passing module: %#v", got)
	}
	original.Modules[0].Status = "blocked"
	if got := validateFocusedRetest(proposal, original, red, green); got.Status != "rejected" {
		t.Fatalf("incomplete original verifier must not publish: %#v", got)
	}
}
