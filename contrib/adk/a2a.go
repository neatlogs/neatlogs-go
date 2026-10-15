package adk

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	neatlogs "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/session"
)

// A2A (agent-to-agent) calls cross an HTTP boundary, so without trace-context
// propagation the remote agent's execution lands in a separate trace from the
// caller. These helpers carry Neatlogs' private W3C trace context without
// reading or replacing the process-global OpenTelemetry propagator.
//
// They deliberately do NOT create their own HTTP spans (unlike otelhttp); they
// only inject/extract the trace context, keeping the trace free of transport
// noise.
//
// A2AHTTPClient returns an *http.Client whose transport injects the current
// trace context as a traceparent header on every outbound request. Pass it to
// the A2A client factory, e.g.:
//
//	factory := a2aclient.NewFactory(a2aclient.WithJSONRPCTransport(nladk.A2AHTTPClient()))
func A2AHTTPClient() *http.Client {
	return &http.Client{Transport: injectingTransport{base: http.DefaultTransport}}
}

// A2AHandler wraps an http.Handler so incoming traceparent headers are extracted
// into the request context. Wrap the A2A server mux with it so ADK's request
// handler (which uses req.Context()) parents its spans under the caller's trace:
//
//	srv := &http.Server{Handler: nladk.A2AHandler(mux)}
func A2AHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := neatlogs.ExtractTraceContext(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// injectingTransport injects trace context into outbound request headers, then
// delegates to base. It adds no spans of its own.
type injectingTransport struct{ base http.RoundTripper }

func (t injectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	neatlogs.InjectTraceContext(clone.Context(), propagation.HeaderCarrier(clone.Header))
	return base.RoundTrip(clone)
}

// Capturing A2A client I/O.
//
// A remote (A2A) agent delegates over HTTP and never calls an LLM locally, so
// its invoke_agent span would otherwise carry no input/output. These callbacks
// record the outbound request and the response onto that span as
// neatlogs.llm.input_messages / output_messages, so the client trace shows what
// was sent to and received from the remote agent. Wire both into the remote
// agent config:
//
//	remoteagent.NewA2A(remoteagent.A2AConfig{
//		BeforeRequestCallbacks: []remoteagent.BeforeA2ARequestCallback{nladk.A2ABeforeRequest},
//		AfterRequestCallbacks:  []remoteagent.AfterA2ARequestCallback{nladk.A2AAfterRequest},
//		...
//	})
//
// The callback signatures match remoteagent.BeforeA2ARequestCallback and
// remoteagent.AfterA2ARequestCallback. Returning (nil, nil) means "did not
// intercept" so ADK proceeds normally.

var activeA2ACalls activeCallRegistry

// A2ABeforeRequest starts a Neatlogs-owned AGENT span for the outbound call.
func A2ABeforeRequest(ctx agent.CallbackContext, req *a2a.SendMessageRequest) (*session.Event, error) {
	if req == nil {
		return nil, nil
	}
	call := startA2ACall(ctx)
	if req.Message != nil {
		if text := a2aMessageText(req.Message); text != "" {
			call.span.SetAttributes(
				attribute.String("neatlogs.input.value", text),
				attribute.String(inputMsgPrefix+"0.role", "user"),
				attribute.String(inputMsgPrefix+"0.content", text),
			)
		}
	}
	endEvicted(
		activeA2ACalls.put(a2aCallKey(req), call),
		"superseded or abandoned ADK A2A callback",
	)
	return nil, nil
}

func startA2ACall(ctx agent.CallbackContext) activeCall {
	_, span, end := neatlogs.StartProviderSpan(ctx, "google.adk.a2a", "agent")
	span.SetAttributes(attribute.String("neatlogs.span.kind", "agent"))
	run, _ := ctx.Value(runKey{}).(trace.SpanID)
	return activeCall{span: span, end: end, run: run}
}

// A2AAfterRequest records the remote agent's response on the active span. It
// sets neatlogs.output.value (read by the backend for AGENT spans — the indexed
// output_messages form is only reconstructed for LLM spans) plus the indexed
// form for completeness.
//
// In streaming mode ADK calls it once per remote event. Under Run the span
// stays open until the event that completes the turn, an error, or the end of
// Run. Without Run nothing could end a span left open, so it ends on each call.
func A2AAfterRequest(ctx agent.CallbackContext, req *a2a.SendMessageRequest, resp *session.Event, callErr error) (*session.Event, error) {
	var call activeCall
	if req != nil {
		call, _ = activeA2ACalls.take(a2aCallKey(req))
	}
	if call.span == nil {
		call = startA2ACall(ctx)
	}
	// ADK turns transport and remote-task failures into an error event with a
	// nil callErr; both forms are a failed call.
	failure := callErr
	if resp != nil {
		eventErr := responseError(&resp.LLMResponse)
		if failure == nil {
			failure = eventErr
		}
		// The text of an error event is the error itself; it must not replace
		// the output received before it.
		if eventErr == nil {
			call.addOutput(resp)
		}
	}
	if failure == nil && resp != nil && !resp.TurnComplete && call.run.IsValid() && a2aSends(req) {
		endEvicted(
			activeA2ACalls.put(a2aCallKey(req), call),
			"superseded or abandoned ADK A2A callback",
		)
		return nil, nil
	}
	defer call.end()
	switch {
	case failure == nil:
		call.span.SetStatus(codes.Ok, "")
	case isCallerCancellation(ctx, failure):
		call.span.SetAttributes(attribute.Bool(streamCancelled, true))
	default:
		call.span.RecordError(failure)
		call.span.SetStatus(codes.Error, failure.Error())
	}
	return nil, nil
}

// addOutput keeps the response text on the span as events arrive, so it
// survives a span that is ended early: the last complete response, or the
// partial deltas while no complete response has arrived.
func (c *activeCall) addOutput(resp *session.Event) {
	text := contentText(resp.Content)
	if text == "" {
		return
	}
	hasText := hasOutputText(resp.Content)
	switch {
	case resp.Partial && hasText:
		c.partial = boundedText(c.partial + text)
	case !resp.Partial && (hasText || c.output == ""):
		c.output = text
	}
	text = c.output
	if text == "" {
		text = c.partial
	}
	if text == "" {
		return
	}
	c.span.SetAttributes(
		attribute.String("neatlogs.output.value", text),
		attribute.String(outputMsgPrefix+"0.role", "assistant"),
		attribute.String(outputMsgPrefix+"0.content", text),
	)
}

// endOpenA2ACalls ends the A2A spans of one Run that never saw an event
// completing the turn, so they are exported instead of staying open. They
// carry no success status because the remote call was not seen to finish.
func endOpenA2ACalls(run trace.SpanID, cancelled bool) {
	if !run.IsValid() {
		return
	}
	for _, call := range activeA2ACalls.takeRun(run) {
		if cancelled {
			call.span.SetAttributes(attribute.Bool(streamCancelled, true))
		} else {
			call.span.SetAttributes(attribute.Bool(streamIncomplete, true))
		}
		call.end()
	}
}

// a2aSends reports whether ADK sends req to the remote agent. It skips an
// empty message, so no later event follows the first callback.
func a2aSends(req *a2a.SendMessageRequest) bool {
	return req != nil && req.Message != nil && len(req.Message.Parts) > 0
}

func a2aCallKey(req *a2a.SendMessageRequest) string {
	if req == nil {
		return ""
	}
	if req.Message != nil && req.Message.ID != "" {
		return fmt.Sprintf("%p:%s", req, req.Message.ID)
	}
	return fmt.Sprintf("%p", req)
}

// a2aMessageText joins the text parts of an A2A message.
func a2aMessageText(msg *a2a.Message) string {
	var text strings.Builder
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		if t := part.Text(); t != "" {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			remaining := maxOutputBytes - text.Len()
			if remaining <= 0 {
				break
			}
			text.WriteString(boundedTextTo(t, remaining))
		}
	}
	return text.String()
}
