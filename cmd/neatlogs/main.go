package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	var client *neatlogs.Client
	var err error
	if mode == "local" {
		client, err = neatlogs.NewClient(ctx, cfg, neatlogs.WithExporter(tracetest.NewInMemoryExporter()))
	} else {
		client, err = neatlogs.NewClient(ctx, cfg)
	}
	if err != nil {
		return output(neatlogs.DoctorV2Result{FormatVersion: neatlogs.DoctorV2FormatVersion, Mode: mode, Status: neatlogs.DoctorFail, Runtime: neatlogs.DoctorV2Runtime{Language: "go", SDKVersion: neatlogs.Version, SchemaVersion: "2", Transport: "otlp_http_protobuf"}, Checks: []neatlogs.DoctorV2Check{{Name: "configuration", Status: neatlogs.DoctorFail, ReasonCode: "INSTRUMENTOR_INACTIVE", Message: "Doctor could not initialize an isolated runtime", RemediationCode: "ENABLE_INSTRUMENTOR"}}}, jsonOutput, 2)
	}
	clientCtx := client.Context(ctx)
	rootCtx, root, endRoot := neatlogs.Trace(clientCtx, "doctor.workflow")
	traceID := root.SpanContext().TraceID().String()
	_, tool, endTool := neatlogs.StartSpan(rootCtx, "doctor.tool", "tool", attribute.String("neatlogs.tool.name", "diagnostic_tool"), attribute.String("neatlogs.tool.input", `{"value":1}`), attribute.String("neatlogs.tool.output", `{"value":2}`))
	_ = tool
	endTool()
	_ = neatlogs.SetTraceOutput(rootCtx, map[string]any{"result": "generated diagnostic output"})
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
		local = neatlogs.DoctorProbeV2(ctx, local, neatlogs.DoctorProbeOptions{Endpoint: endpoint, APIKey: cfg.APIKey, Timeout: 10 * time.Second})
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
