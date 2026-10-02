package main

import (
	"strings"
	"testing"
	"time"
)

func TestSuiteVerdictNeedsPassingBaselineToAttributeFailure(t *testing.T) {
	cases := []struct {
		name, baseline, latest, want string
	}{
		{"both pass", "pass", "pass", "pass"},
		{"new failure", "pass", "fail", "fail"},
		{"preexisting failure", "fail", "fail", "not_tested"},
		{"toolchain block", "pass", "blocked", "blocked"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, _ := suiteVerdict(testRun{Status: test.baseline}, testRun{Status: test.latest})
			if got != test.want {
				t.Fatalf("suiteVerdict() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestVerifyMarksNewerGoRequirementBlockedWithoutRunningTests(t *testing.T) {
	evidence := evidenceReport{}
	evidence.Modules = append(evidence.Modules, struct {
		Module               string `json:"module"`
		ToolchainRequirement string `json:"toolchainRequirement"`
	}{Module: "example.com/sdk", ToolchainRequirement: "requires go >= 1.26.0"})
	result := verify(releaseReport{Changes: []releaseChange{{Module: "example.com/sdk", Latest: "v2.0.0", Integrations: []string{"a2a"}}}}, evidence, t.TempDir(), time.Second)
	if len(result.Modules) != 1 || result.Modules[0].Status != "blocked" || !strings.Contains(result.Modules[0].Detail, "1.26.0") {
		t.Fatalf("verify() = %#v", result.Modules)
	}
}

func TestInfrastructureFailureRecognizesToolchainAndNetworkErrors(t *testing.T) {
	if !infrastructureFailure("module requires go >= 1.26.0") || !infrastructureFailure("dial tcp: i/o timeout") {
		t.Fatal("toolchain and network errors must block verification")
	}
	if infrastructureFailure("# github.com/example/adapter\nundefined: upstream.NewMethod") {
		t.Fatal("compiler errors must remain test failures")
	}
}
