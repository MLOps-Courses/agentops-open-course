package telemetry_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	adktelemetry "google.golang.org/adk/v2/telemetry"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
)

func testBuildInfo() buildinfo.Info {
	revision := strings.Repeat("a", 40)
	return buildinfo.Info{
		Timestamp:      time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC),
		Mode:           buildinfo.Release,
		Version:        "v9.9.9",
		SourceIdentity: revision,
		Revision:       revision,
		TreeDigest:     "sha256:" + strings.Repeat("b", 64),
	}
}

// clearOTLPEnvironment removes every variable the gates read, so a test starts
// from "nothing configured" whatever the developer's shell exports.
//
// t.Setenv restores the previous value at the end of the test, and setting a
// variable to the empty string is what the gates treat as unset, so this is
// both isolated and reversible. It forbids t.Parallel in every test that uses
// it, which is why nothing in this file is parallel.
func clearOTLPEnvironment(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		telemetry.EnvSDKDisabled,
		telemetry.EnvOTLPEndpoint,
		telemetry.EnvOTLPTracesEndpoint,
		telemetry.EnvOTLPMetricsEndpoint,
		telemetry.EnvOTLPLogsEndpoint,
		telemetry.EnvADKCaptureMessageContent,
		telemetry.EnvGenAICaptureMessageContent,
		telemetry.EnvTracesSampler,
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s: %v", name, err)
		}
	}
}

// TestADKTraceRiskGateControlsThePinnedProvider proves the environment guard
// at the public ADK provider seam, rather than only checking an environment
// string. The default path must create non-recording spans even when a caller
// requested always-on sampling; the explicit risk-acceptance path may record.
func TestADKTraceRiskGateControlsThePinnedProvider(t *testing.T) {
	for name, testCase := range map[string]struct {
		capture       string
		wantRecording bool
	}{
		"default rejects trace recording": {
			capture: telemetry.ContentCaptureDisabled,
		},
		"explicit risk acceptance permits trace recording": {
			capture:       telemetry.ContentCaptureEnabled,
			wantRecording: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearOTLPEnvironment(t)
			t.Setenv(telemetry.EnvADKCaptureMessageContent, testCase.capture)
			t.Setenv(telemetry.EnvTracesSampler, "always_on")
			if err := telemetry.SetContentCaptureDefaults(); err != nil {
				t.Fatalf("SetContentCaptureDefaults() error = %v, want nil", err)
			}

			recorder := tracetest.NewSpanRecorder()
			providers, err := adktelemetry.New(
				t.Context(), adktelemetry.WithSpanProcessors(recorder),
			)
			if err != nil {
				t.Fatalf("adktelemetry.New() error = %v, want nil", err)
			}
			t.Cleanup(func() {
				if shutdownErr := providers.Shutdown(context.Background()); shutdownErr != nil {
					t.Errorf("providers.Shutdown() error = %v, want nil", shutdownErr)
				}
			})
			if providers.TracerProvider == nil {
				t.Fatal("ADK tracer provider = nil, want the configured provider")
			}

			_, span := providers.TracerProvider.Tracer("gcp.vertex.agent").Start(t.Context(), "execute_tool synthetic")
			if got := span.IsRecording(); got != testCase.wantRecording {
				t.Errorf("span.IsRecording() = %v, want %v", got, testCase.wantRecording)
			}
			span.End()
			wantEnded := 0
			if testCase.wantRecording {
				wantEnded = 1
			}
			if got := len(recorder.Ended()); got != wantEnded {
				t.Errorf("ended spans = %d, want %d", got, wantEnded)
			}
		})
	}
}

