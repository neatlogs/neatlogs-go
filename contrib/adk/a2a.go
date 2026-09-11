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
	_, span, end := neatlogs.StartProviderSpan(ctx, "google.adk.a2a", "agent")
	span.SetAttributes(attribute.String("neatlogs.span.kind", "agent"))
	if req.Message != nil {
		if text := a2aMessageText(req.Message); text != "" {
			span.SetAttributes(
				attribute.String("neatlogs.input.value", text),
				attribute.String(inputMsgPrefix+"0.role", "user"),
				attribute.String(inputMsgPrefix+"0.content", text),
			)
		}
	}
	endEvicted(
		activeA2ACalls.put(a2aCallKey(req), activeCall{span: span, end: end}),
		"superseded or abandoned ADK A2A callback",
	)
	return nil, nil
}

// A2AAfterRequest records the remote agent's response on the active span. It
// sets neatlogs.output.value (read by the backend for AGENT spans — the indexed
// output_messages form is only reconstructed for LLM spans) plus the indexed
// form for completeness.
func A2AAfterRequest(ctx agent.CallbackContext, req *a2a.SendMessageRequest, resp *session.Event, callErr error) (*session.Event, error) {
	var call activeCall
	if req != nil {
		call, _ = activeA2ACalls.take(a2aCallKey(req))
	}
	if call.span == nil {
		_, call.span, call.end = neatlogs.StartProviderSpan(ctx, "google.adk.a2a", "agent")
		call.span.SetAttributes(attribute.String("neatlogs.span.kind", "agent"))
	}
	defer call.end()
	if callErr != nil {
		call.span.RecordError(callErr)
		call.span.SetStatus(codes.Error, callErr.Error())
	} else {
		call.span.SetStatus(codes.Ok, "")
	}
	if resp != nil && resp.Content != nil {
		if text := contentText(resp.Content); text != "" {
			call.span.SetAttributes(
				attribute.String("neatlogs.output.value", text),
				attribute.String(outputMsgPrefix+"0.role", "assistant"),
				attribute.String(outputMsgPrefix+"0.content", text),
			)
		}
	}
	return nil, nil
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
