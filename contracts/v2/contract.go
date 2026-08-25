// Package telemetryv2 embeds the canonical NeatLogs telemetry contract v2.
//
// The files in this package are vendored byte-for-byte from the authority named
// in manifest.json. Validate verifies that the embedded schema still has the
// authority-published digest and that the bundled golden envelopes satisfy the
// contract's packaging invariants.
package telemetryv2

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
)

const (
	manifestPath = "manifest.json"
	goldenDir    = "golden"
)

// Files contains the canonical schema, manifest, README, and golden fixtures.
// Callers should prefer Schema, Manifest, and GoldenFixtures when they need a
// defensive copy rather than direct filesystem access.
//
//go:embed README.md manifest.json neatlogs-telemetry.schema.json golden/*.json
var Files embed.FS

// Manifest describes the canonical contract artifact and its authority.
type Manifest struct {
	ContractVersion string `json:"contract_version"`
	SchemaVersion   int    `json:"schema_version"`
	SchemaFile      string `json:"schema_file"`
	SchemaID        string `json:"schema_id"`
	SchemaSHA256    string `json:"schema_sha256"`
	Authority       string `json:"authority"`
}

// Fixture is one immutable golden envelope bundled with the contract.
type Fixture struct {
	Name string
	JSON []byte
}

// LoadManifest decodes and returns the embedded manifest.
func LoadManifest() (Manifest, error) {
	data, err := Files.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("read telemetry v2 manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode telemetry v2 manifest: %w", err)
	}
	return manifest, nil
}

// Schema returns a defensive copy of the canonical schema bytes after checking
// the manifest-selected path. Digest validation is performed by Validate.
func Schema() ([]byte, error) {
	manifest, err := LoadManifest()
	if err != nil {
		return nil, err
	}
	if path.Base(manifest.SchemaFile) != manifest.SchemaFile || manifest.SchemaFile == "." {
		return nil, fmt.Errorf("telemetry v2 manifest has unsafe schema_file %q", manifest.SchemaFile)
	}
	data, err := Files.ReadFile(manifest.SchemaFile)
	if err != nil {
		return nil, fmt.Errorf("read telemetry v2 schema: %w", err)
	}
	return append([]byte(nil), data...), nil
}

// GoldenFixtures returns all bundled fixture bytes in stable filename order.
func GoldenFixtures() ([]Fixture, error) {
	entries, err := fs.ReadDir(Files, goldenDir)
	if err != nil {
		return nil, fmt.Errorf("read telemetry v2 golden fixtures: %w", err)
	}
	fixtures := make([]Fixture, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || path.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := Files.ReadFile(path.Join(goldenDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read telemetry v2 golden fixture %q: %w", entry.Name(), err)
		}
		fixtures = append(fixtures, Fixture{Name: entry.Name(), JSON: append([]byte(nil), data...)})
	}
	sort.Slice(fixtures, func(i, j int) bool { return fixtures[i].Name < fixtures[j].Name })
	if len(fixtures) == 0 {
		return nil, fmt.Errorf("telemetry v2 contract has no golden fixtures")
	}
	return fixtures, nil
}

// Validate checks package integrity and the language-neutral golden-envelope
// invariants without rewriting or interpreting provider-specific telemetry.
func Validate() error {
	manifest, err := LoadManifest()
	if err != nil {
		return err
	}
	if manifest.ContractVersion != "2.0.0" || manifest.SchemaVersion != 2 {
		return fmt.Errorf("unsupported telemetry contract %q schema version %d", manifest.ContractVersion, manifest.SchemaVersion)
	}
	if manifest.SchemaID == "" || manifest.Authority == "" {
		return fmt.Errorf("telemetry v2 manifest is missing schema_id or authority")
	}
	schemaBytes, err := Schema()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(schemaBytes)
	if actual := hex.EncodeToString(digest[:]); actual != manifest.SchemaSHA256 {
		return fmt.Errorf("telemetry v2 schema digest mismatch: got %s want %s", actual, manifest.SchemaSHA256)
	}
	var schema struct {
		ID     string `json:"$id"`
		Policy struct {
			ContractVersion string `json:"contract_version"`
		} `json:"x-neatlogs-policy"`
	}
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		return fmt.Errorf("decode telemetry v2 schema: %w", err)
	}
	if schema.ID != manifest.SchemaID || schema.Policy.ContractVersion != manifest.ContractVersion {
		return fmt.Errorf("telemetry v2 schema metadata does not match manifest")
	}
	fixtures, err := GoldenFixtures()
	if err != nil {
		return err
	}
	for _, fixture := range fixtures {
		if err := validateFixture(fixture, manifest.SchemaVersion); err != nil {
			return err
		}
	}
	return nil
}

func validateFixture(fixture Fixture, schemaVersion int) error {
	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		TraceID       string          `json:"trace_id"`
		SpanID        string          `json:"span_id"`
		Kind          string          `json:"kind"`
		Semantic      json.RawMessage `json:"semantic"`
	}
	if err := json.Unmarshal(fixture.JSON, &envelope); err != nil {
		return fmt.Errorf("decode telemetry v2 golden fixture %q: %w", fixture.Name, err)
	}
	if envelope.SchemaVersion != schemaVersion {
		return fmt.Errorf("telemetry v2 golden fixture %q has schema_version %d", fixture.Name, envelope.SchemaVersion)
	}
	if !validLowerHex(envelope.TraceID, 32) || !validLowerHex(envelope.SpanID, 16) {
		return fmt.Errorf("telemetry v2 golden fixture %q has invalid trace or span ID", fixture.Name)
	}
	if envelope.Kind == "" || len(envelope.Semantic) == 0 || string(envelope.Semantic) == "null" {
		return fmt.Errorf("telemetry v2 golden fixture %q is missing kind or semantic data", fixture.Name)
	}
	var semantic struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(envelope.Semantic, &semantic); err != nil {
		return fmt.Errorf("decode telemetry v2 golden fixture %q semantic data: %w", fixture.Name, err)
	}
	if semantic.Kind != envelope.Kind {
		return fmt.Errorf("telemetry v2 golden fixture %q semantic kind %q differs from envelope kind %q", fixture.Name, semantic.Kind, envelope.Kind)
	}
	return nil
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if char < '0' || (char > '9' && char < 'a') || char > 'f' {
			return false
		}
	}
	return true
}
