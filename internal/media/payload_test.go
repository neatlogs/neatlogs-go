package media

import (
	"bytes"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func testSpanContext(id byte) trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{id}, SpanID: trace.SpanID{id}, TraceFlags: trace.FlagsSampled,
	})
}

func TestStoreBoundsDeduplicatesAndReleasesPerSpan(t *testing.T) {
	store := NewStore(5, 2)
	defer store.Close()
	first, second, third, fourth := testSpanContext(1), testSpanContext(2), testSpanContext(3), testSpanContext(4)
	for _, span := range []trace.SpanContext{first, second, third, fourth} {
		RegisterSpan(span, store)
	}
	original := []byte("abc")
	one, reason := Stage(first, original, "image/png")
	if reason != "" || one.Token == "" || one.SHA256 == "" || one.ByteLength != 3 || one.Content != nil {
		t.Fatalf("first stage = %#v, %q", one, reason)
	}
	original[0] = 'z'
	two, reason := Stage(second, []byte("abc"), "image/png")
	if reason != "" || two.Token != one.Token {
		t.Fatalf("deduplicated stage = %#v, %q", two, reason)
	}
	if _, reason = Stage(third, []byte("de"), "image/png"); reason != "" {
		t.Fatalf("second unique stage: %s", reason)
	}
	if _, reason = Stage(fourth, []byte("x"), "image/png"); reason != "pending_media_memory_limit" {
		t.Fatalf("overflow reason = %q", reason)
	}
	if items, retained := store.Snapshot(); items != 2 || retained != 5 {
		t.Fatalf("bounded snapshot = %d/%d", items, retained)
	}

	lease := AcquireSpan(first)
	payload, ok := lease.Payload(one.Token)
	if !ok || !bytes.Equal(payload.Content, []byte("abc")) {
		t.Fatalf("leased immutable payload = %#v", payload)
	}
	lease.Release()
	if items, retained := store.Snapshot(); items != 2 || retained != 5 {
		t.Fatalf("shared item released too early = %d/%d", items, retained)
	}
	DiscardSpan(second)
	if items, retained := store.Snapshot(); items != 1 || retained != 2 {
		t.Fatalf("deduplicated item not released = %d/%d", items, retained)
	}
	DiscardSpan(third)
}

func TestStoreItemLimitAndShutdownRelease(t *testing.T) {
	store := NewStore(100, 1)
	first, second := testSpanContext(11), testSpanContext(12)
	RegisterSpan(first, store)
	RegisterSpan(second, store)
	if _, reason := Stage(first, []byte("one"), "image/png"); reason != "" {
		t.Fatal(reason)
	}
	if _, reason := Stage(second, []byte("two"), "image/png"); reason != "pending_media_item_limit" {
		t.Fatalf("item overflow reason = %q", reason)
	}
	store.Close()
	if items, retained := store.Snapshot(); items != 0 || retained != 0 {
		t.Fatalf("closed snapshot = %d/%d", items, retained)
	}
	if _, reason := Stage(first, []byte("after-close"), "image/png"); reason != "uploads_disabled" {
		t.Fatalf("post-close stage reason = %q", reason)
	}
}

func TestUnregisteredSpanDoesNotStageMedia(t *testing.T) {
	if _, reason := Stage(testSpanContext(21), make([]byte, UploadLimit), "image/png"); reason != "uploads_disabled" {
		t.Fatalf("disabled stage reason = %q", reason)
	}
}
