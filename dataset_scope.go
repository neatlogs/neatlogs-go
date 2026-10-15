package neatlogs

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const maxDatasetCaptureSpans = 2000
const maxDatasetCaptureScopes = 32

type datasetScopeKey struct{}
type datasetSpanKey struct {
	traceID trace.TraceID
	spanID  trace.SpanID
}
type datasetSpanState struct {
	parent              trace.SpanID
	ended, acknowledged bool
}

type datasetCaptureScope struct {
	registry  *datasetCaptureRegistry
	accepting bool
	spans     map[datasetSpanKey]*datasetSpanState
	err       error
}

// The registry retains only bounded IDs and delivery flags, never span payloads.
type datasetCaptureRegistry struct {
	mu     sync.Mutex
	scopes map[*datasetCaptureScope]struct{}
	bySpan map[datasetSpanKey]*datasetCaptureScope
}

type DatasetTraceManifest struct {
	TraceID string   `json:"traceId"`
	SpanIDs []string `json:"spanIds"`
}

type datasetCaptureSampler struct {
	base     sdktrace.Sampler
	registry *datasetCaptureRegistry
}

func (s datasetCaptureSampler) ShouldSample(parameters sdktrace.SamplingParameters) sdktrace.SamplingResult {
	scope, _ := parameters.ParentContext.Value(datasetScopeKey{}).(*datasetCaptureScope)
	if scope != nil && scope.registry == s.registry {
		return sdktrace.AlwaysSample().ShouldSample(parameters)
	}
	return s.base.ShouldSample(parameters)
}

func (s datasetCaptureSampler) Description() string { return s.base.Description() }

func newDatasetCaptureRegistry() *datasetCaptureRegistry {
	return &datasetCaptureRegistry{scopes: make(map[*datasetCaptureScope]struct{}), bySpan: make(map[datasetSpanKey]*datasetCaptureScope)}
}

func (p *datasetCaptureRegistry) begin() (*datasetCaptureScope, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.scopes) >= maxDatasetCaptureScopes {
		return nil, errors.New("neatlogs: too many concurrent dataset captures")
	}
	scope := &datasetCaptureScope{registry: p, accepting: true, spans: make(map[datasetSpanKey]*datasetSpanState)}
	p.scopes[scope] = struct{}{}
	return scope, nil
}