// TestContentCaptureDefaultsToFalse is the course invariant: telemetry content
// stays private unless an operator says otherwise. It is the Go half of
// test_setup_telemetry_is_a_safe_noop_without_an_endpoint.
func TestContentCaptureDefaultsToFalse(t *testing.T) {
	clearOTLPEnvironment(t)

	if err := telemetry.SetContentCaptureDefaults(); err != nil {
		t.Fatalf("SetContentCaptureDefaults() error = %v, want nil", err)
	}
	for _, name := range []string{
		telemetry.EnvADKCaptureMessageContent,
		telemetry.EnvGenAICaptureMessageContent,
	} {
		if got := os.Getenv(name); got != telemetry.ContentCaptureDisabled {
			t.Errorf("%s = %q, want %q", name, got, telemetry.ContentCaptureDisabled)
		}
	}
}

// TestContentCaptureDefaultsDoNotOverrideAnExplicitChoice keeps the switch
// usable: Chapter 7.1 asks a learner to turn capture on for one session, and a
// default that overwrote their value would make that impossible to explain.
func TestContentCaptureDefaultsDoNotOverrideAnExplicitChoice(t *testing.T) {
	clearOTLPEnvironment(t)
	t.Setenv(telemetry.EnvGenAICaptureMessageContent, "true")

	if err := telemetry.SetContentCaptureDefaults(); err != nil {
		t.Fatalf("SetContentCaptureDefaults() error = %v, want nil", err)
	}
	if got := os.Getenv(telemetry.EnvGenAICaptureMessageContent); got != "true" {
		t.Errorf("%s = %q, want the operator's %q", telemetry.EnvGenAICaptureMessageContent, got, "true")
	}
	// The variable the operator did not choose still gets the private default.
	if got := os.Getenv(telemetry.EnvADKCaptureMessageContent); got != telemetry.ContentCaptureDisabled {
		t.Errorf("%s = %q, want %q", telemetry.EnvADKCaptureMessageContent, got, telemetry.ContentCaptureDisabled)
	}
}

// TestMalformedContentCaptureClosesTheBoundaryAndReportsIt is the fail-closed
// half of the risk gate. A typo in the acceptance switch must not read as
// acceptance — and it must not read as a clean default either, because a value
// nobody meant to type is a configuration bug an operator has to be told about
// while the boundary is already shut.
func TestMalformedContentCaptureClosesTheBoundaryAndReportsIt(t *testing.T) {
	clearOTLPEnvironment(t)
	t.Setenv(telemetry.EnvADKCaptureMessageContent, "yes please")
	t.Setenv(telemetry.EnvTracesSampler, "always_on")

	err := telemetry.SetContentCaptureDefaults()
	if err == nil {
		t.Fatal("SetContentCaptureDefaults() error = nil, want the malformed value reported")
	}
	if !strings.Contains(err.Error(), telemetry.EnvADKCaptureMessageContent) {
		t.Errorf("error = %v, want it to name %s", err, telemetry.EnvADKCaptureMessageContent)
	}
	if got := os.Getenv(telemetry.EnvADKCaptureMessageContent); got != telemetry.ContentCaptureDisabled {
		t.Errorf("%s = %q, want the boundary closed with %q",
			telemetry.EnvADKCaptureMessageContent, got, telemetry.ContentCaptureDisabled)
	}
	// The operator's sampler choice is overridden, not merged: unsafe ADK spans
	// carry tool payloads, so an unaccepted risk means no recording at all.
	if got := os.Getenv(telemetry.EnvTracesSampler); got != telemetry.TraceSamplerDisabled {
		t.Errorf("%s = %q, want %q", telemetry.EnvTracesSampler, got, telemetry.TraceSamplerDisabled)
	}
}

