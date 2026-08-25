package neatlogs

import (
	"context"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	telemetryv2 "github.com/neatlogs/neatlogs-go/contracts/v2"
)

const DoctorFormatVersion = "neatlogs.doctor/v1"

type DoctorStatus string
type DoctorReasonCode string

const (
	DoctorPass    DoctorStatus = "pass"
	DoctorWarn    DoctorStatus = "warn"
	DoctorFail    DoctorStatus = "fail"
	DoctorUnknown DoctorStatus = "unknown"
)

const (
	ReasonGoRuntimeVersionUnknown   DoctorReasonCode = "GO_RUNTIME_VERSION_UNKNOWN"
	ReasonGoRuntimeUnsupported      DoctorReasonCode = "GO_RUNTIME_UNSUPPORTED"
	ReasonGoRuntimeSupported        DoctorReasonCode = "GO_RUNTIME_SUPPORTED"
	ReasonModuleMetadataUnavailable DoctorReasonCode = "MODULE_METADATA_UNAVAILABLE"
	ReasonModuleIdentityUnavailable DoctorReasonCode = "MODULE_IDENTITY_UNAVAILABLE"
	ReasonModuleMetadataPresent     DoctorReasonCode = "MODULE_METADATA_PRESENT"
	ReasonSchemaV2Invalid           DoctorReasonCode = "SCHEMA_V2_INVALID"
	ReasonSchemaV2HashValid         DoctorReasonCode = "SCHEMA_V2_HASH_VALID"
	ReasonTransportOTLPHTTPProtobuf DoctorReasonCode = "TRANSPORT_OTLP_HTTP_PROTOBUF"
	ReasonEndpointInvalid           DoctorReasonCode = "ENDPOINT_INVALID"
	ReasonEndpointPathUnsupported   DoctorReasonCode = "ENDPOINT_PATH_UNSUPPORTED"
	ReasonEndpointValid             DoctorReasonCode = "ENDPOINT_VALID"
	ReasonSamplerInvalid            DoctorReasonCode = "SAMPLER_INVALID"
	ReasonSamplerParentBasedValid   DoctorReasonCode = "SAMPLER_PARENT_BASED_VALID"
	ReasonOTelProviderPrivate       DoctorReasonCode = "OTEL_PROVIDER_PRIVATE"
	ReasonExportQueueDisabled       DoctorReasonCode = "EXPORT_QUEUE_DISABLED"
	ReasonExportQueueBatched        DoctorReasonCode = "EXPORT_QUEUE_BATCHED"
	ReasonExportHealthUnobservable  DoctorReasonCode = "EXPORT_HEALTH_UNOBSERVABLE"
	ReasonExportHealthUnhealthy     DoctorReasonCode = "EXPORT_HEALTH_UNHEALTHY"
	ReasonExportHealthy             DoctorReasonCode = "EXPORT_HEALTHY"
	ReasonRootUnobservable          DoctorReasonCode = "ROOT_UNOBSERVABLE"
	ReasonRootNotActive             DoctorReasonCode = "ROOT_NOT_ACTIVE"
	ReasonRootOwnershipInvalid      DoctorReasonCode = "ROOT_OWNERSHIP_INVALID"
	ReasonRootIDsValid              DoctorReasonCode = "ROOT_IDS_VALID"
)

// DoctorCheck is one stable, machine-readable local diagnostic. ReasonCode is
// intended for automation; Message is bounded human guidance and never
// contains credentials or exporter/callback error text.
type DoctorCheck struct {
	Name       string            `json:"name"`
	Status     DoctorStatus      `json:"status"`
	ReasonCode DoctorReasonCode  `json:"reason_code"`
	Message    string            `json:"message"`
	Details    map[string]string `json:"details,omitempty"`
}

// DoctorResult is the versioned, read-only local readiness result.
type DoctorResult struct {
	FormatVersion string        `json:"format_version"`
	SDKVersion    string        `json:"sdk_version"`
	Ready         bool          `json:"ready"`
	Checks        []DoctorCheck `json:"checks"`
}

