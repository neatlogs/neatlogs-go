package neatlogs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var errAgentCanonical = errors.New("neatlogs: unsupported agent canonical identity")

func agentJSONString(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 32 {
				fmt.Fprintf(&out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

func agentJSONNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	parts := strings.Split(strconv.FormatFloat(value, 'e', -1, 64), "e")
	exponent, _ := strconv.Atoi(parts[1])
	digits := strings.ReplaceAll(parts[0], ".", "")
	if exponent >= -6 && exponent < 21 {
		point := exponent + 1
		if point <= 0 {
			return sign + "0." + strings.Repeat("0", -point) + digits
		}
		if point >= len(digits) {
			return sign + digits + strings.Repeat("0", point-len(digits))
		}
		return sign + digits[:point] + "." + digits[point:]
	}
	suffix := strconv.Itoa(exponent)
	if exponent >= 0 {
		suffix = "+" + suffix
	}
	return sign + parts[0] + "e" + suffix
}

// Agent identities contain flat scalar maps, not arbitrary application JSON.
func agentScalarObject(values map[string]any, normalize bool) (string, error) {
	if len(values) > 20 {
		return "", errAgentCanonical
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if !utf8.ValidString(key) {
			return "", errAgentCanonical
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	items := make([]string, 0, len(keys))
	for _, key := range keys {
		value := values[key]
		if number, ok := value.(json.Number); ok {
			n, err := number.Float64()
			if err != nil {
				return "", errAgentCanonical
			}
			if exact, ok := new(big.Rat).SetString(string(number)); !ok || exact.IsInt() && exact.Cmp(new(big.Rat).SetFloat64(n)) != 0 {
				return "", errAgentCanonical
			}
			value = n
		}
		var encoded string
		switch v := value.(type) {
		case nil:
			encoded = "null"
		case bool:
			encoded = strconv.FormatBool(v)
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return "", errAgentCanonical
			}
			encoded = agentJSONNumber(v)
		case string:
			if !utf8.ValidString(v) {
				return "", errAgentCanonical
			}
			if normalize {
				for _, r := range v {
					if !(unicode.IsGraphic(r) || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r)) {
						return "", errAgentCanonical
					}
				}
				v = norm.NFC.String(v)
			}
			encoded = agentJSONString(v)
		default:
			return "", errAgentCanonical
		}
		items = append(items, agentJSONString(key)+":"+encoded)
	}
	return "{" + strings.Join(items, ",") + "}", nil
}
func agentIdentityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func agentVersionFingerprint(key string, params ABParams) (string, error) {
	if !agentValidParams(params) {
		return "", errAgentCanonical
	}
	encoded, err := agentScalarObject(params, true)
	if err != nil {
		return "", err
	}
	return agentIdentityHash(`{"agentKey":` + agentJSONString(key) + `,"params":` + encoded + `,"version":1}`), nil
}
func agentAttributesDigest(attributes map[string]any, keys []string) (string, error) {
	selected := attributes
	if keys != nil {
		selected = map[string]any{}
		for _, key := range keys {
			if value, exists := attributes[key]; exists {
				selected[key] = value
			}
		}
	}
	encoded, err := agentScalarObject(selected, false)
	if err != nil {
		return "", err
	}
	return agentIdentityHash(`{"attributes":` + encoded + `,"version":1}`), nil
}

// Go's JSON decoder replaces lone UTF-16 surrogates; enhanced identities reject them first.
func agentWireUnicode(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	quoted := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
