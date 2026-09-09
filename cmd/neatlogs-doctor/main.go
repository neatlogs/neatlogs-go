// Command neatlogs-doctor is a local, read-only linter for Neatlogs
// processed span logs. It runs as a CLI with --json, --run-id,
// --foreign-only, --read-prompt-content, and --emit-fix flags.
// See the doctor package for the diagnostic logic.
//
// Usage:
//
//	neatlogs-doctor ./spans.log
//	neatlogs-doctor ./spans.log --json
//	neatlogs-doctor ./spans.log --run-id abc123
//	neatlogs-doctor ./spans.log --foreign-only
//	neatlogs-doctor ./spans.log --read-prompt-content
//	neatlogs-doctor --emit-fix init-after-client
//
// Exit code: 0 if no error-severity findings (or --emit-fix success),
// 1 if there are error findings, 2 for argument errors.
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
	// --emit-fix is special: it's a "self-contained" command that
	// does NOT read the log file. We handle it before splitArgs so
	// that the path requirement is bypassed when --emit-fix is set.
	var emitFix string
	var filtered []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--emit-fix" {
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "-") {
				fmt.Fprintln(stderr, "neatlogs-doctor: --emit-fix requires a value")
				return 2
			}
			emitFix = argv[i+1]
			i++ // skip the value
			continue
		}
		if strings.HasPrefix(arg, "--emit-fix=") {
			emitFix = strings.TrimPrefix(arg, "--emit-fix=")
			continue
		}
		filtered = append(filtered, arg)
	}
	if emitFix != "" {
		snippet := doctor.RenderFixSnippet(emitFix)
		if snippet == "" {
			fmt.Fprintf(stderr, "Unknown finding code: '%s'. Known codes: %s\n", emitFix, strings.Join(doctor.FixSnippetCodes(), ", "))
			return 2
		}
		_, _ = io.WriteString(stdout, snippet)
		return 0
	}

	// Pre-scan: separate the first positional arg from the rest, then
	// pass only the flag-shaped pieces to flag.Parse.
	flags, path, err := splitArgs(filtered)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if path == "" {
		fmt.Fprintln(stderr, "usage: neatlogs-doctor <path> [--json] [--run-id ID] [--foreign-only] [--read-prompt-content]")
		fmt.Fprintln(stderr, "       neatlogs-doctor --emit-fix <code>")
		return 2
	}

	fs := flag.NewFlagSet("neatlogs-doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Print a JSON report instead of text.")
	runID := fs.String("run-id", "", "Only analyze spans belonging to this run (session.id or trace_id).")
	foreignOnly := fs.Bool("foreign-only", false, "Only show foreign-instrumentation findings.")
	readPromptContent := fs.Bool("read-prompt-content", false,
		"Read LLM prompt contents to detect the 'repeated-system-prompt' pattern. "+
			"PII concern: the prompt may contain user data. Default is off.")
	if err := fs.Parse(flags); err != nil {
		return 2
	}

	report := doctor.Diagnose(path, doctor.Options{
		RunID:             *runID,
		ForeignOnly:       *foreignOnly,
		ReadPromptContent: *readPromptContent,
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
