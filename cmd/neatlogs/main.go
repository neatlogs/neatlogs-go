package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	neatlogs "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(arguments []string) int {
	jsonOutput := false
	mode := ""
	for _, argument := range arguments {
		switch argument {
		case "doctor":
		case "--local":
			if mode != "" {
				return usage()
			}
			mode = "local"
		case "--probe":
			if mode != "" {
				return usage()
			}
			mode = "probe"
		case "--json":
			jsonOutput = true
		case "--help", "-h":
			fmt.Println("Usage: neatlogs doctor (--local | --probe) [--json]")
			return 0
		default:
			return usage()
		}
	}
	if len(arguments) == 0 || arguments[0] != "doctor" || mode == "" {
		return usage()
	}
	ctx := context.Background()
	cfg := neatlogs.Config{APIKey: os.Getenv("NEATLOGS_API_KEY"), Endpoint: os.Getenv("NEATLOGS_ENDPOINT"), WorkflowName: "neatlogs.doctor.v2"}
	if mode == "probe" && strings.TrimSpace(cfg.APIKey) == "" {
		return output(doctorPreflightFailure("CREDENTIAL_MISSING", "A project ingestion credential is required", "SET_CREDENTIAL"), jsonOutput, 3)
	}
	if mode == "probe" && !validDoctorEndpoint(cfg.Endpoint) {
		return output(doctorPreflightFailure("ENDPOINT_INVALID", "The trace endpoint is invalid", "SET_ENDPOINT"), jsonOutput, 3)
	}
	options := []neatlogs.Option{
		neatlogs.WithExporter(tracetest.NewInMemoryExporter()),
		neatlogs.WithDoctorProbe(),
	}
	if mode == "probe" && cfg.APIKey != "" && validDoctorEndpoint(cfg.Endpoint) {
		// Probe uses the same authenticated OTLP exporter as user telemetry. The
		// versioned marker changes observability only; it never selects a tenant.
		options = []neatlogs.Option{neatlogs.WithDoctorProbe()}
	}
	client, err := neatlogs.NewClient(ctx, cfg, options...)
	if err != nil {
		return output(neatlogs.DoctorV2Result{FormatVersion: neatlogs.DoctorV2FormatVersion, Mode: mode, Status: neatlogs.DoctorFail, Runtime: neatlogs.DoctorV2Runtime{Language: "go", SDKVersion: neatlogs.Version, SchemaVersion: "2", Transport: "otlp_http_protobuf"}, Checks: []neatlogs.DoctorV2Check{{Name: "configuration", Status: neatlogs.DoctorFail, ReasonCode: "INSTRUMENTOR_INACTIVE", Message: "Doctor could not initialize an isolated runtime", RemediationCode: "ENABLE_INSTRUMENTOR"}}}, jsonOutput, 2)
	}
	clientCtx := client.Context(ctx)
	rootCtx, root, endRoot := neatlogs.Trace(clientCtx, "doctor.probe.root")
	traceID := root.SpanContext().TraceID().String()
	root.SetAttributes(append(doctorSpanAttributes("WORKFLOW"), attribute.String("neatlogs.input.value", `{"prompt":"generated diagnostic input"}`))...)
	agentCtx, agent, endAgent := neatlogs.StartSpan(rootCtx, "doctor.probe.agent", "agent", append(doctorSpanAttributes("AGENT"), attribute.String("neatlogs.input.value", `{"prompt":"generated diagnostic input"}`))...)
	_, llm, endLLM := neatlogs.StartSpan(agentCtx, "doctor.probe.llm", "llm",
		append(doctorSpanAttributes("LLM"),
			attribute.String("neatlogs.input.value", `{"messages":[{"role":"user","content":"generated diagnostic input"}]}`),
			attribute.Int("neatlogs.llm.token_count.prompt", 11),
			attribute.Int("neatlogs.llm.token_count.completion", 7),
			attribute.Int("neatlogs.llm.token_count.total", 18),
		)...,
	)
	llm.SetAttributes(attribute.String("neatlogs.output.value", `{"text":"generated diagnostic output"}`))
	endLLM()
	agent.SetAttributes(attribute.String("neatlogs.output.value", `{"text":"generated diagnostic output"}`))
	endAgent()
	_, tool, endTool := neatlogs.StartSpan(rootCtx, "doctor.probe.tool", "tool", append(doctorSpanAttributes("TOOL"), attribute.String("neatlogs.input.value", `{"value":1}`), attribute.String("neatlogs.output.value", `{"value":2}`))...)
	_ = tool
	endTool()
	_ = neatlogs.SetTraceOutput(root, map[string]any{"result": map[string]int{"value": 2}})
	root.SetAttributes(attribute.String("neatlogs.output.value", `{"result":{"value":2}}`))
	endRoot()
	flushStart := time.Now()
	flushCtx, cancel := context.WithTimeout(clientCtx, 5*time.Second)
	flushErr := client.Flush(flushCtx)
	cancel()
	duration := time.Since(flushStart)
	local := neatlogs.DoctorCapturedLocalV2(clientCtx, neatlogs.DoctorLocalOptions{TraceID: traceID, RootSampleRate: 1, FlushOutcome: map[bool]string{true: "success", false: "failed"}[flushErr == nil], FlushTimeout: 5 * time.Second, FlushDuration: duration})
	if mode == "probe" {
		endpoint := cfg.Endpoint
		if endpoint == "" {
			endpoint = "https://ingest.neatlogs.com"
		}
		local = neatlogs.DoctorProbeV2(ctx, local, neatlogs.DoctorProbeOptions{Endpoint: endpoint, APIKey: cfg.APIKey, Timeout: 45 * time.Second})
	}
	_ = client.Shutdown(context.Background())
	exit := 0
	if local.Status == neatlogs.DoctorWarn {
		exit = 1
	}
	if local.Status == neatlogs.DoctorFail {
		if mode == "probe" {
			exit = 3
		} else {
			exit = 2
		}
	}
	return output(local, jsonOutput, exit)
}

