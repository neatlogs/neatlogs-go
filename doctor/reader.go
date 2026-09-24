package doctor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// ReadSpans reads a JSONL span log into a slice of span maps.
// Returns (spans, invalid_line_numbers).
//
// On a missing path, it emits a file-not-found finding and returns ([], []).
func ReadSpans(path string, findings *[]DoctorFinding) ([]Span, []int) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			*findings = append(*findings, DoctorFinding{
				Severity:   "error",
				Code:       "file-not-found",
				Title:      "Span log file not found",
				Evidence:   path,
				Suggestion: "Pass the processed span log path from NEATLOGS_LOG_SPANS_FILE.",
			})
			return nil, nil
		}
		// Other stat error: treat as missing/unreadable and report it.
		*findings = append(*findings, DoctorFinding{
			Severity:   "error",
			Code:       "file-not-found",
			Title:      "Span log file could not be opened",
			Evidence:   fmt.Sprintf("%s: %v", path, err),
			Suggestion: "Verify the path is readable and points to a valid file.",
		})
		return nil, nil
	}

	f, err := os.Open(path)
	if err != nil {
		*findings = append(*findings, DoctorFinding{
			Severity:   "error",
			Code:       "file-not-found",
			Title:      "Span log file could not be opened",
			Evidence:   fmt.Sprintf("%s: %v", path, err),
			Suggestion: "Verify the path is readable and points to a valid file.",
		})
		return nil, nil
	}
	defer f.Close()

	spans, invalid := readSpansFromReader(f)
	return spans, invalid
}

func readSpansFromReader(r io.Reader) ([]Span, []int) {
	var spans []Span
	var invalid []int
	scanner := bufio.NewScanner(r)
	// Allow long lines (some large spans serialize to >64KB JSON).
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := scanner.Bytes()
		// Skip empty/whitespace lines.
		trimmed := trimSpaces(line)
		if len(trimmed) == 0 {
			continue
		}
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			invalid = append(invalid, lineNum)
			continue
		}
		if m, ok := v.(map[string]any); ok {
			spans = append(spans, Span(m))
		} else {
			// Non-dict JSON line — record but don't fail.
			invalid = append(invalid, lineNum)
		}
	}
	return spans, invalid
}

func trimSpaces(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
