package main

import "testing"

func TestLocalDoctorUsesIsolatedSyntheticCapture(t *testing.T) {
	t.Setenv("NEATLOGS_API_KEY", "")
	t.Setenv("NEATLOGS_ENDPOINT", "http://127.0.0.1:1")
	if exit := run([]string{"doctor", "--local", "--json"}); exit != 0 {
		t.Fatalf("local exit = %d, want 0", exit)
	}
}

func TestInvalidDoctorInvocationExitsFour(t *testing.T) {
	if exit := run([]string{"doctor", "--local", "--probe"}); exit != 4 {
		t.Fatalf("invalid exit = %d, want 4", exit)
	}
}
