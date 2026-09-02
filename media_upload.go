package neatlogs

import (
	"context"
	"errors"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"

	internalmedia "github.com/neatlogs/neatlogs-go/internal/media"
)

const uploadUnavailableReason = "uploads_disabled"

type mediaUploadTarget struct {
	values *[]attribute.KeyValue
	prefix string
}

type mediaUploadCandidate struct {
	payload internalmedia.PendingPayload
	targets []mediaUploadTarget
}

// uploadTypedMedia runs only after normalization and the caller's mask. Raw
// media remains in the out-of-band lease; the accepted masked snapshot contains
// only opaque tokens, which are removed on every path before OTLP delegation.
func uploadTypedMedia(ctx context.Context, stub *spanStub, authority uploadAuthority, diagnostics *deliveryDiagnostics) bool {
	if stub == nil {
		return true
	}
	lease := internalmedia.AcquireSpan(stub.SpanContext)
	defer lease.Release()

	attributeSets := []*[]attribute.KeyValue{&stub.Attributes}
	for index := range stub.Events {
		attributeSets = append(attributeSets, &stub.Events[index].Attributes)
	}
	var resourceAttributes []attribute.KeyValue
	if stub.Resource != nil {
		resourceAttributes = append([]attribute.KeyValue(nil), stub.Resource.Attributes()...)
		attributeSets = append(attributeSets, &resourceAttributes)
	}

	candidates := make(map[string]*mediaUploadCandidate)
	failed := false
	for _, values := range attributeSets {
		tokens := detachMediaTokens(values)
		preFailed := make(map[string]struct{})
		for _, prefix := range failedMediaPrefixes(*values) {
			preFailed[prefix] = struct{}{}
			if diagnostics != nil {
				diagnostics.typedMediaUploadFailures.Add(1)
				diagnostics.recordUploadFailure(&uploadFailure{
					stage: "typed_media", reasonCode: mediaFailureReason(*values, prefix), retryable: false,
				})
			}
		}
		for prefix := range tokens {
			if stringAttribute(*values, prefix+"state") == "pending-upload" && canonicalMediaReference(*values, prefix) {
				continue
			}
			if _, alreadyFailed := preFailed[prefix]; !alreadyFailed {
				markMediaUploadFailed(values, prefix, "masked_pending_state", false, diagnostics)
			}
			failed = true
			delete(tokens, prefix)
		}
		for _, prefix := range pendingMediaPrefixes(*values) {
			token := tokens[prefix]
			if token == "" {
				markMediaUploadFailed(values, prefix, "upload_token_missing", false, diagnostics)
				failed = true
				continue
			}
			payload, ok := lease.Payload(token)
			if !ok {
				markMediaUploadFailed(values, prefix, "staged_payload_missing", false, diagnostics)
				failed = true
				continue
			}
			candidate := candidates[token]
			if candidate == nil {
				candidate = &mediaUploadCandidate{payload: payload}
				candidates[token] = candidate
			}
			candidate.targets = append(candidate.targets, mediaUploadTarget{values: values, prefix: prefix})
		}
	}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			reason := contextReason(ctx)
			for _, target := range candidate.targets {
				markMediaUploadFailed(target.values, target.prefix, reason, contextRetryable(ctx), diagnostics)
			}
			failed = true
			continue
		}
		if authority == nil {
			for _, target := range candidate.targets {
				markMediaUploadFailed(target.values, target.prefix, uploadUnavailableReason, false, diagnostics)
			}
			failed = true
			continue
		}
		payload := uploadPayload{
			Content: candidate.payload.Content, Purpose: uploadPurposeTypedMedia,
			MIMEType: candidate.payload.MIMEType, ContentEncoding: uploadEncodingIdentity,
			PayloadSchema: uploadSchemaMediaV1,
			IdempotencyKey: uploadIdempotencyKey(
				uploadPurposeTypedMedia, candidate.payload.MIMEType, uploadEncodingIdentity,
				uploadSchemaMediaV1, candidate.payload.SHA256,
			),
		}
		receipt, err := authority.Upload(ctx, payload)
		if err != nil {
			reason, retryable := uploadFailureDetails(err)
			for _, target := range candidate.targets {
				markMediaUploadFailed(target.values, target.prefix, reason, retryable, diagnostics)
			}
			failed = true
			continue
		}
		if !mediaUploadReceiptMatches(receipt, candidate.payload) {
			for _, target := range candidate.targets {
				markMediaUploadFailed(target.values, target.prefix, "invalid_receipt", false, diagnostics)
			}
			failed = true
			continue
		}
		for _, target := range candidate.targets {
			setMediaUploadAvailable(target, receipt.Reference)
		}
		if diagnostics != nil {
			diagnostics.typedMediaUploads.Add(uint64(len(candidate.targets)))
		}
	}

	if stub.Resource != nil {
		stub.Resource = resource.NewWithAttributes(stub.Resource.SchemaURL(), resourceAttributes...)
	}
	return !failed
}

