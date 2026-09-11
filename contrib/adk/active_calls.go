package adk

import (
	"sync"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxActiveCalls = 1024

type activeCall struct {
	span trace.Span
	end  func()
}

// activeCallRegistry bounds callback state when an upstream framework omits a
// matching after-callback. Evicted calls are ended as errors instead of being
// retained for the lifetime of the process.
type activeCallRegistry struct {
	mu      sync.Mutex
	entries map[string]activeCall
}

func (r *activeCallRegistry) put(key string, call activeCall) []activeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]activeCall)
	}

	var evicted []activeCall
	if previous, ok := r.entries[key]; ok {
		evicted = append(evicted, previous)
		delete(r.entries, key)
	}
	if len(r.entries) >= maxActiveCalls {
		for oldestKey, oldest := range r.entries {
			evicted = append(evicted, oldest)
			delete(r.entries, oldestKey)
			break
		}
	}
	r.entries[key] = call
	return evicted
}

func (r *activeCallRegistry) take(key string) (activeCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call, ok := r.entries[key]
	if ok {
		delete(r.entries, key)
	}
	return call, ok
}

func endEvicted(calls []activeCall, reason string) {
	for _, call := range calls {
		call.span.SetStatus(codes.Error, reason)
		call.end()
	}
}
