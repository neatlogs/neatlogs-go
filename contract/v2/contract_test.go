package contractv2

import "testing"

func TestCanonicalTelemetryContract(t *testing.T) {
	t.Parallel()

	if err := Verify(); err != nil {
		t.Fatal(err)
	}

	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := schema["x-neatlogs-policy"].(map[string]any)
	if !ok {
		t.Fatal("canonical policy is missing")
	}
	if policy["contract_version"] != ContractVersion {
		t.Fatalf("contract version = %v, want %s", policy["contract_version"], ContractVersion)
	}

	precedence, ok := policy["conflict_precedence"].([]any)
	if !ok || len(precedence) != 7 {
		t.Fatalf("conflict precedence = %v, want seven frozen dialects", precedence)
	}
	want := []string{
		"native-v2",
		"neatlogs-direct",
		"otel-genai",
		"openinference",
		"provider-specific",
		"external-legacy",
		"unknown-raw",
	}
	for index, dialect := range want {
		if precedence[index] != dialect {
			t.Fatalf("precedence[%d] = %v, want %q", index, precedence[index], dialect)
		}
	}
}

func TestSchemaBytesReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()

	first := SchemaBytes()
	first[0] = 'x'
	second := SchemaBytes()
	if second[0] != '{' {
		t.Fatal("SchemaBytes exposed mutable embedded contract storage")
	}
}
