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
		root.SetAttributes(
			attribute.String("neatlogs.framework", "google_adk"),
			attribute.String("neatlogs.session.id", sessionID),
		)
		if input := contentText(message); input != "" {
			_ = neatlogs.SetTraceInput(root, input)
		}

		var finalOutput string
		failed := false
		for event, err := range r.Run(traceCtx, userID, sessionID, message, config, options...) {
			if err != nil {
				failed = true
				root.RecordError(err)
				root.SetStatus(codes.Error, err.Error())
			}
			if event != nil && event.Content != nil {
				if output := contentText(event.Content); output != "" && (event.IsFinalResponse() || finalOutput == "") {
					finalOutput = output
				}
			}
			if !yield(event, err) {
				root.SetAttributes(attribute.Bool("neatlogs.stream.cancelled", true))
				if finalOutput != "" {
					_ = neatlogs.SetTraceOutput(root, boundedText(finalOutput))
				}
				return
			}
		}
		if finalOutput != "" {
			_ = neatlogs.SetTraceOutput(root, boundedText(finalOutput))
		}
		if !failed {
			root.SetStatus(codes.Ok, "")
		}
	}
}
