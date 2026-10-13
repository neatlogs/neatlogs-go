package adk

import (
	"context"
	"errors"
	"iter"

	neatlogs "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// runKey carries the Run root's span ID to the A2A callbacks, so Run can end
// the A2A spans it owns.
type runKey struct{}

// Run executes an ADK turn inside a Neatlogs WORKFLOW span. Use it in place of
// runner.Run so wrapped models and tool callbacks inherit the same private
// trace without changing the process-global OpenTelemetry provider.
func Run(
	ctx context.Context,
	r *runner.Runner,
	userID string,
	sessionID string,
	message *genai.Content,
	config agent.RunConfig,
	options ...runner.RunOption,
) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if r == nil {
			yield(nil, errors.New("neatlogs/adk: runner is nil"))
			return
		}
		ctx = neatlogs.Identify(ctx, neatlogs.IdentifyOptions{
			SessionID: sessionID,
			EndUserID: userID,
		})
		traceCtx, root, end := neatlogs.Trace(ctx, "google.adk.run")
		defer end()
		run := root.SpanContext().SpanID()
		traceCtx = context.WithValue(traceCtx, runKey{}, run)
		root.SetAttributes(
			attribute.String("neatlogs.framework", "google_adk"),
			attribute.String("neatlogs.session.id", sessionID),
		)
		if input := contentText(message); input != "" {
			_ = neatlogs.SetTraceInput(root, input)
		}

		var finalOutput string
		failed, cancelled := false, false
		defer func() { endOpenA2ACalls(run, cancelled && !failed) }()
		report := func(err error) {
			if isCallerCancellation(traceCtx, err) {
				cancelled = true
				return
			}
			failed = true
			root.RecordError(err)
			root.SetStatus(codes.Error, err.Error())
		}
		for event, err := range r.Run(traceCtx, userID, sessionID, message, config, options...) {
			if err != nil {
				report(err)
			}
			if event != nil {
				// ADK reports blocked responses and remote-agent failures as
				// an event with a nil error; the run still failed.
				if eventErr := responseError(&event.LLMResponse); eventErr != nil {
					report(eventErr)
				}
				if event.Content != nil {
					if output := contentText(event.Content); output != "" && (event.IsFinalResponse() || finalOutput == "") {
						finalOutput = output
					}
				}
			}
			if !yield(event, err) {
				cancelled = true
				break
			}
		}
		if finalOutput != "" {
			_ = neatlogs.SetTraceOutput(root, boundedText(finalOutput))
		}
		switch {
		case failed:
			// Status is already Error; a failed run is not a cancelled one.
		case cancelled:
			root.SetAttributes(attribute.Bool(streamCancelled, true))
		default:
			root.SetStatus(codes.Ok, "")
		}
	}
}