// Doctor performs local validation only. It does not initialize a provider,
// call a provider/backend, send telemetry, flush, shut down, or mutate config.
// Runtime/root/export-health checks are included only when ctx selects an
// already-running private Neatlogs runtime.
func Doctor(ctx context.Context, cfg Config) DoctorResult {
	result := DoctorResult{FormatVersion: DoctorFormatVersion, SDKVersion: Version, Ready: true}
	result.Checks = append(result.Checks,
		doctorRuntimeCheck(),
		doctorModuleCheck(),
		doctorSchemaCheck(),
		doctorTransportCheck(),
		doctorEndpointCheck(cfg),
		doctorSamplerCheck(cfg),
		doctorOwnershipCheck(),
		doctorQueueCheck(cfg),
	)
	runtime := runtimeForContext(ctx)
	result.Checks = append(result.Checks, doctorExportHealthCheck(runtime), doctorRootCheck(ctx, runtime))
	for _, check := range result.Checks {
		if check.Status == DoctorFail {
			result.Ready = false
		}
	}
	return result
}

func doctorRuntimeCheck() DoctorCheck {
	version := runtime.Version()
	major, minor, ok := parseGoVersion(version)
	if !ok {
		return doctorCheck("runtime", DoctorWarn, ReasonGoRuntimeVersionUnknown, "Go runtime version could not be parsed", map[string]string{"version": version})
	}
	if major < 1 || major == 1 && minor < 25 {
		return doctorCheck("runtime", DoctorFail, ReasonGoRuntimeUnsupported, "Go 1.25 or newer is required", map[string]string{"version": version})
	}
	return doctorCheck("runtime", DoctorPass, ReasonGoRuntimeSupported, "Go runtime is supported", map[string]string{"version": version})
}

func parseGoVersion(value string) (int, int, bool) {
	value = strings.TrimPrefix(value, "devel ")
	value = strings.TrimPrefix(value, "go")
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minorText := parts[1]
	if index := strings.IndexFunc(minorText, func(r rune) bool { return r < '0' || r > '9' }); index >= 0 {
		minorText = minorText[:index]
	}
	minor, errMinor := strconv.Atoi(minorText)
	return major, minor, errMajor == nil && errMinor == nil
}

func doctorModuleCheck() DoctorCheck {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return doctorCheck("module", DoctorWarn, ReasonModuleMetadataUnavailable, "Go build metadata is unavailable", nil)
	}
	version := info.Main.Version
	found := info.Main.Path == "github.com/neatlogs/neatlogs-go"
	if !found {
		for _, dependency := range info.Deps {
			if dependency.Path == "github.com/neatlogs/neatlogs-go" {
				found = true
				version = dependency.Version
				if dependency.Replace != nil {
					version = dependency.Replace.Version
				}
				break
			}
		}
	}
	if !found {
		return doctorCheck("module", DoctorWarn, ReasonModuleIdentityUnavailable, "Neatlogs module identity is unavailable in build metadata", nil)
	}
	if version == "" {
		version = "unknown"
	}
	return doctorCheck("module", DoctorPass, ReasonModuleMetadataPresent, "Neatlogs module metadata is present", map[string]string{"module": "github.com/neatlogs/neatlogs-go", "version": version})
}

func doctorSchemaCheck() DoctorCheck {
	manifest, manifestErr := telemetryv2.LoadManifest()
	if err := telemetryv2.Validate(); err != nil || manifestErr != nil {
		return doctorCheck("schema", DoctorFail, ReasonSchemaV2Invalid, "Embedded telemetry schema or golden fixtures failed validation", nil)
	}
	return doctorCheck("schema", DoctorPass, ReasonSchemaV2HashValid, "Embedded telemetry schema v2 hash and fixtures are valid", map[string]string{
		"contract_version": manifest.ContractVersion, "schema_sha256": manifest.SchemaSHA256,
	})
}

func doctorTransportCheck() DoctorCheck {
	return doctorCheck("transport", DoctorPass, ReasonTransportOTLPHTTPProtobuf, "SDK transport is OTLP HTTP/protobuf", map[string]string{"path": "/v1/traces"})
}

