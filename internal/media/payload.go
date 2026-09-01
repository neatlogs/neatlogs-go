// Package media defines the private handoff used by supported integrations to
// carry large typed media to the final, post-mask exporter boundary.
package media

import (
	"encoding/base64"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

const (
	// InlineLimit is the largest typed media value retained inline. Larger
	// values use the authenticated upload authority when it is enabled.
	InlineLimit = 100_000
	UploadLimit = 25 * 1024 * 1024

	// PayloadPrefix is private transport state. The exporter always removes it
	// before delegating or serializing OTLP.
	PayloadPrefix = "neatlogs.internal.media_payload."
)

func PayloadAttribute(referencePrefix string, content []byte) attribute.KeyValue {
	key := PayloadPrefix + base64.RawURLEncoding.EncodeToString([]byte(referencePrefix))
	return attribute.Key(key).ByteSlice(content)
}

func ReferencePrefix(key string) (string, bool) {
	encoded, ok := strings.CutPrefix(key, PayloadPrefix)
	if !ok || encoded == "" {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}
