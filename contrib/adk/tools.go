package adk

import (
	"fmt"

	neatlogs "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/tool"
)

var activeToolCalls activeCallRegistry

// InstrumentConfig returns an ADK LLM-agent configuration with private-provider
// model and tool instrumentation installed. Existing callbacks are preserved.
func InstrumentConfig(config llmagent.Config) llmagent.Config {
	config.Model = WrapModel(config.Model)
	config.BeforeToolCallbacks = append(
		[]llmagent.BeforeToolCallback{BeforeTool},
		config.BeforeToolCallbacks...,
	)
	config.AfterToolCallbacks = append(
		[]llmagent.AfterToolCallback{AfterTool},
		config.AfterToolCallbacks...,
	)
	return config
}

// BeforeTool starts a Neatlogs TOOL span without changing ADK's result.
func BeforeTool(ctx tool.Context, adkTool tool.Tool, args map[string]any) (map[string]any, error) {
	if adkTool == nil {
		return nil, nil
	}
	_, span, end := neatlogs.StartProviderSpan(ctx, adkTool.Name(), "tool")
	span.SetAttributes(
		attribute.String("neatlogs.span.kind", "tool"),
		attribute.String("neatlogs.tool.name", adkTool.Name()),
		attribute.String("neatlogs.tool.input", boundedJSON(args)),
	)
	if description := adkTool.Description(); description != "" {
		span.SetAttributes(attribute.String("neatlogs.tool.description", description))
	}
	key := toolCallKey(ctx, adkTool)
	endEvicted(
		activeToolCalls.put(key, activeCall{span: span, end: end}),
		"superseded or abandoned ADK tool callback",
	)
	return nil, nil
}

// AfterTool completes the TOOL span opened by BeforeTool and preserves ADK's
// original tool result and error.
func AfterTool(ctx tool.Context, adkTool tool.Tool, args, result map[string]any, callErr error) (map[string]any, error) {
	if adkTool == nil {
		return nil, nil
	}
	key := toolCallKey(ctx, adkTool)
	call, ok := activeToolCalls.take(key)
	if !ok {
		_, call.span, call.end = neatlogs.StartProviderSpan(ctx, adkTool.Name(), "tool")
		call.span.SetAttributes(
			attribute.String("neatlogs.span.kind", "tool"),
			attribute.String("neatlogs.tool.name", adkTool.Name()),
			attribute.String("neatlogs.tool.input", boundedJSON(args)),
		)
	}
	defer call.end()
	call.span.SetAttributes(attribute.String("neatlogs.tool.output", boundedJSON(result)))
	if callErr != nil {
		call.span.RecordError(callErr)
		call.span.SetStatus(codes.Error, callErr.Error())
	} else {
		call.span.SetStatus(codes.Ok, "")
	}
	return nil, nil
}

func toolCallKey(ctx tool.Context, adkTool tool.Tool) string {
	return fmt.Sprintf("%s:%s:%s", ctx.InvocationID(), ctx.FunctionCallID(), adkTool.Name())
}
