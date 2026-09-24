package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// FormatReport renders a DoctorReport as a human-readable text block.
func FormatReport(r DoctorReport) string {
	var lines []string
	lines = append(lines, "Trace Doctor")
	lines = append(lines, fmt.Sprintf("File: %s", r.Path))
	lines = append(lines, fmt.Sprintf("Spans: %d", r.SpansRead))
	lines = append(lines, fmt.Sprintf("Traces: %d", r.TraceCount))
	lines = append(lines, fmt.Sprintf("Runs: %d", r.RunCount))

	if len(r.Findings) == 0 {
		lines = append(lines, "")
		lines = append(lines, "No problems found.")
		return strings.Join(lines, "\n")
	}

	lines = append(lines, "")
	lines = append(lines, "Findings:")
	for idx, f := range r.Findings {
		var locParts []string
		if f.TraceID != "" {
			locParts = append(locParts, "trace="+f.TraceID)
		}
		if f.RunID != "" && f.RunID != DefaultSessionID {
			locParts = append(locParts, "run="+f.RunID)
		}
		loc := ""
		if len(locParts) > 0 {
			loc = " " + strings.Join(locParts, " ")
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s%s", idx+1, f.Severity, f.Title, loc))
		lines = append(lines, fmt.Sprintf("   Evidence: %s", f.Evidence))
		lines = append(lines, fmt.Sprintf("   Fix: %s", f.Suggestion))
	}
	return strings.Join(lines, "\n")
}

// WriteJSON serializes a DoctorReport as indented JSON and writes it to w.
// Adds a trailing newline. Output matches the Python reference
// byte-for-byte: HTML-escaping is disabled (so "<id>" is emitted as-is),
// and non-ASCII characters are escaped as \uXXXX (matching Python's
// default ensure_ascii=True).
func WriteJSON(w io.Writer, r DoctorReport) error {
	enc := json.NewEncoder(asciiEscapeWriter{w: w})
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return err
	}
	return nil
}

// asciiEscapeWriter wraps an io.Writer and escapes any non-ASCII bytes
// (>= 0x80) as \uXXXX. This matches the Python reference's default
// json.dump(..., ensure_ascii=True) behavior. JSON-level escapes (e.g.,
// \" for embedded quotes) are produced by encoding/json before reaching
// us, so we only have to handle UTF-8 sequences.
type asciiEscapeWriter struct{ w io.Writer }

func (a asciiEscapeWriter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p))
	i := 0
	for i < len(p) {
		b := p[i]
		if b < 0x80 {
			out = append(out, b)
			i++
			continue
		}
		// Decode the rune at p[i] and emit \uXXXX (or a surrogate pair).
		r, size := decodeRune(p[i:])
		if size == 0 {
			r = 0xFFFD
			size = 1
		}
		out = appendAsciiEscape(out, r)
		i += size
	}
	if _, err := a.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func decodeRune(p []byte) (rune, int) {
	if len(p) == 0 {
		return 0, 0
	}
	b0 := p[0]
	switch {
	case b0 < 0x80:
		return rune(b0), 1
	case b0 < 0xC0:
		return 0xFFFD, 1
	case b0 < 0xE0:
		if len(p) < 2 || p[1]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(b0&0x1F)<<6 | rune(p[1]&0x3F), 2
	case b0 < 0xF0:
		if len(p) < 3 || p[1]&0xC0 != 0x80 || p[2]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(b0&0x0F)<<12 | rune(p[1]&0x3F)<<6 | rune(p[2]&0x3F), 3
	default:
		if len(p) < 4 || p[1]&0xC0 != 0x80 || p[2]&0xC0 != 0x80 || p[3]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(b0&0x07)<<18 | rune(p[1]&0x3F)<<12 | rune(p[2]&0x3F)<<6 | rune(p[3]&0x3F), 4
	}
}

func appendAsciiEscape(buf []byte, r rune) []byte {
	if r <= 0xFFFF {
		buf = append(buf, '\\', 'u')
		var hex [4]byte
		hex[0] = hexDigit(byte(r >> 12) & 0xF)
		hex[1] = hexDigit(byte(r >> 8) & 0xF)
		hex[2] = hexDigit(byte(r >> 4) & 0xF)
		hex[3] = hexDigit(byte(r) & 0xF)
		buf = append(buf, hex[:]...)
	} else {
		// Surrogate pair for r > 0xFFFF (r is in 0x10000..0x10FFFF)
		r1 := 0xD800 + ((r - 0x10000) >> 10)
		r2 := 0xDC00 + ((r - 0x10000) & 0x3FF)
		buf = appendAsciiEscape(buf, r1)
		buf = appendAsciiEscape(buf, r2)
	}
	return buf
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + (n - 10)
}
