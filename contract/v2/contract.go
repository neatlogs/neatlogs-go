// Package contractv2 exposes the canonical NeatLogs telemetry contract v2.
package contractv2

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	ContractVersion = "2.0.0"
	SchemaVersion   = 2
	SchemaSHA256    = "9aec0e1b4e2fba718a1bad060798a881543c56ec8b887c6b0fb8ab147bbaee75"
)

//go:embed neatlogs-telemetry.schema.json
var schemaBytes []byte

//go:embed manifest.json
var manifestBytes []byte

// SchemaBytes returns a defensive copy of the exact public schema bytes.
func SchemaBytes() []byte {
	return append([]byte(nil), schemaBytes...)
}

// Schema decodes the canonical contract for normalizers and diagnostic tools.
func Schema() (map[string]any, error) {
	var schema map[string]any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		return nil, fmt.Errorf("decode NeatLogs telemetry schema v2: %w", err)
	}
	return schema, nil
}

// Verify proves that the embedded contract and its manifest remain in sync.
func Verify() error {
	var manifest struct {
		ContractVersion string `json:"contract_version"`
		SchemaVersion   int    `json:"schema_version"`
		SchemaSHA256    string `json:"schema_sha256"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("decode NeatLogs telemetry manifest v2: %w", err)
	}

	digest := sha256.Sum256(schemaBytes)
	actual := hex.EncodeToString(digest[:])
	if actual != SchemaSHA256 || actual != manifest.SchemaSHA256 {
		return fmt.Errorf("NeatLogs telemetry schema v2 digest mismatch: received %s", actual)
	}
	if manifest.ContractVersion != ContractVersion || manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("NeatLogs telemetry manifest v2 version mismatch")
	}
	return nil
}