func failedMediaPrefixes(values []attribute.KeyValue) []string {
	prefixes := make([]string, 0)
	for _, value := range values {
		key := string(value.Key)
		if !strings.HasSuffix(key, ".state") || value.Value.Type() != attribute.STRING || value.Value.AsString() != "failed" {
			continue
		}
		prefix := strings.TrimSuffix(key, "state")
		if canonicalMediaReference(values, prefix) &&
			strings.HasPrefix(stringAttribute(values, prefix+"safe_preview"), "upload failed:") {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

func mediaFailureReason(values []attribute.KeyValue, prefix string) string {
	reason := strings.TrimSpace(strings.TrimPrefix(stringAttribute(values, prefix+"safe_preview"), "upload failed:"))
	if !diagnosticCodePattern.MatchString(reason) {
		return "capture_failed"
	}
	return reason
}

func detachMediaTokens(values *[]attribute.KeyValue) map[string]string {
	tokens := make(map[string]string)
	cleaned := make([]attribute.KeyValue, 0, len(*values))
	for _, value := range *values {
		key := string(value.Key)
		if strings.HasSuffix(key, ".upload_token") {
			prefix := strings.TrimSuffix(key, "upload_token")
			if value.Value.Type() == attribute.STRING && strings.HasPrefix(value.Value.AsString(), internalmedia.UploadTokenPrefix) {
				tokens[prefix] = value.Value.AsString()
			}
			continue
		}
		cleaned = append(cleaned, value)
	}
	*values = cleaned
	return tokens
}

func pendingMediaPrefixes(values []attribute.KeyValue) []string {
	prefixes := make([]string, 0)
	for _, value := range values {
		key := string(value.Key)
		if !strings.HasSuffix(key, ".state") || value.Value.Type() != attribute.STRING || value.Value.AsString() != "pending-upload" {
			continue
		}
		prefix := strings.TrimSuffix(key, "state")
		if canonicalMediaReference(values, prefix) {
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

func canonicalMediaReference(values []attribute.KeyValue, prefix string) bool {
	return strings.HasPrefix(stringAttribute(values, prefix+"id"), "nl_media_")
}

func mediaUploadReceiptMatches(receipt uploadReceipt, payload internalmedia.PendingPayload) bool {
	return uploadReceiptReady(receipt) && receipt.Reference.Purpose == uploadPurposeTypedMedia &&
		receipt.Reference.SHA256 == payload.SHA256 && receipt.Reference.ByteLength == int64(payload.ByteLength) &&
		receipt.Reference.MIMEType == payload.MIMEType && receipt.Reference.ContentEncoding == uploadEncodingIdentity
}

func setMediaUploadAvailable(target mediaUploadTarget, reference uploadReference) {
	values := *target.values
	values = setStringAttribute(values, target.prefix+"id", reference.ID)
	values = setStringAttribute(values, target.prefix+"source", "uploaded")
	values = setStringAttribute(values, target.prefix+"sha256", reference.SHA256)
	values = setInt64Attribute(values, target.prefix+"byte_length", reference.ByteLength)
	values = setStringAttribute(values, target.prefix+"mime_type", reference.MIMEType)
	values = setStringAttribute(values, target.prefix+"state", "available")
	values = removeAttribute(values, target.prefix+"safe_preview")
	*target.values = values
}

func markMediaUploadFailed(values *[]attribute.KeyValue, prefix, reason string, retryable bool, diagnostics *deliveryDiagnostics) {
	updated := setStringAttribute(*values, prefix+"state", "failed")
	updated = setStringAttribute(updated, prefix+"safe_preview", "upload failed: "+reason)
	*values = updated
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
