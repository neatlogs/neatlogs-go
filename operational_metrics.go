package neatlogs

import (
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// Operational metric attribute keys, shared with the Python and TypeScript SDKs.
const (
	durationMsAttrKey = "neatlogs.metrics.duration_ms"
	ttftMsAttrKey     = "neatlogs.llm.metrics.ttft_ms"
	stgMsAttrKey      = "neatlogs.llm.metrics.streaming_time_to_generate_ms"

	// googleChunkEventName is emitted by the Google GenAI wrappers on every
	// streamed text chunk and drives TTFT / streaming-time-to-generate.
	googleChunkEventName = "gen_ai.content.chunk"
)

// computeOperationalMetrics ports the Python AttributeProcessor and the
// TypeScript attribute-processor operational metrics to the Go exporter
// boundary, so Go traces carry the same attributes py and ts populate on
// every span:
//
//   - neatlogs.metrics.duration_ms on every span (span end - start)
//   - neatlogs.llm.metrics.ttft_ms from the first gen_ai.content.chunk event
//   - neatlogs.llm.metrics.streaming_time_to_generate_ms from the first to
//     the last chunk event
//
// As in py/ts, a ttft value already captured live by a streaming wrapper is
// authoritative and is never recomputed from events.
func computeOperationalMetrics(stub spanStub, attrs []attribute.KeyValue) []attribute.KeyValue {
	durationMs := float64(stub.EndTime.Sub(stub.StartTime)) / float64(time.Millisecond)
	attrs = upsertFloatAttr(attrs, durationMsAttrKey, durationMs)

	if hasAttrKey(attrs, ttftMsAttrKey) {
		return attrs
	}

	var firstChunk, lastChunk time.Time
	for _, event := range stub.Events {
		if event.Name != googleChunkEventName {
			continue
		}
		if firstChunk.IsZero() || event.Time.Before(firstChunk) {
			firstChunk = event.Time
		}
		if event.Time.After(lastChunk) {
			lastChunk = event.Time
		}
	}
	if firstChunk.IsZero() {
		return attrs
	}

	ttftMs := round3(float64(firstChunk.Sub(stub.StartTime)) / float64(time.Millisecond))
	attrs = upsertFloatAttr(attrs, ttftMsAttrKey, ttftMs)

	if !lastChunk.Equal(firstChunk) {
		stgMs := round3(float64(lastChunk.Sub(firstChunk)) / float64(time.Millisecond))
		attrs = upsertFloatAttr(attrs, stgMsAttrKey, stgMs)
	}
	return attrs
}

func round3(value float64) float64 {
	return math.Round(value*1000) / 1000
}

func hasAttrKey(attrs []attribute.KeyValue, key string) bool {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return true
		}
	}
	return false
}

func upsertFloatAttr(attrs []attribute.KeyValue, key string, value float64) []attribute.KeyValue {
	for index, kv := range attrs {
		if string(kv.Key) == key {
			attrs[index] = attribute.Float64(key, value)
			return attrs
		}
	}
	return append(attrs, attribute.Float64(key, value))
}
