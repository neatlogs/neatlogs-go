// Command neatlogs-doctor is a local, read-only linter for Neatlogs
// processed span logs. It runs as a CLI with --json, --run-id, and
// --foreign-only flags. See doctor/doctor.go for the diagnostic logic.
//
// Usage:
//
//	neatlogs-doctor ./spans.log
//	neatlogs-doctor ./spans.log --json
//	neatlogs-doctor ./spans.log --run-id abc123
//	neatlogs-doctor ./spans.log --foreign-only
//
// Exit code: 0 if no error-severity findings, 1 otherwise.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/neatlogs/neatlogs-go/doctor"
)

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

// runMain is the testable entry point. It accepts argv and streams for
// stdout/stderr, returning the process exit code.
//
// Unlike the Go stdlib flag package (which stops at the first positional
// arg), this CLI accepts flags after the path, matching Python argparse.
func runMain(argv []string, stdout, stderr io.Writer) int {
	// Pre-scan: separate the first positional arg from the rest, then
	// pass only the flag-shaped pieces to flag.Parse.
	flags, path, err := splitArgs(argv)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if path == "" {
		fmt.Fprintln(stderr, "usage: neatlogs-doctor <path> [--json] [--run-id ID] [--foreign-only]")
		return 2
	}

	fs := flag.NewFlagSet("neatlogs-doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Print a JSON report instead of text.")
	runID := fs.String("run-id", "", "Only analyze spans belonging to this run (session.id or trace_id).")
	foreignOnly := fs.Bool("foreign-only", false, "Only show foreign-instrumentation findings.")
	if err := fs.Parse(flags); err != nil {
		return 2
	}

	report := doctor.Diagnose(path, doctor.Options{
		RunID:       *runID,
		ForeignOnly: *foreignOnly,
	})

	if *asJSON {
		if err := doctor.WriteJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "neatlogs-doctor: write json: %v\n", err)
			return 2
		}
	} else {
		fmt.Fprintln(stdout, doctor.FormatReport(report))
	}
	if report.HasErrors() {
		return 1
	}
	return 0
}

// splitArgs separates the first non-flag positional arg from the rest of
// argv. The first non-flag arg is taken as the path; everything else is
// passed through to flag.Parse. We accept flags in any order:
//
//	neatlogs-doctor <path> [--json] [--run-id X] [--foreign-only]
//	neatlogs-doctor --json <path>
//	neatlogs-doctor <path> --json --run-id X
func splitArgs(argv []string) (flags []string, path string, err error) {
	for i, arg := range argv {
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			continue
		}
		// First non-flag: that's the path. Everything after (whether
		// flag or not) goes through to flag.Parse, so flags-after-path
		// still work.
		path = arg
		flags = append(flags, argv[i+1:]...)
		return flags, path, nil
	}
	return flags, "", nil
}