func validDoctorEndpoint(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	return err == nil &&
		(parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.Host != "" &&
		parsed.User == nil
}

func doctorSpanAttributes(spanType string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.Bool("neatlogs.doctor", true),
		attribute.String("neatlogs.doctor.version", "v1"),
		attribute.String("service.name", "neatlogs.doctor.v2"),
		attribute.String("telemetry.sdk.language", "go"),
		attribute.String("telemetry.sdk.version", neatlogs.Version),
		attribute.String("neatlogs.span.type", spanType),
	}
}

func doctorPreflightFailure(code, message, remediation string) neatlogs.DoctorV2Result {
	return neatlogs.DoctorV2Result{
		FormatVersion: neatlogs.DoctorV2FormatVersion,
		Mode:          "probe",
		Status:        neatlogs.DoctorFail,
		FirstFailure:  &code,
		Runtime: neatlogs.DoctorV2Runtime{
			Language: "go", SDKVersion: neatlogs.Version, SchemaVersion: "2", Transport: "otlp_http_protobuf",
		},
		Checks: []neatlogs.DoctorV2Check{{
			Name: "configuration", Status: neatlogs.DoctorFail, ReasonCode: code, Message: message, RemediationCode: remediation,
		}},
	}
}

func output(result neatlogs.DoctorV2Result, jsonOutput bool, exit int) int {
	if jsonOutput {
		encoded, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(encoded))
	} else {
		fmt.Printf("Neatlogs Doctor: %s\n", result.Status)
		for _, check := range result.Checks {
			fmt.Printf("%s %s: %s\n", check.Status, check.ReasonCode, check.Message)
		}
	}
	return exit
}
func usage() int {
	fmt.Fprintln(os.Stderr, "Usage: neatlogs doctor (--local | --probe) [--json]")
	return 4
}
