//go:build !adk_legacy

// Command adk explains why the former Google ADK passthrough example is no
// longer runnable with the isolated Neatlogs SDK.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(
		os.Stderr,
		"Google ADK passive passthrough is unsupported: ADK uses the global OpenTelemetry provider while Neatlogs uses a private provider for isolation. See README.md.",
	)
	os.Exit(1)
}
