package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDownloadKeepsVerifiedArtifactsWhenGoVersionIsTooOld(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "module.zip")
	goMod := filepath.Join(t.TempDir(), "module.mod")
	for _, path := range []string{archive, goMod} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	output, err := json.Marshal(moduleDownload{
		Path: "example.com/sdk", Version: "v2.0.0", Zip: archive, GoMod: goMod,
		Error: "example.com/sdk@v2.0.0 requires go >= 1.26.0 (running go 1.25.0; GOTOOLCHAIN=local)",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseModuleDownload("example.com/sdk", "v2.0.0", output, errors.New("exit status 1"))
	if err != nil || got.Error == "" {
		t.Fatalf("parseModuleDownload() = %#v, %v", got, err)
	}
	got.Error = "network timeout"
	output, _ = json.Marshal(got)
	if _, err := parseModuleDownload("example.com/sdk", "v2.0.0", output, errors.New("exit status 1")); err == nil {
		t.Fatal("unrelated download failures must not be treated as compatibility evidence")
	}
}

func TestGeminiSelectsNextUncoveredTestableModule(t *testing.T) {
	evidence := evidenceReport{Modules: []moduleEvidence{
		{Module: "example.com/blocked", LatestVersion: "v2", ToolchainRequirement: "requires go >= 1.26"},
		{Module: "example.com/covered", LatestVersion: "v2"},
		{Module: "example.com/next", LatestVersion: "v3"},
	}}
	verification := json.RawMessage(`{"modules":[{"module":"example.com/covered","status":"fail"},{"module":"example.com/next","status":"pass"}]}`)
	covered := json.RawMessage(`[{"module":"example.com/covered","latest":"v2"}]`)
	selected, _, module := selectedGeminiEvidence(evidence, verification, covered, 0)
	if module != "example.com/next" || len(selected.Modules) != 1 || selected.Modules[0].Module != module {
		t.Fatalf("selectedGeminiEvidence() = %q, %#v", module, selected.Modules)
	}
}

func TestGeminiRotatesUncoveredModulesAcrossRuns(t *testing.T) {
	evidence := evidenceReport{Modules: []moduleEvidence{{Module: "example.com/first", LatestVersion: "v2"}, {Module: "example.com/second", LatestVersion: "v3"}, {Module: "example.com/third", LatestVersion: "v4"}}}
	verification := json.RawMessage(`{"modules":[{"module":"example.com/first","status":"fail"},{"module":"example.com/second","status":"pass"},{"module":"example.com/third","status":"pass"}]}`)
	want := []string{"example.com/first", "example.com/second", "example.com/third", "example.com/first"}
	for run, expected := range want {
		_, _, got := selectedGeminiEvidence(evidence, verification, json.RawMessage(`[]`), run)
		if got != expected {
			t.Fatalf("run %d selected %q, want %q", run, got, expected)
		}
	}
}

func TestADKReferencedAliasEvidenceAndDecision(t *testing.T) {
	module := moduleEvidence{
		Module: "google.golang.org/adk",
		Integrations: []integrationEvidence{{AdapterSource: []sourceFile{{Path: "contrib/adk/tools.go", Content: `package adk
import "google.golang.org/adk/tool"
func BeforeTool(ctx tool.Context) { _ = ctx; _ = wrapper.field }
`}}}},
	}
	module.AdapterReferencedAliases = adapterReferencedTypeAliases(module.Module, map[string]string{
		"tool/tool.go": "package tool\nimport \"google.golang.org/adk/agent\"\ntype Context = agent.ToolContext\ntype Unused = agent.ToolContext\n",
	}, module.Integrations)
	want := []string{"tool/tool.go: type Context = agent.ToolContext"}
	if !reflect.DeepEqual(module.AdapterReferencedAliases, want) {
		t.Fatalf("adapterReferencedTypeAliases() = %#v, want %#v", module.AdapterReferencedAliases, want)
	}
	evidenceJSON, err := json.Marshal(evidenceReport{Modules: []moduleEvidence{module}})
	if err != nil {
		t.Fatal(err)
	}
	prompt := geminiPrompt(module.Module, json.RawMessage(`{"modules":[]}`), json.RawMessage(`[]`), evidenceJSON)
	if !strings.Contains(prompt, want[0]) || !strings.Contains(prompt, "identical types") || !strings.Contains(prompt, "Use review_only") {
		t.Fatalf("Gemini prompt omitted alias evidence or decision guidance: %s", prompt)
	}
	proposal := func() map[string]any {
		return map[string]any{"decision": "propose_fix", "riskLevel": "high", "summary": "tool.Context changed to agent.ToolContext", "findings": []any{map[string]any{"description": "tool.Context must change to agent.ToolContext"}}, "proposedChanges": []any{
			map[string]any{"path": "contrib/adk/tools.go", "oldText": `"google.golang.org/adk/tool"`, "newText": `"google.golang.org/adk/agent"` + "\n\t" + `"google.golang.org/adk/tool"`},
			map[string]any{"path": "contrib/adk/tools.go", "oldText": "func BeforeTool(ctx tool.Context) {}", "newText": "func BeforeTool(ctx agent.ToolContext) {}"},
			map[string]any{"path": "contrib/adk/tools.go", "oldText": "func AfterTool(ctx tool.Context) {}", "newText": "func AfterTool(ctx agent.ToolContext) {}"},
			map[string]any{"path": "contrib/adk/tools.go", "oldText": "func toolCallKey(ctx tool.Context) {}", "newText": "func toolCallKey(ctx agent.ToolContext) {}"},
		}}
	}
	aliasOnly := proposal()
	discardADKAliasOnlyProposal(aliasOnly, module)
	if aliasOnly["decision"] != "review_only" || aliasOnly["riskLevel"] != "low" || len(aliasOnly["proposedChanges"].([]any)) != 0 {
		t.Fatalf("alias-only proposal was not downgraded: %#v", aliasOnly)
	}
	meaningful := proposal()
	meaningful["proposedChanges"] = append(meaningful["proposedChanges"].([]any), map[string]any{"path": "contrib/adk/tools.go", "oldText": "return old", "newText": "return fixed"})
	discardADKAliasOnlyProposal(meaningful, module)
	if meaningful["decision"] != "propose_fix" {
		t.Fatalf("meaningful fix was suppressed: %#v", meaningful)
	}
	separateFinding := proposal()
	separateFinding["findings"] = append(separateFinding["findings"].([]any), map[string]any{"description": "An unrelated callback behavior changed"})
	discardADKAliasOnlyProposal(separateFinding, module)
	if separateFinding["decision"] != "propose_fix" {
		t.Fatalf("a separate finding was suppressed: %#v", separateFinding)
	}
	noAlias := module
	noAlias.AdapterReferencedAliases = nil
	withoutEvidence := proposal()
	discardADKAliasOnlyProposal(withoutEvidence, noAlias)
	if withoutEvidence["decision"] != "propose_fix" {
		t.Fatalf("proposal without published alias evidence was suppressed: %#v", withoutEvidence)
	}
}

func TestArchivePathDropsModuleAndVersionRoot(t *testing.T) {
	got := normalizedArchivePath("github.com/a2aproject/a2a-go/v2@v2.6.0/client/client.go")
	if got != "client/client.go" {
		t.Fatalf("normalizedArchivePath() = %q", got)
	}
}

func TestDiffObjects(t *testing.T) {
	got := diffObjects(map[string]any{"Go": "1.23"}, map[string]any{"Go": "1.24"})
	want := []objectChange{{Key: "Go", Before: "1.23", After: "1.24"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffObjects() = %#v, want %#v", got, want)
	}
}

func TestDiffFiles(t *testing.T) {
	got := diffFiles(
		[]fileInfo{{Path: "old.go", Size: 1}, {Path: "changed.go", Size: 2}},
		[]fileInfo{{Path: "new.go", Size: 1}, {Path: "changed.go", Size: 3}},
	)
	want := fileChanges{
		Added:       []string{"new.go"},
		Removed:     []string{"old.go"},
		SizeChanged: []sizeChange{{Path: "changed.go", BeforeBytes: 2, AfterBytes: 3}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffFiles() = %#v, want %#v", got, want)
	}
}

func TestExtractGoAPIAndDiff(t *testing.T) {
	before := map[string]string{"client.go": "package sdk\nfunc Instrument(context string) error { return nil }\nfunc hidden() {}\n"}
	after := map[string]string{"client.go": "package sdk\nfunc Instrument(runtimeContext map[string]any) error { return nil }\n"}
	got := diffAPI(extractGoAPI(before), extractGoAPI(after))
	want := apiChanges{
		Added:   []string{"client.go: func Instrument(runtimeContext map[string]any) error"},
		Removed: []string{"client.go: func Instrument(context string) error"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffAPI() = %#v, want %#v", got, want)
	}
}

func TestDiffTextFilesIncludesSourceLines(t *testing.T) {
	got := diffTextFiles(
		map[string]string{"client.go": "const Context = \"experimental\"\n"},
		map[string]string{"client.go": "const Context = \"runtime\"\n"},
	)
	want := []contentChange{{
		Path:         "client.go",
		AddedLines:   []string{"const Context = \"runtime\""},
		RemovedLines: []string{"const Context = \"experimental\""},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("diffTextFiles() = %#v, want %#v", got, want)
	}
}

func TestOfficialDocumentationUsesVersionedGoDocsAndRepository(t *testing.T) {
	got := officialDocumentationURLs(moduleDownload{
		Path:    "example.com/sdk",
		Version: "v2.0.0",
		Origin:  &moduleOrigin{VCS: "git", URL: "https://github.com/example/sdk"},
	})
	want := []documentationEvidence{
		{Kind: "versioned-api-documentation", URL: "https://pkg.go.dev/example.com/sdk@v2.0.0"},
		{Kind: "source-repository", URL: "https://github.com/example/sdk"},
		{Kind: "release-notes", URL: "https://github.com/example/sdk/releases"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("officialDocumentationURLs() = %#v, want %#v", got, want)
	}
}

func TestDocumentationTextExtractsVisibleHTML(t *testing.T) {
	got := documentationText("<html><script>ignore()</script><body><h1>Migration</h1><p>Use RuntimeContext.</p></body></html>", "text/html")
	if got != "Migration Use RuntimeContext." {
		t.Fatalf("documentationText() = %q", got)
	}
}
