package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
