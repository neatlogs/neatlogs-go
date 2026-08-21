package neatlogs

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	attrs "github.com/neatlogs/neatlogs-go/internal/attributes"
)

// sdkState is shared by the process-wide Init lifecycle and each independent
// Client. The global lifecycle uses uninitialized/running/closing; a Client
// runtime transitions running/closing/closed and is never re-opened.
type sdkState uint8

const (
	stateUninitialized sdkState = iota
	stateRunning
	stateClosing
	stateClosed
)

// sdkRuntime owns exactly one private provider and its shutdown gate. It has no
// dependency on the process-wide lifecycle, so context-scoped Clients cannot
// block or close Init or one another.
type sdkRuntime struct {
	mu sync.Mutex

	state        sdkState
	provider     *sdktrace.TracerProvider
	tracer       trace.Tracer
	lifecycle    *activeSpanRegistry
	workflowName string

	done        chan struct{}
	shutdownErr error
	signals     *shutdownSignalController
}

func newSDKRuntime(tp *sdktrace.TracerProvider, lifecycle *activeSpanRegistry, workflowName string) *sdkRuntime {
	return &sdkRuntime{
		state:        stateRunning,
		provider:     tp,
		tracer:       tp.Tracer(tracerName, trace.WithInstrumentationVersion(Version)),
		lifecycle:    lifecycle,
		workflowName: workflowName,
		done:         make(chan struct{}),
	}
}

func (r *sdkRuntime) setSignalController(controller *shutdownSignalController) {
	r.mu.Lock()
	if r.state == stateRunning {
		r.signals = controller
	}
	r.mu.Unlock()
}

// startSpan holds the runtime gate through Tracer.Start. Once shutdown changes
// the state to closing, no helper or wrapper can register another span.
func (r *sdkRuntime) startSpan(
	ctx context.Context,
	name string,
	options ...trace.SpanStartOption,
) (context.Context, trace.Span, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateRunning {
		return startNoopSpan(ctx, name, options...)
	}
	_, span := r.tracer.Start(privateStartContext(ctx, r), name, options...)
	return withPrivateTraceContext(ctx, span.SpanContext(), r), span, func() { span.End() }
}

// startProviderSpan starts the optional auto-root and provider span under one
// gate acquisition. Shutdown therefore cannot claim the registry between the
// two starts and leave a half-created hierarchy.
func (r *sdkRuntime) startProviderSpan(ctx context.Context, name, kind string) (context.Context, trace.Span, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != stateRunning {
		return startNoopSpan(ctx, name)
	}

	needsRoot := autoRootEnabled()
	if _, isRoot := rootKinds[kind]; isRoot {
		needsRoot = false
	}
	if privateSpanContextFor(ctx, r).IsValid() {
		needsRoot = false
	}

	if !needsRoot {
		_, span := r.tracer.Start(privateStartContext(ctx, r), name)
		return withPrivateTraceContext(ctx, span.SpanContext(), r), span, func() { span.End() }
	}

	rootCtx, root := r.tracer.Start(privateStartContext(ctx, r), r.workflowName, trace.WithAttributes(
		attribute.String(attrs.SpanKind, attrs.KindWorkflow),
		attribute.Bool("neatlogs.auto_root", true),
	))
	_, span := r.tracer.Start(rootCtx, name)
	return withPrivateTraceContext(ctx, span.SpanContext(), r), span, func() {
		span.End()
		root.End()
	}
}

func startNoopSpan(
	ctx context.Context,
	name string,
	options ...trace.SpanStartOption,
) (context.Context, trace.Span, func()) {
	_, span := noopTP.Tracer(tracerName, trace.WithInstrumentationVersion(Version)).Start(
		trace.ContextWithSpanContext(ctx, trace.SpanContext{}),
		name,
		options...,
	)
	return ctx, span, func() { span.End() }
}

func (r *sdkRuntime) forceFlush(ctx context.Context) error {
	r.mu.Lock()
	if r.state == stateClosed {
		err := r.shutdownErr
		r.mu.Unlock()
		return err
	}
	if r.state == stateClosing {
		done := r.done
		r.mu.Unlock()
		select {
		case <-done:
			r.mu.Lock()
			err := r.shutdownErr
			r.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := r.provider.ForceFlush(ctx)
	r.mu.Unlock()
	return err
}

// shutdown changes state before ending spans. Calls that arrive after closing
// begins wait for the same result; they never invoke provider shutdown twice.
func (r *sdkRuntime) shutdown(ctx context.Context, reason string) error {
	r.mu.Lock()
	switch r.state {
	case stateClosed:
		err := r.shutdownErr
		r.mu.Unlock()
		return err
	case stateClosing:
		done := r.done
		r.mu.Unlock()
		select {
		case <-done:
			r.mu.Lock()
			err := r.shutdownErr
			r.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case stateRunning:
		r.state = stateClosing
	}
	controller := r.signals
	r.mu.Unlock()

	if controller != nil {
		controller.Stop()
	}
	r.lifecycle.endActiveSpans(reason)
	err := r.provider.Shutdown(ctx)

	r.mu.Lock()
	r.shutdownErr = err
	r.state = stateClosed
	close(r.done)
	r.mu.Unlock()
	return err
}

func (r *sdkRuntime) wait(ctx context.Context) error {
	r.mu.Lock()
	if r.state == stateClosed {
		err := r.shutdownErr
		r.mu.Unlock()
		return err
	}
	done := r.done
	r.mu.Unlock()
	select {
	case <-done:
		r.mu.Lock()
		err := r.shutdownErr
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