func doctorEndpointCheck(cfg Config) DoctorCheck {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("NEATLOGS_ENDPOINT"))
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return doctorCheck("endpoint", DoctorFail, ReasonEndpointInvalid, "Endpoint must be an HTTP(S) origin without credentials, query, or fragment", nil)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return doctorCheck("endpoint", DoctorFail, ReasonEndpointPathUnsupported, "Endpoint must be an origin; the SDK appends /v1/traces", map[string]string{"scheme": parsed.Scheme, "host": parsed.Host})
	}
	return doctorCheck("endpoint", DoctorPass, ReasonEndpointValid, "Endpoint origin is valid", map[string]string{"scheme": parsed.Scheme, "host": parsed.Host})
}

func doctorSamplerCheck(cfg Config) DoctorCheck {
	if _, err := samplerFromConfig(cfg); err != nil {
		return doctorCheck("sampler", DoctorFail, ReasonSamplerInvalid, "Sample rate must be finite and between 0 and 1", nil)
	}
	rate := 1.0
	if cfg.SampleRate != nil {
		rate = *cfg.SampleRate
	}
	return doctorCheck("sampler", DoctorPass, ReasonSamplerParentBasedValid, "ParentBased trace sampling is valid", map[string]string{"root_sample_rate": strconv.FormatFloat(rate, 'g', -1, 64)})
}

func doctorOwnershipCheck() DoctorCheck {
	return doctorCheck("ownership", DoctorPass, ReasonOTelProviderPrivate, "Neatlogs owns a private provider and leaves global OpenTelemetry state untouched", nil)
}

func doctorQueueCheck(cfg Config) DoctorCheck {
	if cfg.DisableExport {
		return doctorCheck("queue", DoctorWarn, ReasonExportQueueDisabled, "Export is disabled, so no batch queue will be created", nil)
	}
	return doctorCheck("queue", DoctorPass, ReasonExportQueueBatched, "Export uses the OpenTelemetry batch span processor", nil)
}

func doctorExportHealthCheck(runtime *sdkRuntime) DoctorCheck {
	if runtime == nil {
		return doctorCheck("export_health", DoctorUnknown, ReasonExportHealthUnobservable, "No running Neatlogs runtime is selected", nil)
	}
	health := runtime.exportHealth()
	details := map[string]string{
		"dropped_spans":   strconv.FormatUint(health.DroppedSpans, 10),
		"mask_failures":   strconv.FormatUint(health.MaskFailures, 10),
		"export_failures": strconv.FormatUint(health.ExportFailures, 10),
	}
	if health.Err() != nil {
		return doctorCheck("export_health", DoctorFail, ReasonExportHealthUnhealthy, "The selected runtime has masking drops or exporter failures", details)
	}
	return doctorCheck("export_health", DoctorPass, ReasonExportHealthy, "The selected runtime has no observed export failures or drops", details)
}

func doctorRootCheck(ctx context.Context, runtime *sdkRuntime) DoctorCheck {
	if runtime == nil {
		return doctorCheck("root", DoctorUnknown, ReasonRootUnobservable, "No running Neatlogs runtime is selected", nil)
	}
	current, owned := privateOwnedSpanContextFor(ctx, runtime)
	if !owned {
		return doctorCheck("root", DoctorUnknown, ReasonRootNotActive, "Context does not carry an active root owned by the selected runtime", nil)
	}
	root, ok := runtime.lifecycle.rootSpanContext(current)
	if !ok {
		return doctorCheck("root", DoctorFail, ReasonRootOwnershipInvalid, "Owned context does not resolve to one active root", nil)
	}
	return doctorCheck("root", DoctorPass, ReasonRootIDsValid, "Active owned root has valid trace and span IDs", map[string]string{
		"trace_id": root.TraceID().String(), "span_id": root.SpanID().String(),
	})
}

func runtimeForContext(ctx context.Context) *sdkRuntime {
	if client, ok := ClientFromContext(ctx); ok {
		return client.runtime
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.state != stateRunning {
		return nil
	}
	return global.runtime
}

func doctorCheck(name string, status DoctorStatus, code DoctorReasonCode, message string, details map[string]string) DoctorCheck {
	return DoctorCheck{Name: name, Status: status, ReasonCode: code, Message: message, Details: details}
}
