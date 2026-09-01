package neatlogs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"

	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

const uploadUnavailableReason = "uploads_disabled"

type mediaUploadCandidate struct {
	prefix  string
	content []byte
}

// uploadTypedMedia runs only after normalization and the caller's mask. Its
// private payload attributes are removed on every path, including disabled and
// malformed paths, so raw media cannot leak into ordinary OTLP.
func uploadTypedMedia(ctx context.Context, stub *spanStub, authority uploadAuthority, diagnostics *deliveryDiagnostics) {
	if stub == nil {
		return
	}
	candidates := make([]mediaUploadCandidate, 0)
	attributes := make([]attribute.KeyValue, 0, len(stub.Attributes))
	for _, value := range stub.Attributes {
		prefix, private := internalmedia.ReferencePrefix(string(value.Key))
		if !private {
			attributes = append(attributes, value)
			continue
		}
		if value.Value.Type() == attribute.BYTESLICE {
			candidates = append(candidates, mediaUploadCandidate{prefix: prefix, content: value.Value.AsByteSlice()})
		}
	}
	stub.Attributes = attributes
	for index := range stub.Events {
		stub.Events[index].Attributes = stripPrivateMedia(stub.Events[index].Attributes)
	}
	if stub.Resource != nil {
		resourceAttributes := stripPrivateMedia(stub.Resource.Attributes())
		stub.Resource = resource.NewWithAttributes(stub.Resource.SchemaURL(), resourceAttributes...)
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		seen[candidate.prefix] = struct{}{}
		if stringAttribute(stub.Attributes, candidate.prefix+"state") != "pending-upload" {
			continue
		}
		if len(candidate.content) == 0 {
			markMediaUploadFailed(stub, candidate.prefix, "empty_payload", false, diagnostics)
			continue
		}
		digest := sha256.Sum256(candidate.content)
		digestHex := hex.EncodeToString(digest[:])
		stub.Attributes = setStringAttribute(stub.Attributes, candidate.prefix+"sha256", digestHex)
		stub.Attributes = setInt64Attribute(stub.Attributes, candidate.prefix+"byte_length", int64(len(candidate.content)))
		mimeType := strings.TrimSpace(stringAttribute(stub.Attributes, candidate.prefix+"mime_type"))
		if mimeType == "" {
			mimeType = "application/octet-stream"
			stub.Attributes = setStringAttribute(stub.Attributes, candidate.prefix+"mime_type", mimeType)
		}
		if authority == nil {
			markMediaUploadFailed(stub, candidate.prefix, uploadUnavailableReason, false, diagnostics)
			continue
		}
		payload := uploadPayload{
			Content: candidate.content, Purpose: uploadPurposeTypedMedia, MIMEType: mimeType,
			ContentEncoding: uploadEncodingIdentity, PayloadSchema: uploadSchemaMediaV1,
			IdempotencyKey: uploadIdempotencyKey(
				uploadPurposeTypedMedia, mimeType, uploadEncodingIdentity, uploadSchemaMediaV1, digestHex,
			),
		}
		receipt, err := authority.Upload(ctx, payload)
		if err != nil {
			reason, retryable := uploadFailureDetails(err)
			markMediaUploadFailed(stub, candidate.prefix, reason, retryable, diagnostics)
			if diagnostics != nil {
				diagnostics.recordUploadFailure(err)
			}
			continue
		}
		if !uploadReceiptReady(receipt) {
			markMediaUploadFailed(stub, candidate.prefix, "invalid_receipt", false, diagnostics)
			continue
		}
		stub.Attributes = setStringAttribute(stub.Attributes, candidate.prefix+"id", receipt.Reference.ID)
		stub.Attributes = setStringAttribute(stub.Attributes, candidate.prefix+"source", "uploaded")
		stub.Attributes = setStringAttribute(stub.Attributes, candidate.prefix+"state", "available")
		stub.Attributes = removeAttribute(stub.Attributes, candidate.prefix+"safe_preview")
		if diagnostics != nil {
			diagnostics.typedMediaUploads.Add(1)
		}
	}

	// A pending reference whose private payload was lost to an attribute limit
	// or removed by a mask is explicit failure metadata, never a silent hash.
	for _, prefix := range pendingMediaPrefixes(stub.Attributes) {
		if _, ok := seen[prefix]; !ok {
			markMediaUploadFailed(stub, prefix, "payload_unavailable", false, diagnostics)
		}
	}
}

func stripPrivateMedia(values []attribute.KeyValue) []attribute.KeyValue {
	cleaned := make([]attribute.KeyValue, 0, len(values))
	for _, value := range values {
		if _, private := internalmedia.ReferencePrefix(string(value.Key)); !private {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}

func pendingMediaPrefixes(values []attribute.KeyValue) []string {
	prefixes := make([]string, 0)
	for _, value := range values {
		key := string(value.Key)
		if !strings.HasSuffix(key, ".state") || value.Value.Type() != attribute.STRING || value.Value.AsString() != "pending-upload" {
			continue
		}
		prefix := strings.TrimSuffix(key, "state")
		if stringAttribute(values, prefix+"sha256") != "" && stringAttribute(values, prefix+"mime_type") != "" {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

func markMediaUploadFailed(stub *spanStub, prefix, reason string, retryable bool, diagnostics *deliveryDiagnostics) {
	stub.Attributes = setStringAttribute(stub.Attributes, prefix+"state", "failed")
	stub.Attributes = setStringAttribute(stub.Attributes, prefix+"safe_preview", "upload failed: "+reason)
	if diagnostics != nil {
		diagnostics.typedMediaUploadFailures.Add(1)
		diagnostics.recordUploadFailure(&uploadFailure{stage: "typed_media", reasonCode: reason, retryable: retryable})
	}
}

func uploadFailureDetails(err error) (string, bool) {
	var failure *uploadFailure
	if errors.As(err, &failure) {
		return failure.reasonCode, failure.retryable
	}
	return "unknown", false
}

func stringAttribute(values []attribute.KeyValue, key string) string {
	for _, value := range values {
		if string(value.Key) == key && value.Value.Type() == attribute.STRING {
			return value.Value.AsString()
		}
	}
	return ""
}

func setStringAttribute(values []attribute.KeyValue, key, replacement string) []attribute.KeyValue {
	return setAttribute(values, key, attribute.String(key, replacement))
}

func setInt64Attribute(values []attribute.KeyValue, key string, replacement int64) []attribute.KeyValue {
	return setAttribute(values, key, attribute.Int64(key, replacement))
}

func setAttribute(values []attribute.KeyValue, key string, replacement attribute.KeyValue) []attribute.KeyValue {
	for index := range values {
		if string(values[index].Key) == key {
			values[index] = replacement
			return values
		}
	}
	return append(values, replacement)
}

func removeAttribute(values []attribute.KeyValue, key string) []attribute.KeyValue {
	cleaned := values[:0]
	for _, value := range values {
		if string(value.Key) != key {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
