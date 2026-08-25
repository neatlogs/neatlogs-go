package telemetryv2

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
)

func TestValidateCanonicalContract(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaDigestMatchesAuthoritativeManifest(t *testing.T) {
	manifest, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if actual := hex.EncodeToString(digest[:]); actual != manifest.SchemaSHA256 {
		t.Fatalf("schema digest = %s, want %s", actual, manifest.SchemaSHA256)
	}
	if manifest.Authority != "https://github.com/neatlogs/skills/tree/main/contracts/v2" {
		t.Fatalf("unexpected contract authority %q", manifest.Authority)
	}
}

func TestGoldenFixturesAreStableAndConform(t *testing.T) {
	fixtures, err := GoldenFixtures()
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{
		"llm-tool-envelope.json",
		"reconciled-recovery-envelope.json",
		"recovered-root-envelope.json",
		"tool-execution-envelope.json",
		"unlinked-tool-envelope.json",
	}
	gotNames := make([]string, 0, len(fixtures))
	for _, fixture := range fixtures {
		gotNames = append(gotNames, fixture.Name)
		var envelope map[string]any
		if err := json.Unmarshal(fixture.JSON, &envelope); err != nil {
			t.Errorf("%s is invalid JSON: %v", fixture.Name, err)
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("golden fixtures = %v, want %v", gotNames, wantNames)
	}
}

func TestReturnedBytesAreDefensiveCopies(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	original := schema[0]
	schema[0] ^= 0xff
	again, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != original {
		t.Fatal("Schema returned mutable shared storage")
	}

	fixtures, err := GoldenFixtures()
	if err != nil {
		t.Fatal(err)
	}
	fixtureOriginal := fixtures[0].JSON[0]
	fixtures[0].JSON[0] ^= 0xff
	againFixtures, err := GoldenFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if againFixtures[0].JSON[0] != fixtureOriginal {
		t.Fatal("GoldenFixtures returned mutable shared storage")
	}
}
