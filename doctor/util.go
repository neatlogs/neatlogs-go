package doctor

import (
	"strings"
)

// Truncate returns a string representation of v, truncated to maxLen chars
// with a "..." suffix when over the limit. Non-strings are coerced via fmt.Sprint.
func Truncate(v any, maxLen int) string {
	s := toString(v)
	if len(s) > maxLen {
		if maxLen < 3 {
			return s[:maxLen]
		}
		return s[:maxLen-3] + "..."
	}
	return s
}

// truncate is a convenience wrapper with the default max length.
func truncate(v any) string {
	return Truncate(v, MaxEvidenceLen)
}

// SpanKind returns the lowercase trimmed kind for a span, looking at
// both the top-level "kind" field and attributes["neatlogs.span.kind"].
func SpanKind(s Span) string {
	if s == nil {
		return ""
	}
	if v, ok := s["kind"]; ok {
		if t := strings.ToLower(strings.TrimSpace(toString(v))); t != "" {
			return t
		}
	}
	if attrs := Attributes(s); attrs != nil {
		if v, ok := attrs["neatlogs.span.kind"]; ok {
			if t := strings.ToLower(strings.TrimSpace(toString(v))); t != "" {
				return t
			}
		}
	}
	return ""
}

// IsInternal reports whether a span is internal (neatlogs-internal) and
// therefore excluded from per-trace checks. The two criteria are OR:
//   - the attributes["neatlogs.internal"] is truthy, OR
//   - the span name is exactly "neatlogs.trace.complete".
func IsInternal(s Span) bool {
	if s == nil {
		return false
	}
	if attrs := Attributes(s); attrs != nil {
		if v, ok := attrs["neatlogs.internal"]; ok && isTruthy(v) {
			return true
		}
	}
	name, _ := s["name"].(string)
	return name == "neatlogs.trace.complete"
}

// IsNeatlogsScope reports whether a scope name belongs to the Neatlogs
// SDK or one of its sub-scopes. Anything else is foreign instrumentation.
func IsNeatlogsScope(scope string) bool {
	return scope == NeatlogsScopePrefix || strings.HasPrefix(scope, NeatlogsScopePrefix+".")
}

// SpanStatusIsError returns true if a span's status indicates an error.
// Tolerant of two formats:
//   - neatlogs-normalized: {"code": "ERROR", "description": "..."}
//   - OTel SDK canonical:   {"status_code": {"name": "ERROR", "value": 2}}
func SpanStatusIsError(status any) bool {
	m, ok := status.(map[string]any)
	if !ok {
		return false
	}
	if code, ok := m["code"]; ok {
		if s := strings.ToUpper(strings.TrimSpace(toString(code))); s == "ERROR" || s == "ERROR_STATUS" {
			return true
		}
	}
	if sc, ok := m["status_code"]; ok {
		if scMap, ok := sc.(map[string]any); ok {
			if name, ok := scMap["name"]; ok {
				if s := strings.ToUpper(strings.TrimSpace(toString(name))); s == "ERROR" || s == "ERROR_STATUS" {
					return true
				}
			}
		}
		if s := strings.ToUpper(strings.TrimSpace(toString(sc))); s == "ERROR" || s == "ERROR_STATUS" {
			return true
		}
	}
	return false
}

// Attributes returns the attributes map for a span, or nil if absent or
// not a map.
func Attributes(s Span) map[string]any {
	if s == nil {
		return nil
	}
	if attrs, ok := s["attributes"].(map[string]any); ok {
		return attrs
	}
	return nil
}

// HasExceptionEvent reports whether the span's events list contains a
// dict with name == "exception".
func HasExceptionEvent(span Span) bool {
	events, _ := span["events"].([]any)
	for _, e := range events {
		if em, ok := e.(map[string]any); ok {
			if n, _ := em["name"].(string); n == "exception" {
				return true
			}
		}
	}
	return false
}

// SpanName returns the span's name or "<unnamed>" when absent.
func SpanName(s Span) string {
	if s == nil {
		return "<unnamed>"
	}
	if n, ok := s["name"].(string); ok && n != "" {
		return n
	}
	return "<unnamed>"
}

// SpanID returns the span's span_id or "".
func SpanID(s Span) string {
	if s == nil {
		return ""
	}
	if id, ok := s["span_id"].(string); ok {
		return id
	}
	return ""
}

// ParentSpanID returns the span's parent_span_id, or "" when the span
// is a root. The reference treats nil and missing identically.
func ParentSpanID(s Span) string {
	if s == nil {
		return ""
	}
	if v, ok := s["parent_span_id"]; ok && v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// TraceID returns the span's trace_id or "".
func TraceID(s Span) string {
	if s == nil {
		return ""
	}
	if id, ok := s["trace_id"].(string); ok {
		return id
	}
	return ""
}

// SessionID returns the span's attributes["session.id"] or "".
func SessionID(s Span) string {
	attrs := Attributes(s)
	if attrs == nil {
		return ""
	}
	if v, ok := attrs["session.id"].(string); ok {
		return v
	}
	return ""
}

// InstrumentationScopeName returns the scope name from the top-level
// instrumentation_scope field, accepting both dict and string forms.
func InstrumentationScopeName(s Span) string {
	if s == nil {
		return ""
	}
	scope := s["instrumentation_scope"]
	switch v := scope.(type) {
	case map[string]any:
		if n, ok := v["name"].(string); ok {
			return n
		}
	case string:
		return v
	}
	return ""
}

// isTruthy returns true for non-nil, non-empty, non-false values.
func isTruthy(v any) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return true
}

// toString coerces a value to a string for truncation/display.
func toString(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case bool:
		if s {
			return "true"
		}
		return "false"
	case float64:
		return floatToString(s)
	case int:
		return intToString(int64(s))
	case int64:
		return intToString(s)
	}
	return ""
}

// floatToString and intToString avoid the strconv dependency in the
// hot path of truncation. They produce clean decimal output that matches
// what JSON would emit.
func floatToString(f float64) string {
	// Format without trailing zeros for whole numbers.
	if f == float64(int64(f)) {
		return intToString(int64(f))
	}
	// Fall back: use Go's default formatting via a small buffer.
	return trimFloat(f)
}

func intToString(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func trimFloat(f float64) string {
	// Use a quick path: %.6f trimmed of trailing zeros.
	// Avoid strconv to keep this file dependency-free.
	whole := int64(f)
	frac := f - float64(whole)
	if frac < 0 {
		frac = -frac
	}
	s := intToString(whole)
	// Render up to 6 fractional digits, drop trailing zeros.
	var buf [10]byte
	bi := 0
	for i := 0; i < 6; i++ {
		frac *= 10
		d := int64(frac)
		if d > 9 {
			d = 9
		}
		buf[bi] = byte('0' + d)
		bi++
		frac -= float64(d)
	}
	for bi > 0 && buf[bi-1] == '0' {
		bi--
	}
	if bi == 0 {
		return s
	}
	return s + "." + string(buf[:bi])
}