func (p *datasetCaptureRegistry) allows(ctx context.Context) bool {
	scope, _ := ctx.Value(datasetScopeKey{}).(*datasetCaptureScope)
	if scope == nil || scope.registry != p {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return scope.accepting
}

func datasetKey(span trace.SpanContext) datasetSpanKey {
	return datasetSpanKey{span.TraceID(), span.SpanID()}
}

func (p *datasetCaptureRegistry) OnStart(ctx context.Context, span sdktrace.ReadWriteSpan) {
	scope, _ := ctx.Value(datasetScopeKey{}).(*datasetCaptureScope)
	if scope == nil || scope.registry != p {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !scope.accepting {
		scope.err = errors.New("neatlogs: a dataset capture span started after execution returned")
		return
	}
	if !span.SpanContext().IsSampled() {
		scope.err = errors.New("neatlogs: dataset capture requires sampled spans")
		return
	}
	if len(scope.spans) >= maxDatasetCaptureSpans {
		scope.err = errors.New("neatlogs: dataset capture exceeds 2000 spans")
		return
	}
	key := datasetKey(span.SpanContext())
	scope.spans[key] = &datasetSpanState{parent: span.Parent().SpanID()}
	p.bySpan[key] = scope
}

func (p *datasetCaptureRegistry) OnEnd(span sdktrace.ReadOnlySpan) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := datasetKey(span.SpanContext())
	scope := p.bySpan[key]
	if scope == nil {
		return
	}
	scope.spans[key].ended = true
	if datasetIncompleteSpan(span, false) {
		scope.err = errors.New("neatlogs: dataset capture contains dropped or incomplete span evidence")
	}
}

func datasetIncompleteSpan(span sdktrace.ReadOnlySpan, final bool) bool {
	if span.DroppedAttributes() > 0 || span.DroppedEvents() > 0 || span.DroppedLinks() > 0 || datasetIncompleteAttributes(span.Attributes(), final) {
		return true
	}
	for _, event := range span.Events() {
		if event.DroppedAttributeCount > 0 || datasetIncompleteAttributes(event.Attributes, final) {
			return true
		}
	}
	for _, link := range span.Links() {
		if link.DroppedAttributeCount > 0 || datasetIncompleteAttributes(link.Attributes, final) {
			return true
		}
	}
	return span.Resource() != nil && datasetIncompleteAttributes(span.Resource().Attributes(), final)
}

func datasetIncompleteAttributes(values []attribute.KeyValue, final bool) bool {
	for _, value := range values {
		key := strings.ToLower(string(value.Key))
		if !strings.HasPrefix(key, "neatlogs.") {
			continue
		}
		if key == interruptedAttribute && value.Value.Type() == attribute.BOOL && value.Value.AsBool() {
			return true
		}
		if strings.Contains(key, "truncat") && ((value.Value.Type() == attribute.BOOL && value.Value.AsBool()) || (value.Value.Type() == attribute.INT64 && value.Value.AsInt64() > 0) || (value.Value.Type() == attribute.STRING && value.Value.AsString() != "" && value.Value.AsString() != "false")) {
			return true
		}
		if value.Value.Type() == attribute.STRING && strings.HasSuffix(key, ".state") && canonicalMediaReference(values, strings.TrimSuffix(key, "state")) {
			switch value.Value.AsString() {
			case "failed", "upload-failed", "truncated", "error":
				return true
			case "pending-upload":
				return final
			}
		}
	}
	return false
}

func (p *datasetCaptureRegistry) acknowledge(spans []sdktrace.ReadOnlySpan) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, span := range spans {
		key := datasetKey(span.SpanContext())
		if scope := p.bySpan[key]; scope != nil {
			scope.spans[key].acknowledged = true
			if datasetIncompleteSpan(span, true) {
				scope.err = errors.New("neatlogs: exported dataset evidence is incomplete")
			}
		}
	}
}

func (s *datasetCaptureScope) closeExecution() {
	s.registry.mu.Lock()
	s.accepting = false
	s.registry.mu.Unlock()
}

func (s *datasetCaptureScope) release() {
	p := s.registry
	p.mu.Lock()
	defer p.mu.Unlock()
	s.accepting = false
	delete(p.scopes, s)
	for key := range s.spans {
		delete(p.bySpan, key)
	}
	s.spans = nil
}

func (s *datasetCaptureScope) manifest() ([]DatasetTraceManifest, error) {
	p := s.registry
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if len(s.spans) == 0 {
		return nil, errors.New("neatlogs: dataset capture recorded no instrumented executions")
	}
	traces := make(map[string][]string)
	roots := make(map[string]int)
	for key, state := range s.spans {
		if !state.ended {
			return nil, errors.New("neatlogs: dataset capture has open spans; join execution goroutines before returning")
		}
		if !state.acknowledged {
			return nil, errors.New("neatlogs: dataset capture has unacknowledged span exports")
		}
		if state.parent.IsValid() {
			if _, ok := s.spans[datasetSpanKey{key.traceID, state.parent}]; !ok {
				return nil, errors.New("neatlogs: dataset capture has a parent outside its execution")
			}
		} else {
			roots[key.traceID.String()]++
		}
		id := key.traceID.String()
		traces[id] = append(traces[id], key.spanID.String())
	}
	manifest := make([]DatasetTraceManifest, 0, len(traces))
	for id, spans := range traces {
		if roots[id] != 1 {
			return nil, errors.New("neatlogs: dataset capture requires one complete root per trace")
		}
		sort.Strings(spans)
		manifest = append(manifest, DatasetTraceManifest{TraceID: id, SpanIDs: spans})
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].TraceID < manifest[j].TraceID })
	return manifest, nil
}

func (*datasetCaptureRegistry) ForceFlush(context.Context) error { return nil }
func (*datasetCaptureRegistry) Shutdown(context.Context) error   { return nil }
