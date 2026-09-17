package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type integration struct {
	ID                string   `json:"id"`
	Packages          []string `json:"packages"`
	DocumentationURLs []string `json:"documentationUrls"`
	ReleaseMonitoring bool     `json:"releaseMonitoring"`
}

type integrationsFile struct {
	SchemaVersion int           `json:"schemaVersion"`
	Ecosystem     string        `json:"ecosystem"`
	Integrations  []integration `json:"integrations"`
}

type versionsFile struct {
	SchemaVersion int               `json:"schemaVersion"`
	Ecosystem     string            `json:"ecosystem"`
	Packages      map[string]string `json:"packages"`
}

type change struct {
	Module             string   `json:"module"`
	PreviouslyAnalyzed string   `json:"previouslyAnalyzed,omitempty"`
	Latest             string   `json:"latest"`
	Integrations       []string `json:"integrations"`
}

type report struct {
	SchemaVersion  int      `json:"schemaVersion"`
	Ecosystem      string   `json:"ecosystem"`
	GeneratedAt    string   `json:"generatedAt"`
	CheckedModules int      `json:"checkedModules"`
	Changes        []change `json:"changes"`
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func validateConfig(config integrationsFile) error {
	if config.SchemaVersion != 1 || config.Ecosystem != "go" {
		return errors.New("integration config must use schemaVersion 1 and ecosystem go")
	}
	seen := make(map[string]bool)
	for _, item := range config.Integrations {
		if item.ID == "" {
			return errors.New("integration id cannot be empty")
		}
		if seen[item.ID] {
			return fmt.Errorf("duplicate integration id %q", item.ID)
		}
		seen[item.ID] = true
		if item.ReleaseMonitoring && len(item.Packages) == 0 {
			return fmt.Errorf("monitored integration %q has no modules", item.ID)
		}
		if len(item.DocumentationURLs) == 0 {
			return fmt.Errorf("integration %q has no documentation URLs", item.ID)
		}
	}
	return nil
}

func watchedModules(config integrationsFile) map[string][]string {
	modules := make(map[string][]string)
	for _, item := range config.Integrations {
		if !item.ReleaseMonitoring {
			continue
		}
		for _, module := range item.Packages {
			modules[module] = append(modules[module], item.ID)
		}
	}
	for module := range modules {
		sort.Strings(modules[module])
	}
	return modules
}

func selectLatest(versions []string) string {
	for index := len(versions) - 1; index >= 0; index-- {
		if !strings.Contains(versions[index], "-") {
			return versions[index]
		}
	}
	if len(versions) > 0 {
		return versions[len(versions)-1]
	}
	return ""
}

func latestModuleVersion(module string) (string, error) {
	output, err := exec.Command("go", "list", "-m", "-versions", module).Output()
	if err != nil {
		return "", fmt.Errorf("query %s: %w", module, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 {
		return "", fmt.Errorf("registry returned no versions for %s", module)
	}
	latest := selectLatest(fields[1:])
	if latest == "" {
		return "", fmt.Errorf("registry returned no usable versions for %s", module)
	}
	return latest, nil
}

func compareVersions(modules map[string][]string, baselines map[string]string, lookup func(string) (string, error)) ([]change, error) {
	names := make([]string, 0, len(modules))
	for module := range modules {
		names = append(names, module)
	}
	sort.Strings(names)

	changes := make([]change, 0)
	for _, module := range names {
		latest, err := lookup(module)
		if err != nil {
			return nil, err
		}
		previous := baselines[module]
		if previous != latest {
			changes = append(changes, change{
				Module:             module,
				PreviouslyAnalyzed: previous,
				Latest:             latest,
				Integrations:       modules[module],
			})
		}
	}
	return changes, nil
}

func writeGitHubOutput(changesFound bool, reportPath string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "changes_found=%t\nreport_path=%s\n", changesFound, reportPath)
	return err
}

func main() {
	validateOnly := flag.Bool("validate-only", false, "validate compatibility configuration without querying modules")
	reportPath := flag.String("report", "compatibility-release-report.json", "path for the JSON report")
	flag.Parse()

	var config integrationsFile
	if err := readJSON(filepath.FromSlash(".compatibility/integrations.json"), &config); err != nil {
		panic(err)
	}
	if err := validateConfig(config); err != nil {
		panic(err)
	}

	var versions versionsFile
	if err := readJSON(filepath.FromSlash(".compatibility/versions.lock.json"), &versions); err != nil {
		panic(err)
	}
	if versions.SchemaVersion != 1 || versions.Ecosystem != "go" {
		panic("versions lock must use schemaVersion 1 and ecosystem go")
	}

	modules := watchedModules(config)
	if *validateOnly {
		fmt.Printf("Validated %d Go integrations and %d watched modules\n", len(config.Integrations), len(modules))
		return
	}

	changes, err := compareVersions(modules, versions.Packages, latestModuleVersion)
	if err != nil {
		panic(err)
	}
	result := report{
		SchemaVersion:  1,
		Ecosystem:      "go",
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		CheckedModules: len(modules),
		Changes:        changes,
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(*reportPath, data, 0o600); err != nil {
		panic(err)
	}
	if err := writeGitHubOutput(len(changes) > 0, *reportPath); err != nil {
		panic(err)
	}
	fmt.Print(string(data))
}
