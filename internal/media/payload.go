// Package media owns the private, bounded handoff used by supported
// integrations to carry large typed media to the final post-mask exporter
// boundary. Raw bytes never enter OpenTelemetry attributes.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"go.opentelemetry.io/otel/trace"
)

const (
	// InlineLimit is the largest typed media value retained inline. Larger
	// values use the authenticated upload authority when it is enabled.
	InlineLimit = 100_000
	UploadLimit = 25 * 1024 * 1024

	// MaxPendingItems bounds unique media payloads retained by one SDK runtime.
	MaxPendingItems = 32

	// UploadTokenPrefix identifies opaque handles which can pass through the
	// public mask. Tokens are always removed before ordinary OTLP export.
	UploadTokenPrefix = "nl_pending_media_"
)

type spanKey struct {
	traceID trace.TraceID
	spanID  trace.SpanID
}

func keyFor(span trace.SpanContext) (spanKey, bool) {
	if !span.IsValid() {
		return spanKey{}, false
	}
	return spanKey{traceID: span.TraceID(), spanID: span.SpanID()}, true
}

// PendingPayload is immutable media content retained outside telemetry until
// the accepted masked span reaches the exporter.
type PendingPayload struct {
	Token      string
	Content    []byte
	SHA256     string
	ByteLength int
	MIMEType   string
}

type pendingItem struct {
	payload PendingPayload
	owners  map[spanKey]struct{}
}

// Store is a per-runtime, byte- and item-bounded pending media store.
type Store struct {
	mu       sync.Mutex
	maxBytes int
	maxItems int
	bytes    int
	closed   bool
	items    map[string]*pendingItem
	spans    map[spanKey]map[string]struct{}
}

// NewStore constructs one isolated pending-media store.
func NewStore(maxBytes, maxItems int) *Store {
	if maxBytes <= 0 {
		maxBytes = UploadLimit
	}
	if maxItems <= 0 {
		maxItems = MaxPendingItems
	}
	return &Store{
		maxBytes: maxBytes,
		maxItems: maxItems,
		items:    make(map[string]*pendingItem),
		spans:    make(map[spanKey]map[string]struct{}),
	}
}

var spanStores = struct {
	sync.Mutex
	values map[spanKey]*Store
}{values: make(map[spanKey]*Store)}

// RegisterSpan binds a recording span to its runtime's store. Disabled upload
// pipelines never register, so capture does not retain their raw bytes.
func RegisterSpan(span trace.SpanContext, store *Store) {
	key, ok := keyFor(span)
	if !ok || store == nil {
		return
	}
	spanStores.Lock()
	store.mu.Lock()
	if !store.closed {
		spanStores.values[key] = store
	}
	store.mu.Unlock()
	spanStores.Unlock()
}

// Stage copies content into the store registered for span and returns an opaque
// token plus authoritative digest metadata. No bytes are retained on failure.
func Stage(span trace.SpanContext, content []byte, mimeType string) (PendingPayload, string) {
	key, ok := keyFor(span)
	if !ok || len(content) == 0 {
		return PendingPayload{}, "uploads_disabled"
	}
	spanStores.Lock()
	store := spanStores.values[key]
	spanStores.Unlock()
	if store == nil {
		return PendingPayload{}, "uploads_disabled"
	}
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	tokenDigest := sha256.Sum256([]byte(digestHex + "\x00" + mimeType))
	token := UploadTokenPrefix + hex.EncodeToString(tokenDigest[:12])

	spanStores.Lock()
	if spanStores.values[key] != store {
		spanStores.Unlock()
		return PendingPayload{}, "uploads_disabled"
	}
	store.mu.Lock()
	spanStores.Unlock()
	defer store.mu.Unlock()
	if store.closed {
		return PendingPayload{}, "uploads_disabled"
	}
	if item := store.items[token]; item != nil {
		if item.payload.SHA256 != digestHex || item.payload.ByteLength != len(content) || item.payload.MIMEType != mimeType {
			return PendingPayload{}, "pending_media_identity_conflict"
		}
		if _, exists := item.owners[key]; !exists {
			item.owners[key] = struct{}{}
			store.addSpanToken(key, token)
		}
		return payloadMetadata(item.payload), ""
	}
	if len(content) > store.maxBytes || store.bytes+len(content) > store.maxBytes {
		return PendingPayload{}, "pending_media_memory_limit"
	}
	if len(store.items) >= store.maxItems {
		return PendingPayload{}, "pending_media_item_limit"
	}
	payload := PendingPayload{
		Token: token, Content: append([]byte(nil), content...), SHA256: digestHex,
		ByteLength: len(content), MIMEType: mimeType,
	}
	store.items[token] = &pendingItem{payload: payload, owners: map[spanKey]struct{}{key: {}}}
	store.bytes += len(content)
	store.addSpanToken(key, token)
	return payloadMetadata(payload), ""
}