// TestExportGatesFollowTheStandardEnvironment ports three Python tests at once:
// the no-endpoint no-op, the OTEL_SDK_DISABLED kill switch, and — the case that
// is easy to get wrong — a traces-only endpoint that must not turn the log path
// on.
func TestExportGatesFollowTheStandardEnvironment(t *testing.T) {
	const endpoint = "http://collector:4318"

	cases := []struct {
		environment map[string]string
		name        string
		export      bool
		logs        bool
		metrics     bool
	}{
		{
			name:        "nothing configured",
			environment: map[string]string{},
		},
		{
			name:        "combined endpoint feeds every signal",
			environment: map[string]string{telemetry.EnvOTLPEndpoint: endpoint},
			export:      true,
			logs:        true,
			metrics:     true,
		},
		{
			name:        "traces-only endpoint exports but installs no log path",
			environment: map[string]string{telemetry.EnvOTLPTracesEndpoint: endpoint + "/v1/traces"},
			export:      true,
		},
		{
			name:        "logs-only endpoint installs the log path alone",
			environment: map[string]string{telemetry.EnvOTLPLogsEndpoint: endpoint + "/v1/logs"},
			export:      true,
			logs:        true,
		},
		{
			name:        "metrics-only endpoint installs the meter alone",
			environment: map[string]string{telemetry.EnvOTLPMetricsEndpoint: endpoint + "/v1/metrics"},
			export:      true,
			metrics:     true,
		},
		{
			name: "the kill switch outranks every endpoint",
			environment: map[string]string{
				telemetry.EnvOTLPEndpoint:     endpoint,
				telemetry.EnvSDKDisabled:      "true",
				telemetry.EnvOTLPLogsEndpoint: endpoint + "/v1/logs",
			},
		},
		{
			name: "the kill switch accepts the values a hand-edited .env carries",
			environment: map[string]string{
				telemetry.EnvOTLPEndpoint: endpoint,
				telemetry.EnvSDKDisabled:  " YES ",
			},
		},
		{
			name: "an unrecognized kill-switch value leaves export on",
			environment: map[string]string{
				telemetry.EnvOTLPEndpoint: endpoint,
				telemetry.EnvSDKDisabled:  "maybe",
			},
			export:  true,
			logs:    true,
			metrics: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clearOTLPEnvironment(t)
			for name, value := range testCase.environment {
				t.Setenv(name, value)
			}

			if got := telemetry.ExportConfigured(); got != testCase.export {
				t.Errorf("ExportConfigured() = %v, want %v", got, testCase.export)
			}
			if got := telemetry.LogsConfigured(); got != testCase.logs {
				t.Errorf("LogsConfigured() = %v, want %v", got, testCase.logs)
			}
			if got := telemetry.MetricsConfigured(); got != testCase.metrics {
				t.Errorf("MetricsConfigured() = %v, want %v", got, testCase.metrics)
			}
		})
	}
}

// TestResourcePinsTheServiceIdentity holds the two attributes every dashboard
// selector and every Tempo-to-Loki link is written against.
func TestResourcePinsTheServiceIdentity(t *testing.T) {
	t.Parallel()

	build := testBuildInfo()
	res := telemetry.Resource(build)

	// An empty schema URL is what lets ADK merge this into its own resource:
	// resource.Merge fails outright on two different non-empty schema URLs, and
	// ADK's default resource carries whichever one its SDK release shipped.
	if url := res.SchemaURL(); url != "" {
		t.Errorf("SchemaURL() = %q, want empty so ADK's resource.Merge cannot fail", url)
	}
	attributes := map[string]string{}
	for _, attribute := range res.Attributes() {
		attributes[string(attribute.Key)] = attribute.Value.String()
	}
	if got := attributes["service.name"]; got != telemetry.ServiceName {
		t.Errorf("service.name = %q, want %q", got, telemetry.ServiceName)
	}
	if got := attributes["service.version"]; got != build.Version {
		t.Errorf("service.version = %q, want %q", got, build.Version)
	}
	for key, want := range map[string]string{
		"agentops.build.mode":         string(build.Mode),
		"agentops.build.timestamp":    build.Timestamp.Format(time.RFC3339),
		"agentops.source.identity":    build.SourceIdentity,
		"agentops.source.revision":    build.Revision,
		"agentops.source.tree_digest": build.TreeDigest,
		"agentops.source.dirty":       "false",
	} {
		if got := attributes[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
