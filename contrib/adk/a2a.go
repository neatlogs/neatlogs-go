package adk

import (
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/session"
)

// A2A (agent-to-agent) calls cross an HTTP boundary, so without trace-context
// propagation the remote agent's execution lands in a SEPARATE trace from the
// caller. These helpers carry the W3C traceparent across that boundary using the
// global OpenTelemetry propagator (installed by neatlogs.Init), so the remote
// server's agent/LLM spans nest under the calling trace — one linked trace.
//
// They deliberately do NOT create their own HTTP spans (unlike otelhttp); they
// only inject/extract the trace context, keeping the trace free of transport
// noise.
//
// Deprecated: part of the quarantined ADK integration (see DEPRECATED.md). These
// helpers read/write the GLOBAL OpenTelemetry propagator, which the isolated
// Neatlogs SDK no longer installs, so no traceparent is propagated. Cross-process
// continuation in the isolated SDK uses neatlogs.InjectTraceContext /
// ExtractTraceContext (the SDK's private propagator) instead.

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
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// injectingTransport injects trace context into outbound request headers, then
// delegates to base. It adds no spans of its own.
type injectingTransport struct{ base http.RoundTripper }

func (t injectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
	return t.base.RoundTrip(req)
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

// A2ABeforeRequest records the outbound A2A request message on the active span.
// It sets both the generic neatlogs.input.value (which the backend reads for any
// span kind, including AGENT) and the indexed input_messages form (consumed for
// LLM-shaped views), so the client's invoke_agent span shows what was sent.
func A2ABeforeRequest(ctx agent.CallbackContext, req *a2a.SendMessageRequest) (*session.Event, error) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() && req != nil && req.Message != nil {
		if text := a2aMessageText(req.Message); text != "" {
			span.SetAttributes(
				attribute.String("neatlogs.input.value", text),
				attribute.String(inputMsgPrefix+"0.role", "user"),
				attribute.String(inputMsgPrefix+"0.content", text),
			)
		}
	}
	return nil, nil
}

// A2AAfterRequest records the remote agent's response on the active span. It
// sets neatlogs.output.value (read by the backend for AGENT spans — the indexed
// output_messages form is only reconstructed for LLM spans) plus the indexed
// form for completeness.
func A2AAfterRequest(ctx agent.CallbackContext, _ *a2a.SendMessageRequest, resp *session.Event, _ error) (*session.Event, error) {
	span := trace.SpanFromContext(ctx)
	if span != nil && span.IsRecording() && resp != nil && resp.Content != nil {
		if text := contentText(resp.Content); text != "" {
			span.SetAttributes(
				attribute.String("neatlogs.output.value", text),
				attribute.String(outputMsgPrefix+"0.role", "assistant"),
				attribute.String(outputMsgPrefix+"0.content", text),
			)
		}
	}
	return nil, nil
}

// a2aMessageText joins the text parts of an A2A message.
func a2aMessageText(msg *a2a.Message) string {
	var text string
	for _, part := range msg.Parts {
		if part == nil {
			continue
		}
		if t := part.Text(); t != "" {
			if text != "" {
				text += "\n"
			}
			text += t
		}
	}
	return text
}