func payloadMetadata(payload PendingPayload) PendingPayload {
	payload.Content = nil
	return payload
}

func (s *Store) addSpanToken(key spanKey, token string) {
	tokens := s.spans[key]
	if tokens == nil {
		tokens = make(map[string]struct{})
		s.spans[key] = tokens
	}
	tokens[token] = struct{}{}
}

// Lease owns the staged payloads for one exported span until Release. Keeping
// accounting live through upload prevents concurrent exports from exceeding a
// runtime's configured memory budget.
type Lease struct {
	store    *Store
	key      spanKey
	payloads map[string]PendingPayload
	once     sync.Once
}

// AcquireSpan detaches a span from the capture registry and leases its staged
// payloads for post-mask resolution.
func AcquireSpan(span trace.SpanContext) *Lease {
	key, ok := keyFor(span)
	if !ok {
		return &Lease{}
	}
	spanStores.Lock()
	store := spanStores.values[key]
	delete(spanStores.values, key)
	if store == nil {
		spanStores.Unlock()
		return &Lease{}
	}
	store.mu.Lock()
	spanStores.Unlock()
	payloads := make(map[string]PendingPayload)
	for token := range store.spans[key] {
		if item := store.items[token]; item != nil {
			payloads[token] = item.payload
		}
	}
	store.mu.Unlock()
	return &Lease{store: store, key: key, payloads: payloads}
}

// Payload returns the staged bytes authorized for this span and token.
func (l *Lease) Payload(token string) (PendingPayload, bool) {
	if l == nil {
		return PendingPayload{}, false
	}
	payload, ok := l.payloads[token]
	return payload, ok
}

// Release drops all staged ownership held by this span.
func (l *Lease) Release() {
	if l == nil || l.store == nil {
		return
	}
	l.once.Do(func() { l.store.releaseSpan(l.key) })
}

// DiscardSpan drops staged media for a span that will not reach export.
func DiscardSpan(span trace.SpanContext) {
	key, ok := keyFor(span)
	if !ok {
		return
	}
	spanStores.Lock()
	store := spanStores.values[key]
	delete(spanStores.values, key)
	if store != nil {
		store.mu.Lock()
	}
	spanStores.Unlock()
	if store != nil {
		store.releaseSpanLocked(key)
		store.mu.Unlock()
	}
}

func (s *Store) releaseSpan(key spanKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseSpanLocked(key)
}

func (s *Store) releaseSpanLocked(key spanKey) {
	for token := range s.spans[key] {
		item := s.items[token]
		if item == nil {
			continue
		}
		delete(item.owners, key)
		if len(item.owners) == 0 {
			s.bytes -= item.payload.ByteLength
			delete(s.items, token)
		}
	}
	delete(s.spans, key)
}

// Close releases all retained bytes and unregisters this runtime's spans.
func (s *Store) Close() {
	if s == nil {
		return
	}
	spanStores.Lock()
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		for key, store := range spanStores.values {
			if store == s {
				delete(spanStores.values, key)
			}
		}
		s.items = make(map[string]*pendingItem)
		s.spans = make(map[spanKey]map[string]struct{})
		s.bytes = 0
	}
	s.mu.Unlock()
	spanStores.Unlock()
}

// Snapshot exposes aggregate accounting for focused internal tests.
func (s *Store) Snapshot() (items, bytes int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items), s.bytes
}
