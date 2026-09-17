package main

import (
	"errors"
	"testing"
)

func TestValidateConfigRejectsDuplicateIDs(t *testing.T) {
	config := integrationsFile{
		SchemaVersion: 1,
		Ecosystem:     "go",
		Integrations: []integration{
			{ID: "genai"},
			{ID: "genai"},
		},
	}
	if err := validateConfig(config); err == nil {
		t.Fatal("expected duplicate integration id to fail validation")
	}
}

func TestSelectLatestPrefersStableRelease(t *testing.T) {
	versions := []string{"v1.0.0", "v1.1.0-rc.1", "v1.1.0"}
	if got := selectLatest(versions); got != "v1.1.0" {
		t.Fatalf("selectLatest() = %q, want v1.1.0", got)
	}
}

func TestCompareVersionsDeduplicatesModuleChecks(t *testing.T) {
	modules := map[string][]string{"example.com/sdk": {"one", "two"}}
	calls := 0
	changes, err := compareVersions(modules, map[string]string{"example.com/sdk": "v1.0.0"}, func(module string) (string, error) {
		calls++
		if module != "example.com/sdk" {
			return "", errors.New("unexpected module")
		}
		return "v1.1.0", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("lookup called %d times, want 1", calls)
	}
	if len(changes) != 1 || changes[0].Latest != "v1.1.0" {
		t.Fatalf("unexpected changes: %#v", changes)
	}
}
